package orchestrator

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

var t0 = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestBudget(caps BudgetCaps) (*Budget, *fakeClock) {
	c := &fakeClock{now: t0}
	b := NewBudget(caps, t0)
	b.Clock = c.Now
	return b, c
}

func wantExceeded(t *testing.T, err error, kind BudgetKind) {
	t.Helper()
	var be *BudgetExceededError
	if !errors.As(err, &be) || be.Kind != kind {
		t.Fatalf("want exceeded %s, got %v", kind, err)
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal("not ErrBudgetExceeded")
	}
	if be.Reason() != ReasonPlanningBudgetExceeded || ReasonPlanningBudgetExceeded != "planning_budget_exceeded" {
		t.Fatalf("reason %q", be.Reason())
	}
}

func TestDefaultBudgetCaps(t *testing.T) {
	c := DefaultBudgetCaps()
	if c.PlanIterations != 3 || c.Replans != 2 || c.RepairAttempts != 2 || c.Tokens != 80000 || c.Wall != 90*time.Minute {
		t.Fatalf("%+v", c)
	}
	if c.PlanCaps() != (BudgetRequest{PlanIterations: 3, Replans: 2, Tokens: 80000, WallSeconds: 5400}) {
		t.Fatalf("%+v", c.PlanCaps())
	}
}

// TC-B1 (REQ-10): CanCall checks before the call and changes nothing.
func TestBudget_CanCall(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{PlanIterations: 3, Tokens: 1000, Wall: time.Hour})
	if err := b.CanCall(1000); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, b.CanCall(1001), BudgetTokens)
	if err := b.CanCall(-5); err != nil {
		t.Fatalf("negative estimate must act as 0: %v", err)
	}
	if b.Snapshot().Used != (BudgetUsed{}) {
		t.Fatal("CanCall changed the counters")
	}
	b.RecordUsage(Usage{Tokens: domain.Usage{Input: 600}})
	if err := b.CanCall(400); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, b.CanCall(401), BudgetTokens)
	b.RecordUsage(Usage{Tokens: domain.Usage{Output: 400}})
	wantExceeded(t, b.CanCall(0), BudgetTokens)
}

// TC-B2 (REQ-10): a zero or negative cap allows nothing.
func TestBudget_ZeroCapsAllowNothing(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{})
	wantExceeded(t, b.CanCall(0), BudgetWall)
	wantExceeded(t, b.CheckWall(), BudgetWall)
	wantExceeded(t, b.Charge(BudgetTokens, 1), BudgetTokens)
	wantExceeded(t, b.Charge(BudgetPlanIterations, 1), BudgetPlanIterations)
	wantExceeded(t, b.StartPlanIteration(false, 0), BudgetWall)
	if b.RepairAllowance(5) != 0 {
		t.Fatal("repair allowed with zero cap")
	}
	b2, _ := newTestBudget(BudgetCaps{Wall: -time.Hour, Tokens: -5})
	wantExceeded(t, b2.CanCall(0), BudgetWall)
	b3, _ := newTestBudget(BudgetCaps{Wall: time.Hour, Tokens: 0})
	wantExceeded(t, b3.CanCall(0), BudgetTokens)
}

// TC-B3 (REQ-10): Charge is all-or-nothing and validates its arguments.
func TestBudget_ChargeAllOrNothing(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{PlanIterations: 2, Replans: 1, RepairAttempts: 2, Tokens: 100, Wall: time.Hour})
	if err := b.Charge(BudgetTokens, 60); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, b.Charge(BudgetTokens, 41), BudgetTokens)
	if b.Snapshot().Used.Tokens != 60 {
		t.Fatalf("failed charge changed tokens to %d", b.Snapshot().Used.Tokens)
	}
	if err := b.Charge(BudgetTokens, 40); err != nil {
		t.Fatal("exact fit must be allowed:", err)
	}
	wantExceeded(t, b.Charge(BudgetTokens, 1), BudgetTokens)
	if err := b.Charge(BudgetReplans, 1); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, b.Charge(BudgetReplans, 1), BudgetReplans)
	wantExceeded(t, b.Charge(BudgetRepairAttempts, 3), BudgetRepairAttempts)
	if b.Snapshot().Used.RepairAttempts != 0 {
		t.Fatal("failed repair charge changed state")
	}
	if err := b.Charge(BudgetTokens, -1); err == nil {
		t.Fatal("negative charge accepted")
	}
	if err := b.Charge(BudgetWall, 1); !errors.Is(err, ErrUnknownBudgetKind) {
		t.Fatalf("wall charge: %v", err)
	}
	if err := b.Charge("bogus", 1); !errors.Is(err, ErrUnknownBudgetKind) {
		t.Fatalf("bogus charge: %v", err)
	}
	// Overflow attempt does not wrap around.
	b2, _ := newTestBudget(BudgetCaps{Tokens: 100, Wall: time.Hour})
	wantExceeded(t, b2.Charge(BudgetTokens, 1<<62), BudgetTokens)
}

