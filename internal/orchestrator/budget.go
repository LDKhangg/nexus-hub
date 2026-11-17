package orchestrator

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Setting keys of the per-execution budget caps (O5a); missing settings use
// the defaults below.
const (
	SettingMaxPlanIterations = "orchestrator_max_plan_iterations"
	SettingMaxReplans        = "orchestrator_max_replans"
	SettingMaxRepairAttempts = "orchestrator_max_repair_attempts"
	SettingMaxTokens         = "orchestrator_max_tokens"
	SettingMaxWallMinutes    = "orchestrator_max_wall_minutes"
)

// Default per-execution budget caps (O5a).
const (
	DefaultMaxPlanIterations = 3
	DefaultMaxReplans        = 2
	DefaultMaxRepairAttempts = 2
	DefaultMaxTokens         = int64(80000)
	// DefaultMaxWall leaves room for one 7a worker job (60 min wall).
	DefaultMaxWall = 90 * time.Minute
)

// Budget extension ("Thêm 1 vòng", O4a) limits.
const (
	// MaxBudgetExtensions bounds Extend per execution.
	MaxBudgetExtensions = 3
	// ExtendWall is the wall time an extension adds, and the least wall
	// time left after any extension.
	ExtendWall = 10 * time.Minute
)

// O4a outcome when a budget is spent: the execution waits for the owner
// (buttons "Thêm 1 vòng" / "Huỷ"); an expired wait fails. Reason is the
// reason code of both.
const (
	BudgetOutcomeState           = "WAITING_FOR_USER"
	BudgetExpiredState           = "FAILED"
	ReasonPlanningBudgetExceeded = "planning_budget_exceeded"
)

// BudgetKind names one budget counter.
type BudgetKind string

const (
	BudgetPlanIterations BudgetKind = "plan_iterations"
	BudgetReplans        BudgetKind = "replans"
	BudgetRepairAttempts BudgetKind = "repair_attempts"
	BudgetTokens         BudgetKind = "tokens"
	BudgetWall           BudgetKind = "wall_time"
)

// ErrBudgetExceeded means a budget limit is reached; no model call may be
// made. Errors wrapping it are *BudgetExceededError.
var ErrBudgetExceeded = errors.New("orchestrator: budget exceeded")

// ErrBudgetExtensionLimit means MaxBudgetExtensions are already used.
var ErrBudgetExtensionLimit = errors.New("orchestrator: budget extension limit reached")

// ErrUnknownBudgetKind means a kind the operation does not accept.
var ErrUnknownBudgetKind = errors.New("orchestrator: unknown budget kind")

// BudgetExceededError says which limit was reached.
type BudgetExceededError struct {
	Kind BudgetKind
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("orchestrator: budget exceeded: %s", e.Kind)
}

// Unwrap makes errors.Is(err, ErrBudgetExceeded) true.
func (e *BudgetExceededError) Unwrap() error { return ErrBudgetExceeded }

// Reason is the reason code of the O4 outcome.
func (e *BudgetExceededError) Reason() string { return ReasonPlanningBudgetExceeded }

// BudgetCaps are the limits of one execution. A cap <= 0 allows nothing.
type BudgetCaps struct {
	PlanIterations int           `json:"plan_iterations"`
	Replans        int           `json:"replans"`
	RepairAttempts int           `json:"repair_attempts"`
	Tokens         int64         `json:"tokens"`
	Wall           time.Duration `json:"wall_ns"`
}

// DefaultBudgetCaps returns the O5a defaults.
func DefaultBudgetCaps() BudgetCaps {
	return BudgetCaps{
		PlanIterations: DefaultMaxPlanIterations,
		Replans:        DefaultMaxReplans,
		RepairAttempts: DefaultMaxRepairAttempts,
		Tokens:         DefaultMaxTokens,
		Wall:           DefaultMaxWall,
	}
}

// PlanCaps returns the caps as the BudgetRequest a Plan call is clamped to.
func (c BudgetCaps) PlanCaps() BudgetRequest {
	return BudgetRequest{
		PlanIterations: c.PlanIterations,
		Replans:        c.Replans,
		Tokens:         c.Tokens,
		WallSeconds:    int(c.Wall / time.Second),
	}
}

