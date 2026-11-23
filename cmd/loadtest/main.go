// Command loadtest runs the Phase 12 local webhook-to-workflow load check.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

const (
	webhookID    = "phase12-loadtest"
	secretRef    = "loadtest-webhook"
	behaviorName = "phase12-loadtest"
)

type options struct {
	databaseURL string
	n           int
	concurrency int
	duplicates  float64
	out         string
	timeout     time.Duration
}

type latencyReport struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

type report struct {
	N               int           `json:"n"`
	Duplicates      int           `json:"duplicates"`
	Concurrency     int           `json:"concurrency"`
	ClientLatency   latencyReport `json:"client_latency"`
	ServerLatency   latencyReport `json:"server_latency"`
	Completed       int           `json:"completed"`
	Failed          int           `json:"failed"`
	Stuck           int           `json:"stuck"`
	HTTPFailures    int           `json:"http_failures"`
	ExecutionCount  int           `json:"execution_count"`
	TotalWallTimeMS float64       `json:"total_wall_time_ms"`
	GoVersion       string        `json:"go_version"`
	PostgresVersion string        `json:"postgres_version"`
	CPUCount        int           `json:"cpu_count"`
	Passed          bool          `json:"passed"`
	Failures        []string      `json:"failures,omitempty"`
	GeneratedAt     time.Time     `json:"generated_at"`
}

type memoryResolver struct {
	workspaceID string
	secret      string
}

func (r memoryResolver) Resolve(_ context.Context, workspaceID, ref string) (string, error) {
	if workspaceID != r.workspaceID || ref != secretRef {
		return "", errors.New("loadtest: webhook secret unavailable")
	}
	return r.secret, nil
}

type noOpStep struct{}

func (noOpStep) Execute(context.Context, workflow.StepContext) (workflow.StepResult, error) {
	return workflow.StepResult{Checkpoint: &workflow.Checkpoint{Reusable: true}}, nil
}

type durationCollector struct {
	mu     sync.Mutex
	values []time.Duration
}

func (c *durationCollector) add(value time.Duration) {
	c.mu.Lock()
	c.values = append(c.values, value)
	c.mu.Unlock()
}

func (c *durationCollector) snapshot() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.values...)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run() error {
	opts, err := parseOptions()
	if err != nil {
		return err
	}
	poolConfig, err := parseValidatedDatabaseURL(opts.databaseURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if err := migrate(ctx, poolConfig.ConnConfig.ConnString()); err != nil {
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return errors.New("database connection configuration failed")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.New("database connection failed")
	}
	var postgresVersion string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&postgresVersion); err != nil {
		return errors.New("postgres version query failed")
	}

	workspaceID, err := newID()
	if err != nil {
		return err
	}
	workspaces, err := storage.NewWorkspaceRepository(pool)
	if err != nil {
		return errors.New("workspace repository initialization failed")
	}
	if _, err := workspaces.Create(ctx, storage.CreateWorkspaceInput{ID: workspaceID, DisplayName: "Phase 12 load test"}); err != nil {
		return errors.New("loadtest workspace creation failed")
	}
	defer cleanupWorkspace(workspaces, workspaceID)

	startedAt := time.Now()
	result, runErr := execute(ctx, pool, workspaceID, opts, postgresVersion, startedAt)
	result.TotalWallTimeMS = milliseconds(time.Since(startedAt))
	result.GeneratedAt = time.Now().UTC()
	if runErr != nil {
		result.Failures = append(result.Failures, runErr.Error())
	}
	result.Passed = len(result.Failures) == 0
	if err := writeReport(opts.out, result); err != nil {
		return err
	}
	fmt.Printf("n=%d duplicates=%d p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms completed=%d failed=%d stuck=%d wall=%.2fms go=%s postgres=%s cpus=%d\n",
		result.N, result.Duplicates, result.ClientLatency.P50MS, result.ClientLatency.P95MS,
		result.ClientLatency.P99MS, result.ClientLatency.MaxMS, result.Completed, result.Failed,
		result.Stuck, result.TotalWallTimeMS, result.GoVersion, result.PostgresVersion, result.CPUCount)
	if !result.Passed {
		return errors.New("checks failed; see JSON report")
	}
	return nil
}