// TC-B4 (REQ-10): StartPlanIteration charges iteration and replan all-or-nothing; 3 iterations allow 2 replans.
func TestBudget_StartPlanIteration(t *testing.T) {
	b, _ := newTestBudget(DefaultBudgetCaps())
	if err := b.StartPlanIteration(false, 100); err != nil {
		t.Fatal(err)
	}
	if err := b.StartPlanIteration(true, 100); err != nil {
		t.Fatal(err)
	}
	if err := b.StartPlanIteration(true, 100); err != nil {
		t.Fatal(err)
	}
	u := b.Snapshot().Used
	if u.PlanIterations != 3 || u.Replans != 2 {
		t.Fatalf("%+v", u)
	}
	wantExceeded(t, b.StartPlanIteration(false, 0), BudgetPlanIterations)

	// replans exhausted before iterations: the iteration must not be kept.
	b, _ = newTestBudget(BudgetCaps{PlanIterations: 5, Replans: 1, Tokens: 1000, Wall: time.Hour})
	if err := b.StartPlanIteration(false, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.StartPlanIteration(true, 0); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, b.StartPlanIteration(true, 0), BudgetReplans)
	if u := b.Snapshot().Used; u.PlanIterations != 2 || u.Replans != 1 {
		t.Fatalf("partial charge kept: %+v", u)
	}
	// Estimate that does not fit stops it before any charge.
	wantExceeded(t, b.StartPlanIteration(false, 1001), BudgetTokens)
	if b.Snapshot().Used.PlanIterations != 2 {
		t.Fatal("iteration charged despite token estimate")
	}
}

// TC-B5 (REQ-10): wall time runs from the execution start, including waits, without any call.
func TestBudget_WallIncludesWaits(t *testing.T) {
	b, clk := newTestBudget(BudgetCaps{PlanIterations: 3, Tokens: 1000, Wall: 20 * time.Minute})
	clk.Advance(19*time.Minute + 59*time.Second)
	if err := b.CanCall(0); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckWall(); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	wantExceeded(t, b.CanCall(0), BudgetWall)
	wantExceeded(t, b.CheckWall(), BudgetWall)
	wantExceeded(t, b.StartPlanIteration(false, 0), BudgetWall)
	if b.Snapshot().Used.PlanIterations != 0 {
		t.Fatal("iteration charged after wall exceeded")
	}
	if b.Snapshot().Used.Tokens != 0 {
		t.Fatal("waiting spent tokens")
	}
	if b.Elapsed() != 20*time.Minute {
		t.Fatalf("elapsed %v", b.Elapsed())
	}
	// A clock that goes backwards never yields negative elapsed time.
	clk.now = t0.Add(-time.Hour)
	if b.Elapsed() != 0 {
		t.Fatalf("elapsed %v", b.Elapsed())
	}
}

// TC-B6 (REQ-10): tokens include cache read and write; spend past the cap is recorded.
func TestBudget_RecordUsage(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{PlanIterations: 3, RepairAttempts: 5, Tokens: 100, Wall: time.Hour})
	b.RecordUsage(Usage{Tokens: domain.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}, Attempts: 1})
	if u := b.Snapshot().Used; u.Tokens != 10 || u.RepairAttempts != 0 {
		t.Fatalf("%+v", u)
	}
	b.RecordUsage(Usage{Tokens: domain.Usage{Input: 5}, Attempts: 3})
	if u := b.Snapshot().Used; u.Tokens != 15 || u.RepairAttempts != 2 {
		t.Fatalf("%+v", u)
	}
	b.RecordUsage(Usage{Attempts: 0})
	b.RecordUsage(Usage{Tokens: domain.Usage{Input: -50}, Attempts: -2})
	if u := b.Snapshot().Used; u.Tokens != 15 || u.RepairAttempts != 2 {
		t.Fatalf("negative usage changed counters: %+v", u)
	}
	b.RecordUsage(Usage{Tokens: domain.Usage{CacheRead: 500}})
	if u := b.Snapshot().Used; u.Tokens != 515 {
		t.Fatalf("overspend not recorded: %+v", u)
	}
	wantExceeded(t, b.CanCall(0), BudgetTokens)
}