// BudgetUsed are the counters spent so far. Wall time is not a counter: it
// is the time since StartedAt.
type BudgetUsed struct {
	PlanIterations int   `json:"plan_iterations"`
	Replans        int   `json:"replans"`
	RepairAttempts int   `json:"repair_attempts"`
	Tokens         int64 `json:"tokens"`
}

// BudgetSnapshot is the JSON checkpoint of a Budget (REQ-10); restoring it
// resumes with the same counters.
type BudgetSnapshot struct {
	Caps       BudgetCaps `json:"caps"`
	Used       BudgetUsed `json:"used"`
	StartedAt  time.Time  `json:"started_at"`
	Extensions int        `json:"extensions"`
}

// Budget holds the caps and counters of one execution (REQ-10). Check
// before every model call (CanCall, StartPlanIteration, RepairAllowance)
// and record what the call spent (RecordUsage). Wall time runs from the
// execution start, including waits for a worker; waits never spend tokens.
// Safe for concurrent use.
type Budget struct {
	// Clock returns the current time; nil means time.Now.
	Clock func() time.Time

	mu   sync.Mutex
	snap BudgetSnapshot
}

// NewBudget returns a budget with caps whose wall time starts at startedAt.
func NewBudget(caps BudgetCaps, startedAt time.Time) *Budget {
	return &Budget{snap: BudgetSnapshot{Caps: caps, StartedAt: startedAt}}
}

func (b *Budget) now() time.Time {
	if b.Clock != nil {
		return b.Clock()
	}
	return time.Now()
}

// Snapshot returns the checkpoint of the budget.
func (b *Budget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snap
}

// Restore replaces the budget with a checkpoint. Negative counters, a
// negative extension count or a zero start time are rejected and leave the
// budget unchanged.
func (b *Budget) Restore(s BudgetSnapshot) error {
	u := s.Used
	if u.PlanIterations < 0 || u.Replans < 0 || u.RepairAttempts < 0 || u.Tokens < 0 ||
		s.Extensions < 0 || s.StartedAt.IsZero() {
		return errors.New("orchestrator: invalid budget snapshot")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snap = s
	return nil
}

// Elapsed is the wall time since the execution start.
func (b *Budget) Elapsed() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.elapsedLocked()
}

func (b *Budget) elapsedLocked() time.Duration {
	d := b.now().Sub(b.snap.StartedAt)
	if d < 0 {
		return 0
	}
	return d
}

func exceeded(k BudgetKind) error { return &BudgetExceededError{Kind: k} }

// checkCallLocked fails when the wall time is spent or the token cap is
// reached (estimate > 0 must also fit).
func (b *Budget) checkCallLocked(estimate int64) error {
	c, u := b.snap.Caps, b.snap.Used
	if c.Wall <= 0 || b.elapsedLocked() >= c.Wall {
		return exceeded(BudgetWall)
	}
	if estimate < 0 {
		estimate = 0
	}
	if c.Tokens <= 0 || u.Tokens >= c.Tokens || u.Tokens+estimate > c.Tokens {
		return exceeded(BudgetTokens)
	}
	return nil
}

// CanCall reports whether a model call that may spend about estimate tokens
// is allowed: wall time and tokens remain. It changes nothing.
func (b *Budget) CanCall(estimate int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkCallLocked(estimate)
}

// CheckWall fails when the wall time is spent (for waits without a model
// call, such as waiting for a worker).
func (b *Budget) CheckWall() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.snap.Caps; c.Wall <= 0 || b.elapsedLocked() >= c.Wall {
		return exceeded(BudgetWall)
	}
	return nil
}