func parseOptions() (options, error) {
	defaultOut := filepath.Join("docs", "product", "reports", "loadtest-"+time.Now().Format("2006-01-02")+".json")
	opts := options{}
	flag.StringVar(&opts.databaseURL, "database-url", os.Getenv("DATABASE_URL"), "dedicated loopback PostgreSQL URL")
	flag.IntVar(&opts.n, "n", 120, "number of unique webhook deliveries")
	flag.IntVar(&opts.concurrency, "concurrency", 10, "concurrent HTTP clients and workflow workers")
	flag.Float64Var(&opts.duplicates, "duplicates", 0.1, "duplicate deliveries as a fraction of n")
	flag.StringVar(&opts.out, "out", defaultOut, "JSON report path")
	flag.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "whole-run timeout")
	flag.Parse()
	if opts.databaseURL == "" {
		return options{}, errors.New("DATABASE_URL or -database-url is required")
	}
	if opts.n < 1 || opts.concurrency < 1 || opts.duplicates < 0 || opts.duplicates > 1 || opts.timeout <= 0 || strings.TrimSpace(opts.out) == "" {
		return options{}, errors.New("invalid flags: n and concurrency must be positive, duplicates must be 0..1, timeout and out are required")
	}
	return opts, nil
}

func validateDatabaseURL(raw string) error {
	_, err := parseValidatedDatabaseURL(raw)
	return err
}

func parseValidatedDatabaseURL(raw string) (*pgxpool.Config, error) {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "PG") {
			continue
		}
		if name == "PGPORT" {
			port, err := strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 {
				return nil, errors.New("PGPORT must be numeric")
			}
			continue
		}
		return nil, errors.New("PG environment variables are not allowed")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("database URL must use postgres or postgresql")
	}
	if strings.Contains(u.Host, ",") {
		return nil, errors.New("database URL must contain exactly one host")
	}
	for _, key := range []string{"host", "hostaddr", "dbname", "service", "servicefile", "passfile"} {
		if _, present := u.Query()[key]; present {
			return nil, errors.New("database URL contains a forbidden connection option")
		}
	}
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("database connection configuration failed")
	}
	ip := net.ParseIP(config.ConnConfig.Host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("database host must be a loopback IP address")
	}
	if len(config.ConnConfig.Fallbacks) != 0 {
		return nil, errors.New("database connection fallbacks are not allowed")
	}
	if !strings.HasSuffix(config.ConnConfig.Database, "_loadtest") {
		return nil, errors.New("database name must end in _loadtest")
	}
	return config, nil
}

func migrate(ctx context.Context, databaseURL string) error {
	cmd := exec.CommandContext(ctx, "goose", "-dir", "db/migrations", "up")
	cmd.Env = append(os.Environ(), "GOOSE_DRIVER=postgres", "GOOSE_DBSTRING="+databaseURL)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("database migration failed; install goose and run from the repository root")
	}
	return nil
}