// TC-B7 (REQ-10): RepairAllowance never exceeds the repair budget.
func TestBudget_RepairAllowance(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{RepairAttempts: 3, Tokens: 100, Wall: time.Hour})
	if got := b.RepairAllowance(2); got != 2 {
		t.Fatal(got)
	}
	if got := b.RepairAllowance(10); got != 3 {
		t.Fatal(got)
	}
	b.RecordUsage(Usage{Attempts: 3}) // 2 repairs
	if got := b.RepairAllowance(2); got != 1 {
		t.Fatal(got)
	}
	b.RecordUsage(Usage{Attempts: 5}) // 4 more: over the cap
	if got := b.RepairAllowance(2); got != 0 {
		t.Fatal(got)
	}
	if got := b.RepairAllowance(-3); got != 0 {
		t.Fatal(got)
	}
}

// TC-B8 (REQ-10): Snapshot and Restore round trip through JSON; a restart keeps the counters.
func TestBudget_SnapshotRestoreRoundTrip(t *testing.T) {
	b, clk := newTestBudget(DefaultBudgetCaps())
	_ = b.StartPlanIteration(false, 0)
	_ = b.StartPlanIteration(true, 0)
	b.RecordUsage(Usage{Tokens: domain.Usage{Input: 1000, CacheWrite: 234}, Attempts: 2})
	if err := b.Extend(BudgetReplans); err != nil {
		t.Fatal(err)
	}
	clk.Advance(7 * time.Minute)
	raw, err := json.Marshal(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap BudgetSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	restarted := &Budget{}
	clk2 := &fakeClock{now: t0.Add(7 * time.Minute)}
	restarted.Clock = clk2.Now
	if err := restarted.Restore(snap); err != nil {
		t.Fatal(err)
	}
	got, want := restarted.Snapshot(), b.Snapshot()
	if got.Caps != want.Caps || got.Used != want.Used || got.Extensions != want.Extensions || !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("restored %+v want %+v", got, want)
	}
	if restarted.Elapsed() != 7*time.Minute {
		t.Fatalf("wall did not continue from the original start: %v", restarted.Elapsed())
	}
	if restarted.ExtensionsLeft() != MaxBudgetExtensions-1 {
		t.Fatal("extensions lost")
	}
	// Counters keep binding after the restart.
	if err := restarted.StartPlanIteration(false, 0); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, restarted.StartPlanIteration(false, 0), BudgetPlanIterations)
}

func TestBudget_RestoreRejectsInvalid(t *testing.T) {
	b, _ := newTestBudget(DefaultBudgetCaps())
	_ = b.Charge(BudgetTokens, 5)
	bad := []BudgetSnapshot{
		{Caps: DefaultBudgetCaps(), StartedAt: t0, Used: BudgetUsed{Tokens: -1}},
		{Caps: DefaultBudgetCaps(), StartedAt: t0, Used: BudgetUsed{PlanIterations: -1}},
		{Caps: DefaultBudgetCaps(), StartedAt: t0, Used: BudgetUsed{Replans: -1}},
		{Caps: DefaultBudgetCaps(), StartedAt: t0, Used: BudgetUsed{RepairAttempts: -1}},
		{Caps: DefaultBudgetCaps(), StartedAt: t0, Extensions: -1},
		{Caps: DefaultBudgetCaps()},
	}
	for i, s := range bad {
		if err := b.Restore(s); err == nil {
			t.Fatalf("invalid snapshot %d accepted", i)
		}
	}
	if b.Snapshot().Used.Tokens != 5 {
		t.Fatal("rejected restore changed the budget")
	}
}

// TC-B9 (REQ-10): Extend per kind.
func TestBudget_ExtendPerKind(t *testing.T) {
	b, _ := newTestBudget(DefaultBudgetCaps())
	base := b.Snapshot().Caps
	if err := b.Extend(BudgetPlanIterations); err != nil {
		t.Fatal(err)
	}
	if err := b.Extend(BudgetReplans); err != nil {
		t.Fatal(err)
	}
	if err := b.Extend(BudgetRepairAttempts); err != nil {
		t.Fatal(err)
	}
	c := b.Snapshot().Caps
	if c.PlanIterations != base.PlanIterations+1 || c.Replans != base.Replans+1 || c.RepairAttempts != base.RepairAttempts+1 {
		t.Fatalf("%+v", c)
	}
	if c.Tokens != base.Tokens || c.Wall != base.Wall {
		t.Fatalf("other limits changed %+v", c)
	}

	b, _ = newTestBudget(DefaultBudgetCaps())
	if err := b.Extend(BudgetTokens); err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot().Caps.Tokens; got != 100000 {
		t.Fatalf("tokens +25%%: %d", got)
	}
	b, _ = newTestBudget(BudgetCaps{Tokens: 10, Wall: time.Hour})
	_ = b.Extend(BudgetTokens)
	if got := b.Snapshot().Caps.Tokens; got != 13 {
		t.Fatalf("rounded up: %d", got)
	}
	b, _ = newTestBudget(BudgetCaps{Wall: time.Hour})
	_ = b.Extend(BudgetTokens)
	if got := b.Snapshot().Caps.Tokens; got != DefaultMaxTokens/4 {
		t.Fatalf("zero token cap extension: %d", got)
	}
	b, _ = newTestBudget(DefaultBudgetCaps())
	if err := b.Extend(BudgetWall); err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot().Caps.Wall; got != 100*time.Minute {
		t.Fatalf("wall +10m: %v", got)
	}
}