// Charge adds n to a counted kind (plan iterations, replans, repair
// attempts, tokens) if it fits the cap; otherwise it returns a
// *BudgetExceededError for that kind and changes nothing. Charge it before
// the model call it pays for. Wall time is not charged (see CanCall);
// BudgetWall or another kind is ErrUnknownBudgetKind, and n < 0 is an error.
func (b *Budget) Charge(kind BudgetKind, n int64) error {
	if n < 0 {
		return errors.New("orchestrator: negative budget charge")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.chargeLocked(kind, n)
}

func (b *Budget) chargeLocked(kind BudgetKind, n int64) error {
	c, u := &b.snap.Caps, &b.snap.Used
	fits := func(used, cap int64) bool { return cap > 0 && used+n <= cap }
	switch kind {
	case BudgetPlanIterations:
		if !fits(int64(u.PlanIterations), int64(c.PlanIterations)) {
			return exceeded(kind)
		}
		u.PlanIterations += int(n)
	case BudgetReplans:
		if !fits(int64(u.Replans), int64(c.Replans)) {
			return exceeded(kind)
		}
		u.Replans += int(n)
	case BudgetRepairAttempts:
		if !fits(int64(u.RepairAttempts), int64(c.RepairAttempts)) {
			return exceeded(kind)
		}
		u.RepairAttempts += int(n)
	case BudgetTokens:
		if !fits(u.Tokens, c.Tokens) {
			return exceeded(kind)
		}
		u.Tokens += n
	default:
		return ErrUnknownBudgetKind
	}
	return nil
}

// StartPlanIteration checks the call budget (wall, tokens with estimate)
// and charges one plan iteration, and one replan when replan is true, all
// or nothing. Call it before every Plan call; the first plan is iteration
// one, so 3 iterations allow 2 replans.
func (b *Budget) StartPlanIteration(replan bool, estimate int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkCallLocked(estimate); err != nil {
		return err
	}
	saved := b.snap.Used
	if err := b.chargeLocked(BudgetPlanIterations, 1); err != nil {
		return err
	}
	if replan {
		if err := b.chargeLocked(BudgetReplans, 1); err != nil {
			b.snap.Used = saved
			return err
		}
	}
	return nil
}

// RepairAllowance is the most repair attempts the next model call may make:
// min(configured, repair attempts left). Pass it as the call's MaxRepair so
// the call can never overspend the repair budget.
func (b *Budget) RepairAllowance(configured int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	left := b.snap.Caps.RepairAttempts - b.snap.Used.RepairAttempts
	return max(min(configured, left), 0)
}

// RecordUsage records what a finished model call spent: every token
// (input, output, cache read and cache write) and its repair attempts
// (attempts after the first). Spend that already happened is always
// recorded, even past a cap, so the next check fails.
func (b *Budget) RecordUsage(u Usage) {
	t := u.Tokens
	n := t.Input + t.Output + t.CacheRead + t.CacheWrite
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 {
		b.snap.Used.Tokens += n
	}
	if u.Attempts > 1 {
		b.snap.Used.RepairAttempts += u.Attempts - 1
	}
}

// Extend applies one "Thêm 1 vòng" extension (O4a) to kind: +1 plan
// iteration, +1 replan, +1 repair attempt, +25% tokens (rounded up, at
// least a quarter of the default), or +ExtendWall wall time. Every
// extension also leaves at least ExtendWall of wall time, so a long wait for
// the owner does not make the extension useless. At most
// MaxBudgetExtensions per execution: then ErrBudgetExtensionLimit.
func (b *Budget) Extend(kind BudgetKind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snap.Extensions >= MaxBudgetExtensions {
		return ErrBudgetExtensionLimit
	}
	c := &b.snap.Caps
	switch kind {
	case BudgetPlanIterations:
		c.PlanIterations = max(c.PlanIterations, b.snap.Used.PlanIterations) + 1
	case BudgetReplans:
		c.Replans = max(c.Replans, b.snap.Used.Replans) + 1
	case BudgetRepairAttempts:
		c.RepairAttempts = max(c.RepairAttempts, b.snap.Used.RepairAttempts) + 1
	case BudgetTokens:
		add := (c.Tokens + 3) / 4
		if c.Tokens <= 0 {
			add = DefaultMaxTokens / 4
		}
		c.Tokens = max(c.Tokens, b.snap.Used.Tokens) + add
	case BudgetWall:
		c.Wall = max(c.Wall, b.elapsedLocked()) + ExtendWall
	default:
		return ErrUnknownBudgetKind
	}
	if floor := b.elapsedLocked() + ExtendWall; c.Wall < floor {
		c.Wall = floor
	}
	b.snap.Extensions++
	return nil
}

// ExtensionsLeft is how many extensions remain.
func (b *Budget) ExtensionsLeft() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(MaxBudgetExtensions-b.snap.Extensions, 0)
}