func execute(ctx context.Context, pool *pgxpool.Pool, workspaceID string, opts options, postgresVersion string, startedAt time.Time) (report, error) {
	result := report{N: opts.n, Concurrency: opts.concurrency, GoVersion: runtime.Version(), PostgresVersion: postgresVersion, CPUCount: runtime.NumCPU()}
	duplicateCount := int(float64(opts.n)*opts.duplicates + 0.5)
	result.Duplicates = duplicateCount

	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return result, errors.New("integration repository initialization failed")
	}
	targetID, err := newID()
	if err != nil {
		return result, err
	}
	if _, err := integrations.Create(ctx, storage.CreateIntegrationInput{
		ID:          targetID,
		WorkspaceID: workspaceID,
		Platform:    "telegram",
		Role:        tenant.IntegrationRoleControl,
		Config:      map[string]any{"chat_id": "-1000000000000", "bot_token_ref": "loadtest-telegram"},
		IsEnabled:   true,
	}); err != nil {
		return result, errors.New("target integration creation failed")
	}
	sourceID, err := newID()
	if err != nil {
		return result, err
	}
	if _, err := integrations.Create(ctx, storage.CreateIntegrationInput{
		ID:          sourceID,
		WorkspaceID: workspaceID,
		Platform:    "clickup",
		Role:        tenant.IntegrationRoleSource,
		Config:      map[string]any{"clickup_team_id": "1", "webhook_id": webhookID, "webhook_secret_ref": secretRef, "target_integration_id": targetID},
		IsEnabled:   true,
	}); err != nil {
		return result, errors.New("source integration creation failed")
	}

	behaviors, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return result, errors.New("behavior store initialization failed")
	}
	workflowDoc := &behavior.WorkflowDoc{Steps: []behavior.WorkflowStepDoc{
		{Key: "prepare", Kind: workflow.StepKindActions},
		{Key: "execute", Kind: workflow.StepKindActions, DependsOn: []string{"prepare"}},
		{Key: "finish", Kind: workflow.StepKindActions, DependsOn: []string{"execute"}},
	}}
	doc := behavior.Doc{
		Schema:       2,
		Name:         behaviorName,
		Triggers:     []behavior.TriggerDoc{{Type: behavior.TriggerCommentAdded}},
		Instructions: "Run the local load-test workflow.",
		Actions: map[string][]behavior.ActionDoc{
			"notify": {{Type: "telegram.message"}},
		},
		Workflow: workflowDoc,
	}
	rawDoc, err := behavior.Encode(doc)
	if err != nil {
		return result, errors.New("behavior encoding failed")
	}
	rawVars, err := behavior.EncodeVars(map[string]string{})
	if err != nil {
		return result, errors.New("behavior variables encoding failed")
	}
	if _, err := behaviors.SaveDraft(ctx, workspaceID, behaviorName, rawDoc, rawVars, "loadtest"); err != nil {
		return result, errors.New("behavior draft creation failed")
	}
	if _, err := behavior.PublishDraft(ctx, behaviors, workspaceID, behaviorName, nil, behavior.WithWorkflowCapabilities([]string{workflow.CapabilityInProcess})); err != nil {
		return result, errors.New("behavior publication failed")
	}
	if err := behaviors.SetStatus(ctx, workspaceID, behaviorName, behavior.StatusEnabled); err != nil {
		return result, errors.New("behavior enabling failed")
	}

	store, err := storage.NewWorkflowStore(pool)
	if err != nil {
		return result, errors.New("workflow store initialization failed")
	}
	if _, err := store.Cutover(ctx, workspaceID, true); err != nil {
		return result, errors.New("durable workflow cutover failed")
	}
	registry := workflow.NewRegistry()
	if err := registry.Register(workflow.StepKindActions, noOpStep{}); err != nil {
		return result, errors.New("workflow step registration failed")
	}
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	worker := workflow.NewWorker(store, registry, nil, workflow.WorkerSettings{Workers: opts.concurrency, PollInterval: time.Millisecond})
	var runtimeWG sync.WaitGroup
	runtimeWG.Add(2)
	go func() {
		defer runtimeWG.Done()
		_ = worker.Run(runtimeCtx)
	}()
	starterErrors := make(chan error, 1)
	go func() {
		defer runtimeWG.Done()
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, startErr := store.StartPendingBehaviorRuns(runtimeCtx, workspaceID, time.Now(), workflow.MaxListLimit); startErr != nil && runtimeCtx.Err() == nil {
				select {
				case starterErrors <- errors.New("workflow starter failed"):
				default:
				}
				return
			}
			select {
			case <-runtimeCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() {
		stopRuntime()
		runtimeWG.Wait()
	}()

	secret, err := randomSecret()
	if err != nil {
		return result, err
	}
	notices, err := storage.NewNoticeRepository(pool, newID, storage.DebounceSettings{})
	if err != nil {
		return result, errors.New("notice repository initialization failed")
	}
	serverLatency := &durationCollector{}
	e := echo.New()
	e.HideBanner = true
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			serverLatency.add(time.Since(start))
			return err
		}
	})
	handler := httpdelivery.NewWebhookHandler(integrations, httpdelivery.NewVerifiedParserFactory(memoryResolver{workspaceID: workspaceID, secret: secret}), relay.NewNoticeService(notices))
	e.POST("/webhook/:source/:integrationID", handler.HandleWebhook)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return result, errors.New("local HTTP listener failed")
	}
	server := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serverDone
	}()

	address := listener.Addr().String()
	client := localClient(address)
	clientLatency, httpFailures := deliver(ctx, client, "http://"+address+"/webhook/clickup/"+sourceID, secret, opts.n, duplicateCount, opts.concurrency)
	result.ClientLatency = summarize(clientLatency)
	result.ServerLatency = summarize(serverLatency.snapshot())
	result.HTTPFailures = httpFailures

	views, waitErr := waitForExecutions(ctx, store, workspaceID, opts.n, starterErrors)
	if waitErr != nil && !errors.Is(waitErr, context.DeadlineExceeded) && !errors.Is(waitErr, context.Canceled) {
		return result, waitErr
	}
	result.ExecutionCount = len(views)
	for _, view := range views {
		switch view.State {
		case workflow.StateCompleted:
			result.Completed++
		case workflow.StateFailed, workflow.StateCancelled:
			result.Failed++
		default:
			result.Stuck++
		}
	}
	if len(views) < opts.n {
		result.Stuck += opts.n - len(views)
	}
	if httpFailures != 0 {
		result.Failures = append(result.Failures, fmt.Sprintf("%d webhook requests did not return 2xx", httpFailures))
	}
	if len(views) != opts.n {
		result.Failures = append(result.Failures, fmt.Sprintf("execution count is %d, want %d", len(views), opts.n))
	}
	if result.Completed != opts.n || result.Failed != 0 || result.Stuck != 0 {
		result.Failures = append(result.Failures, "not all unique deliveries completed successfully")
	}
	if result.ClientLatency.P95MS >= 500 {
		result.Failures = append(result.Failures, fmt.Sprintf("client p95 latency %.2fms is not below 500ms", result.ClientLatency.P95MS))
	}
	if len(clientLatency) != opts.n+duplicateCount || len(serverLatency.snapshot()) != opts.n+duplicateCount {
		result.Failures = append(result.Failures, "latency sample count does not match delivery count")
	}
	result.TotalWallTimeMS = milliseconds(time.Since(startedAt))
	return result, waitErr
}

