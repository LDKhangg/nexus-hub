package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// Long polling (getUpdates) for hubs without a public HTTPS URL. Every
// Telegram control integration gets one loop; each update goes to the same
// handler path as the webhook. The offset only moves past an update after
// the handler accepted it. Telegram itself keeps every update not yet
// confirmed by a later getUpdates offset (for 24 hours), so after a
// restart the loop starts without an offset and receives them again; the
// inbox deduplicates by update_id (at-least-once).

// Poller defaults.
const (
	DefaultPollTimeout = 50 * time.Second // getUpdates long-poll wait
	DefaultPollRefresh = time.Minute      // how often the integration list is read again
	DefaultMinBackoff  = time.Second
	DefaultMaxBackoff  = 30 * time.Second
)

// PollAllowedUpdates are the update types the hub handles.
var PollAllowedUpdates = []string{"message", "callback_query"}

// updateInvalid is the handler result for a malformed update (see
// delivery/http UpdateInvalid): it can never be stored and is skipped.
const updateInvalid = "invalid"

// PollTarget is one Telegram control integration and its bot token
// reference.
type PollTarget struct {
	WorkspaceID, IntegrationID, TokenRef string
}

// PollTargets lists the Telegram control integrations to poll.
type PollTargets interface {
	TelegramPollTargets(ctx context.Context) ([]PollTarget, error)
}

// UpdateHandler handles one raw update (delivery/http TelegramHandler). An
// error means the update must be offered again later.
type UpdateHandler interface {
	HandleUpdate(ctx context.Context, integrationID string, raw []byte) (string, error)
}

// UpdatesAPI is the Bot API part the poller uses (implemented by Client).
type UpdatesAPI interface {
	GetUpdates(ctx context.Context, workspaceID, tokenRef string, offset int64, timeoutSec int, allowedUpdates []string) ([]json.RawMessage, error)
	DeleteWebhook(ctx context.Context, workspaceID, tokenRef string) error
	BotFingerprint(ctx context.Context, workspaceID, tokenRef string) (string, error)
}

// PollerSettings tune the poller. Zero values take the defaults.
type PollerSettings struct {
	PollTimeout time.Duration
	Refresh     time.Duration
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
}

// Poller long-polls getUpdates for every Telegram control integration.
type Poller struct {
	targets PollTargets
	api     UpdatesAPI
	handler UpdateHandler
	s       PollerSettings
}

// NewPoller builds a poller. The API's HTTP client timeout must exceed
// PollTimeout.
func NewPoller(targets PollTargets, api UpdatesAPI, handler UpdateHandler, s PollerSettings) *Poller {
	if s.PollTimeout < time.Second {
		s.PollTimeout = DefaultPollTimeout
	}
	if s.Refresh <= 0 {
		s.Refresh = DefaultPollRefresh
	}
	if s.MinBackoff <= 0 {
		s.MinBackoff = DefaultMinBackoff
	}
	if s.MaxBackoff < s.MinBackoff {
		s.MaxBackoff = DefaultMaxBackoff
		if s.MaxBackoff < s.MinBackoff {
			s.MaxBackoff = s.MinBackoff
		}
	}
	return &Poller{targets: targets, api: api, handler: handler, s: s}
}

type pollLoop struct {
	target      PollTarget
	fingerprint string
	cancel      context.CancelFunc
}