// TC-B10 (REQ-10): at most 3 extensions; unknown kinds do not count.
func TestBudget_ExtendLimit(t *testing.T) {
	b, _ := newTestBudget(DefaultBudgetCaps())
	if err := b.Extend("bogus"); !errors.Is(err, ErrUnknownBudgetKind) {
		t.Fatalf("%v", err)
	}
	if b.ExtensionsLeft() != MaxBudgetExtensions {
		t.Fatal("unknown kind consumed an extension")
	}
	for i := 0; i < MaxBudgetExtensions; i++ {
		if err := b.Extend(BudgetPlanIterations); err != nil {
			t.Fatalf("extension %d: %v", i, err)
		}
	}
	before := b.Snapshot()
	for _, k := range []BudgetKind{BudgetPlanIterations, BudgetWall, BudgetTokens, "bogus"} {
		if err := b.Extend(k); !errors.Is(err, ErrBudgetExtensionLimit) {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if b.Snapshot() != before {
		t.Fatal("rejected extension changed the budget")
	}
	if b.ExtensionsLeft() != 0 || MaxBudgetExtensions != 3 {
		t.Fatalf("left %d", b.ExtensionsLeft())
	}
}

// TC-B11 (REQ-10, human): after a long wait for the owner, an extension still leaves at least 10 minutes of wall time.
func TestBudget_ExtendLeavesTenMinutes(t *testing.T) {
	for _, kind := range []BudgetKind{BudgetPlanIterations, BudgetReplans, BudgetRepairAttempts, BudgetTokens, BudgetWall} {
		t.Run(string(kind), func(t *testing.T) {
			b, clk := newTestBudget(DefaultBudgetCaps())
			clk.Advance(2 * time.Hour) // wall long spent
			wantExceeded(t, b.CanCall(0), BudgetWall)
			if err := b.Extend(kind); err != nil {
				t.Fatal(err)
			}
			if left := b.Snapshot().Caps.Wall - b.Elapsed(); left < ExtendWall {
				t.Fatalf("only %v of wall time left", left)
			}
			if err := b.CheckWall(); err != nil {
				t.Fatalf("wall still exceeded after extension: %v", err)
			}
		})
	}
	// Plenty left: the floor does not shrink or inflate the wall cap.
	b, _ := newTestBudget(DefaultBudgetCaps())
	_ = b.Extend(BudgetReplans)
	if b.Snapshot().Caps.Wall != 90*time.Minute {
		t.Fatalf("wall changed to %v", b.Snapshot().Caps.Wall)
	}
}

// TC-B12 (REQ-10): extension after exhaustion makes the next call possible and never lowers a counter.
func TestBudget_ExtendAfterExhaustion(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{PlanIterations: 1, Replans: 1, RepairAttempts: 1, Tokens: 100, Wall: time.Hour})
	_ = b.StartPlanIteration(false, 0)
	wantExceeded(t, b.StartPlanIteration(true, 0), BudgetPlanIterations)
	if err := b.Extend(BudgetPlanIterations); err != nil {
		t.Fatal(err)
	}
	if err := b.StartPlanIteration(true, 0); err != nil {
		t.Fatal(err)
	}
	// Restored over-cap counters: extension adds one on top of use, not on top of the cap.
	snap := b.Snapshot()
	snap.Used.Replans = 5
	snap.Extensions = 0
	if err := b.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if err := b.Extend(BudgetReplans); err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot().Caps.Replans; got != 6 {
		t.Fatalf("cap %d", got)
	}
	if err := b.Charge(BudgetReplans, 1); err != nil {
		t.Fatal(err)
	}
	wantExceeded(t, b.Charge(BudgetReplans, 1), BudgetReplans)
}

// TC-B13 (REQ-10): concurrent charges never overspend (run with -race).
func TestBudget_ConcurrentChargesRespectCap(t *testing.T) {
	b, _ := newTestBudget(BudgetCaps{Tokens: 10, Wall: time.Hour})
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Charge(BudgetTokens, 1) == nil {
				atomic.AddInt32(&ok, 1)
			}
			_ = b.CanCall(1)
			_ = b.Snapshot()
		}()
	}
	wg.Wait()
	if ok != 10 || b.Snapshot().Used.Tokens != 10 {
		t.Fatalf("ok=%d used=%d", ok, b.Snapshot().Used.Tokens)
	}
}
