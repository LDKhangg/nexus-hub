// Package metrics is the hub's small metrics registry (Phase 12 REQ-8).
// Domain code sees only the Counter and Observer interfaces and the Hub's
// methods; the Prometheus client stays inside this package. Every label is
// a closed set: values outside the set given at construction become
// "other", so no user text, id or name can become a label.
package metrics

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Error kinds of nexushub_errors_total.
const (
	KindStepRetry       = "step_retry"
	KindStepFailure     = "step_failure"
	KindWebhookReject   = "webhook_reject"
	KindDeliveryFailure = "delivery_failure"
	KindDeliveryRetry   = "delivery_retry"
	KindLLMBudget       = "llm_budget_refused"
)

// Webhook sources of the latency histogram.
const (
	SourceTelegram = "telegram"
	SourceClickUp  = "clickup"
	SourceOther    = "other"
)

// other replaces a label value outside its closed set.
const other = "other"

var codeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Snapshot is the database view read on each scrape.
type Snapshot struct {
	ExecutionsByState  map[string]int64
	QueuePending       int64
	OldestDueAge       time.Duration
	WorkerDevices      int64
	WorkersOnline      int64
	NewestHeartbeatAge time.Duration // < 0: no heartbeat yet
	LLM                []LLMDay
}

// LLMDay is one provider and model's usage of the current UTC day.
type LLMDay struct {
	Provider, Model           string
	InputTokens, OutputTokens int64
	CostUSD                   float64
}

// Snapshotter reads a Snapshot (storage.OpsStore through the wiring).
type Snapshotter func(ctx context.Context) (Snapshot, error)

// Options are the closed label sets and the scrape source.
type Options struct {
	// States are the workflow execution states (always all reported).
	States []string
	// Codes are the known error and reason codes; any other code is
	// reported as "other".
	Codes []string
	// Providers and Models bound the LLM labels.
	Providers, Models []string
	// Snapshot is read on every scrape, bounded by ScrapeTimeout.
	Snapshot      Snapshotter
	ScrapeTimeout time.Duration
}

// Hub holds the hub's metrics.
type Hub struct {
	reg       *prometheus.Registry
	errors    *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	codes     map[string]bool
	providers map[string]bool
	models    map[string]bool
}

