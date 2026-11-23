package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const (
	readinessTimeout = 2 * time.Second
	databaseTimeout  = time.Second
)

// ReadinessChecks uses a closed vocabulary so readiness never exposes errors.
type ReadinessChecks struct {
	DB             string `json:"db"`
	Migrations     string `json:"migrations"`
	MasterKey      string `json:"master_key"`
	WorkflowWorker string `json:"workflow_worker"`
	GitSandbox     string `json:"git_sandbox"`
}

// ReadinessReport is the private response served on the loopback ops listener.
type ReadinessReport struct {
	Status string          `json:"status"`
	Checks ReadinessChecks `json:"checks"`
}

// ReadinessDeps are the privacy-safe seams used by readiness checks.
type ReadinessDeps struct {
	PingDB           func(context.Context) error
	MigrationVersion func(context.Context) (version int64, applied bool, err error)
	MasterKeyLoaded  bool
	WorkflowLastPoll func() time.Time
	WorkflowInterval time.Duration
	GitSandboxReady  bool
	Now              func() time.Time
}

// Readiness checks required process dependencies with one overall deadline.
type Readiness struct {
	deps ReadinessDeps
}

// NewReadiness builds a bounded readiness checker.
func NewReadiness(deps ReadinessDeps) *Readiness {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Readiness{deps: deps}
}

type checkResult struct {
	name  string
	value string
}

// Check runs every check concurrently. Required failures make the report not ready.
func (r *Readiness) Check(parent context.Context) ReadinessReport {
	report := ReadinessReport{
		Status: "not_ready",
		Checks: ReadinessChecks{DB: "fail", Migrations: "fail", MasterKey: "fail", WorkflowWorker: "stale", GitSandbox: "unavailable"},
	}
	if r == nil {
		return report
	}
	ctx, cancel := context.WithTimeout(parent, readinessTimeout)
	defer cancel()
	results := make(chan checkResult, 5)
	go r.checkDB(ctx, results)
	go r.checkMigrations(ctx, results)
	go r.checkMasterKey(results)
	go r.checkWorkflow(results)
	go r.checkGitSandbox(results)

	for range 5 {
		select {
		case result := <-results:
			setCheck(&report.Checks, result)
		case <-ctx.Done():
			return report
		}
	}
	if report.Checks.DB == "ok" && report.Checks.Migrations == "ok" && report.Checks.MasterKey == "ok" && report.Checks.WorkflowWorker == "ok" {
		report.Status = "ready"
	}
	return report
}

func (r *Readiness) checkDB(ctx context.Context, results chan<- checkResult) {
	checkCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	value := "fail"
	if r.deps.PingDB != nil && r.deps.PingDB(checkCtx) == nil {
		value = "ok"
	}
	results <- checkResult{name: "db", value: value}
}

func (r *Readiness) checkMigrations(ctx context.Context, results chan<- checkResult) {
	value := "fail"
	if r.deps.MigrationVersion != nil {
		version, applied, err := r.deps.MigrationVersion(ctx)
		if err == nil && applied && version == ExpectedMigrationVersion {
			value = "ok"
		}
	}
	results <- checkResult{name: "migrations", value: value}
}

func (r *Readiness) checkMasterKey(results chan<- checkResult) {
	value := "fail"
	if r.deps.MasterKeyLoaded {
		value = "ok"
	}
	results <- checkResult{name: "master_key", value: value}
}

func (r *Readiness) checkWorkflow(results chan<- checkResult) {
	value := "stale"
	if r.deps.WorkflowLastPoll != nil && r.deps.WorkflowInterval > 0 {
		last := r.deps.WorkflowLastPoll()
		if !last.IsZero() && r.deps.Now().Sub(last) <= 3*r.deps.WorkflowInterval {
			value = "ok"
		}
	}
	results <- checkResult{name: "workflow_worker", value: value}
}

func (r *Readiness) checkGitSandbox(results chan<- checkResult) {
	value := "unavailable"
	if r.deps.GitSandboxReady {
		value = "ready"
	}
	results <- checkResult{name: "git_sandbox", value: value}
}

func setCheck(checks *ReadinessChecks, result checkResult) {
	switch result.name {
	case "db":
		checks.DB = result.value
	case "migrations":
		checks.Migrations = result.value
	case "master_key":
		checks.MasterKey = result.value
	case "workflow_worker":
		checks.WorkflowWorker = result.value
	case "git_sandbox":
		checks.GitSandbox = result.value
	}
}

// Handler returns the loopback-only readiness endpoint.
func (r *Readiness) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		report := r.Check(request.Context())
		w.Header().Set("Content-Type", "application/json")
		if report.Status != "ready" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(report)
	})
}