// Run polls until ctx ends, then stops every loop, waits for them and
// returns nil. A failed listing or call never stops it.
func (p *Poller) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	active := map[string]*pollLoop{}
	skipped := map[string]string{} // integration -> reason already logged
	defer func() {
		for _, l := range active {
			l.cancel()
		}
		wg.Wait()
	}()
	for {
		p.refresh(ctx, &wg, active, skipped)
		t := time.NewTimer(p.s.Refresh)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// refresh starts a loop for each new target, and stops loops whose
// integration is gone or whose bot changed. Two integrations of one bot
// would steal each other's updates, so only the first one is polled.
func (p *Poller) refresh(ctx context.Context, wg *sync.WaitGroup, active map[string]*pollLoop, skipped map[string]string) {
	targets, err := p.targets.TelegramPollTargets(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Print("telegram poll: listing integrations failed; retrying later")
		}
		return
	}
	want := map[string]PollTarget{}
	fingerprints := map[string]string{}
	for _, t := range targets {
		fp, err := p.api.BotFingerprint(ctx, t.WorkspaceID, t.TokenRef)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.skip(skipped, t, ErrorCode(err))
			continue
		}
		want[t.IntegrationID], fingerprints[t.IntegrationID] = t, fp
	}
	for id, l := range active {
		if t, ok := want[id]; !ok || t != l.target || fingerprints[id] != l.fingerprint {
			l.cancel()
			delete(active, id)
		}
	}
	inUse := map[string]bool{}
	for _, l := range active {
		inUse[l.fingerprint] = true
	}
	for _, t := range targets {
		fp, ok := fingerprints[t.IntegrationID]
		if !ok || active[t.IntegrationID] != nil {
			continue
		}
		if inUse[fp] {
			p.skip(skipped, t, "bot_shared")
			continue
		}
		inUse[fp] = true
		delete(skipped, t.IntegrationID)
		lctx, cancel := context.WithCancel(ctx)
		active[t.IntegrationID] = &pollLoop{target: t, fingerprint: fp, cancel: cancel}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.poll(lctx, t)
		}()
	}
}

// skip logs once per integration and reason why it is not polled.
func (p *Poller) skip(skipped map[string]string, t PollTarget, reason string) {
	if skipped[t.IntegrationID] == reason {
		return
	}
	skipped[t.IntegrationID] = reason
	log.Printf("telegram poll: not polling workspace=%s integration=%s reason=%s", t.WorkspaceID, t.IntegrationID, reason)
}

// poll is one integration's loop. It removes a webhook once, only when
// getUpdates reports a conflict, and backs off (doubling, MinBackoff to
// MaxBackoff, or the server's retry_after) on failures.
func (p *Poller) poll(ctx context.Context, t PollTarget) {
	var offset int64
	var backoff time.Duration
	webhookRemoved := false
	timeoutSec := int(p.s.PollTimeout / time.Second)
	fail := func(retryAfter time.Duration) {
		if backoff == 0 {
			backoff = p.s.MinBackoff
		} else if backoff *= 2; backoff > p.s.MaxBackoff {
			backoff = p.s.MaxBackoff
		}
		wait := backoff
		if retryAfter > wait {
			wait = retryAfter
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	for ctx.Err() == nil {
		updates, err := p.api.GetUpdates(ctx, t.WorkspaceID, t.TokenRef, offset, timeoutSec, PollAllowedUpdates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			code := ErrorCode(err)
			if code == "conflict" && !webhookRemoved {
				webhookRemoved = true
				if derr := p.api.DeleteWebhook(ctx, t.WorkspaceID, t.TokenRef); derr == nil {
					log.Print("telegram: webhook removed for polling")
					continue
				} else if ctx.Err() == nil {
					log.Printf("telegram poll: removing webhook failed integration=%s code=%s", t.IntegrationID, ErrorCode(derr))
				}
			}
			log.Printf("telegram poll: getUpdates failed integration=%s code=%s", t.IntegrationID, code)
			var de *relay.DeliveryError
			var after time.Duration
			if errors.As(err, &de) {
				after = de.RetryAfter
			}
			fail(after)
			continue
		}
		deferred, start := false, offset
		for _, raw := range updates {
			id := rawUpdateID(raw)
			if id <= 0 {
				continue // no update_id: nothing to confirm or store
			}
			if id < offset {
				continue
			}
			status, err := p.handler.HandleUpdate(ctx, t.IntegrationID, raw)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("telegram poll: update deferred integration=%s update=%d", t.IntegrationID, id)
				deferred = true
				break
			}
			if status == updateInvalid {
				log.Printf("telegram poll: invalid update skipped integration=%s update=%d", t.IntegrationID, id)
			}
			offset = id + 1
		}
		if deferred || (len(updates) > 0 && offset == start) {
			fail(0) // nothing could be confirmed: never spin on the same batch
			continue
		}
		backoff = 0
	}
}

// rawUpdateID reads update_id; 0 when it is missing or malformed.
func rawUpdateID(raw json.RawMessage) int64 {
	var u struct {
		UpdateID int64 `json:"update_id"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0
	}
	return u.UpdateID
}