func set(values []string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// New builds the registry. A nil Snapshot reports only the counters and
// the histogram.
func New(o Options) *Hub {
	h := &Hub{
		reg:       prometheus.NewRegistry(),
		codes:     set(o.Codes),
		providers: set(o.Providers),
		models:    set(o.Models),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nexushub_errors_total",
			Help: "Errors by kind and safe code: workflow step retries and failures, webhook rejects, delivery failures, LLM budget refusals.",
		}, []string{"kind", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nexushub_webhook_duration_seconds",
			Help:    "Webhook handler latency by source.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"source"}),
	}
	h.reg.MustRegister(h.errors, h.latency)
	if o.Snapshot != nil {
		timeout := o.ScrapeTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		h.reg.MustRegister(&collector{h: h, snapshot: o.Snapshot, timeout: timeout, states: append([]string(nil), o.States...)})
	}
	return h
}

// Handler serves the text exposition format.
func (h *Hub) Handler() http.Handler {
	return promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

func (h *Hub) code(c string) string {
	if h.codes[c] && codeShape.MatchString(c) {
		return c
	}
	return other
}

// Error counts one error of kind with a safe code. A nil Hub counts
// nothing.
func (h *Hub) Error(kind, code string) {
	if h == nil {
		return
	}
	switch kind {
	case KindStepRetry, KindStepFailure, KindWebhookReject, KindDeliveryFailure, KindDeliveryRetry, KindLLMBudget:
	default:
		kind = other
	}
	h.errors.WithLabelValues(kind, h.code(code)).Inc()
}

// WebhookLatency records one webhook handler duration.
func (h *Hub) WebhookLatency(source string, d time.Duration) {
	if h == nil {
		return
	}
	switch source {
	case SourceTelegram, SourceClickUp:
	default:
		source = SourceOther
	}
	h.latency.WithLabelValues(source).Observe(d.Seconds())
}

// HTTPStatusCode is the closed code of a rejected webhook status.
func HTTPStatusCode(status int) string {
	switch status {
	case 400, 401, 403, 404, 405, 409, 413, 415, 429:
		return "http_" + strconv.Itoa(status)
	}
	if status >= 500 {
		return "http_5xx"
	}
	return "http_4xx"
}

// collector reads the database view on each scrape.
type collector struct {
	h        *Hub
	snapshot Snapshotter
	timeout  time.Duration
	states   []string
}

var (
	descExecutions = prometheus.NewDesc("nexushub_executions", "Workflow executions by state.", []string{"state"}, nil)
	descQueue      = prometheus.NewDesc("nexushub_queue_pending_jobs", "Workflow jobs waiting for a claim.", nil, nil)
	descOldest     = prometheus.NewDesc("nexushub_queue_oldest_due_age_seconds", "Wait of the oldest due pending job (0: none).", nil, nil)
	descDevices    = prometheus.NewDesc("nexushub_worker_devices", "Enrolled worker devices (not revoked).", nil, nil)
	descOnline     = prometheus.NewDesc("nexushub_worker_devices_online", "Worker devices online or busy.", nil, nil)
	descHeartbeat  = prometheus.NewDesc("nexushub_worker_newest_heartbeat_age_seconds", "Age of the newest worker heartbeat (-1: none yet).", nil, nil)
	descTokens     = prometheus.NewDesc("nexushub_llm_tokens_today", "LLM tokens of the current UTC day by provider, model and direction.", []string{"provider", "model", "direction"}, nil)
	descCost       = prometheus.NewDesc("nexushub_llm_cost_usd_today", "Estimated LLM cost in USD of the current UTC day by provider and model.", []string{"provider", "model"}, nil)
	descScrapeErr  = prometheus.NewDesc("nexushub_ops_scrape_error", "1 when the database view could not be read on this scrape.", nil, nil)
)

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descExecutions, descQueue, descOldest, descDevices, descOnline, descHeartbeat, descTokens, descCost, descScrapeErr} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	s, err := c.snapshot(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(descScrapeErr, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(descScrapeErr, prometheus.GaugeValue, 0)
	known := set(c.states)
	var otherStates int64
	for state, n := range s.ExecutionsByState {
		if !known[state] {
			otherStates += n
		}
	}
	for _, state := range c.states {
		ch <- prometheus.MustNewConstMetric(descExecutions, prometheus.GaugeValue, float64(s.ExecutionsByState[state]), state)
	}
	if otherStates > 0 {
		ch <- prometheus.MustNewConstMetric(descExecutions, prometheus.GaugeValue, float64(otherStates), other)
	}
	ch <- prometheus.MustNewConstMetric(descQueue, prometheus.GaugeValue, float64(s.QueuePending))
	ch <- prometheus.MustNewConstMetric(descOldest, prometheus.GaugeValue, s.OldestDueAge.Seconds())
	ch <- prometheus.MustNewConstMetric(descDevices, prometheus.GaugeValue, float64(s.WorkerDevices))
	ch <- prometheus.MustNewConstMetric(descOnline, prometheus.GaugeValue, float64(s.WorkersOnline))
	hb := -1.0
	if s.NewestHeartbeatAge >= 0 {
		hb = s.NewestHeartbeatAge.Seconds()
	}
	ch <- prometheus.MustNewConstMetric(descHeartbeat, prometheus.GaugeValue, hb)
	// Rows outside the closed provider and model sets are summed under
	// "other", so every label pair is reported once.
	type key struct{ provider, model string }
	type sum struct {
		in, out int64
		cost    float64
	}
	sums := map[key]*sum{}
	var order []key
	for _, r := range s.LLM {
		k := key{provider: other, model: other}
		if c.h.providers[r.Provider] {
			k.provider = r.Provider
		}
		if c.h.models[r.Model] {
			k.model = r.Model
		}
		v, ok := sums[k]
		if !ok {
			v = &sum{}
			sums[k] = v
			order = append(order, k)
		}
		v.in += r.InputTokens
		v.out += r.OutputTokens
		v.cost += r.CostUSD
	}
	for _, k := range order {
		v := sums[k]
		ch <- prometheus.MustNewConstMetric(descTokens, prometheus.GaugeValue, float64(v.in), k.provider, k.model, "input")
		ch <- prometheus.MustNewConstMetric(descTokens, prometheus.GaugeValue, float64(v.out), k.provider, k.model, "output")
		ch <- prometheus.MustNewConstMetric(descCost, prometheus.GaugeValue, v.cost, k.provider, k.model)
	}
}