func deliver(ctx context.Context, client *http.Client, endpoint, secret string, n, duplicateCount, concurrency int) ([]time.Duration, int) {
	bodies := make([][]byte, n)
	for i := range n {
		bodies[i] = []byte(fmt.Sprintf(`{"webhook_id":%q,"event":"taskCommentPosted","task_id":"task-%d","history_items":[{"id":"history-%d","field":"comment","comment":{"id":"comment-%d","text_content":"load test","user":{"id":1}}}]}`, webhookID, i, i, i))
	}
	jobs := make(chan []byte)
	latencies := &durationCollector{}
	var failures int
	var failureMu sync.Mutex
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for body := range jobs {
				started := time.Now()
				status, err := post(ctx, client, endpoint, secret, body)
				latencies.add(time.Since(started))
				if err != nil || status < 200 || status >= 300 {
					failureMu.Lock()
					failures++
					failureMu.Unlock()
				}
			}
		}()
	}
	for _, body := range bodies {
		jobs <- body
	}
	for i := range duplicateCount {
		jobs <- bodies[i%len(bodies)]
	}
	close(jobs)
	workers.Wait()
	return latencies.snapshot(), failures
}

func post(ctx context.Context, client *http.Client, endpoint, secret string, body []byte) (int, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func localClient(address string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, requested string) (net.Conn, error) {
			if network != "tcp" || requested != address {
				return nil, errors.New("loadtest: non-local HTTP connection denied")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp4", address)
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func waitForExecutions(ctx context.Context, store *storage.WorkflowStore, workspaceID string, want int, starterErrors <-chan error) ([]workflow.ExecutionView, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		views, err := listExecutions(ctx, store, workspaceID)
		if err != nil {
			return nil, errors.New("execution query failed")
		}
		terminal := len(views) == want
		for _, view := range views {
			terminal = terminal && view.State.IsTerminal()
		}
		if terminal {
			return views, nil
		}
		select {
		case err := <-starterErrors:
			return views, err
		case <-ctx.Done():
			return views, ctx.Err()
		case <-ticker.C:
		}
	}
}

func listExecutions(ctx context.Context, store *storage.WorkflowStore, workspaceID string) ([]workflow.ExecutionView, error) {
	var out []workflow.ExecutionView
	var before int64
	for {
		page, next, err := store.ListExecutions(ctx, workflow.ExecutionQuery{WorkspaceID: workspaceID, BeforeID: before, Limit: workflow.MaxListLimit})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == 0 {
			return out, nil
		}
		before = next
	}
}

func summarize(values []time.Duration) latencyReport {
	if len(values) == 0 {
		return latencyReport{}
	}
	slices.Sort(values)
	return latencyReport{
		P50MS: milliseconds(percentile(values, 0.50)),
		P95MS: milliseconds(percentile(values, 0.95)),
		P99MS: milliseconds(percentile(values, 0.99)),
		MaxMS: milliseconds(values[len(values)-1]),
	}
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	index := int(float64(len(values))*fraction+0.999999999) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

func milliseconds(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }

func writeReport(path string, value report) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("report encoding failed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errors.New("report directory creation failed")
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return errors.New("report write failed")
	}
	return nil
}

func randomSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("webhook secret generation failed")
	}
	return hex.EncodeToString(raw), nil
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", errors.New("identifier generation failed")
	}
	return id.String(), nil
}

func cleanupWorkspace(repository *storage.WorkspaceRepository, workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = repository.Delete(ctx, workspaceID)
}
