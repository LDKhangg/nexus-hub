package behavior

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// DefaultTopicCloseStatuses are the ClickUp status names that count as done
// when an event or read carries no status type.
const DefaultTopicCloseStatuses = "complete,closed,done"

const (
	topicSweepInterval = 30 * time.Second
	// topicReadEvery is the least wait before the same task is read again to
	// learn whether it is done.
	topicReadEvery = 30 * time.Minute
	// topicReadsPerSweep bounds the ClickUp reads of one sweep.
	topicReadsPerSweep = 5
	topicCloseTimeout  = 20 * time.Second
	topicLifecycleCap  = 5000
)

// TopicCloser closes a forum topic (the Telegram Messenger).
type TopicCloser interface {
	CloseTopic(ctx context.Context, to ChatRef, threadID int64) error
}

// TopicLifecycleDeps are the lifecycle's collaborators.
type TopicLifecycleDeps struct {
	Store  TopicStore
	Closer TopicCloser
	// Workspaces lists every workspace id.
	Workspaces func(ctx context.Context) ([]string, error)
	// Executions finds the executions bound to a ClickUp task; Reader reads
	// their state. Without both, no topic is closed (activity is unknown).
	Executions interface {
		ExecutionsByResource(ctx context.Context, workspaceID, kind, externalID string) ([]int64, error)
	}
	Reader interface {
		GetExecutionDetail(ctx context.Context, workspaceID string, executionID int64) (workflow.ExecutionDetail, error)
	}
	// ReadTask reads the task's current status; optional.
	ReadTask func(ctx context.Context, workspaceID, taskID string) (status, statusType string, err error)
	// CloseStatuses is the comma-separated topic_close_statuses setting.
	CloseStatuses string
	Interval      time.Duration
}

type lifecycleKey struct{ ws, task string }

type taskSeen struct {
	done bool
	at   time.Time
}

// TopicLifecycle closes the topic of a ClickUp task that is done or closed
// when no execution bound to the task is active, and never deletes one. A
// closed topic is reopened by the next send (TopicRouter). It learns that a
// task is done from ClickUp status events (StatusChanged) or, when the last
// execution ends without one, from a task read. It logs metadata only.
type TopicLifecycle struct {
	d      TopicLifecycleDeps
	closes map[string]bool
	now    func() time.Time

	mu   sync.Mutex
	seen map[lifecycleKey]taskSeen
	bg   sync.WaitGroup
}

// NewTopicLifecycle returns a lifecycle; Store and Closer are required.
func NewTopicLifecycle(d TopicLifecycleDeps) (*TopicLifecycle, error) {
	if d.Store == nil || d.Closer == nil {
		return nil, errors.New("topic lifecycle: missing store or closer")
	}
	if d.Interval <= 0 {
		d.Interval = topicSweepInterval
	}
	if strings.TrimSpace(d.CloseStatuses) == "" {
		d.CloseStatuses = DefaultTopicCloseStatuses
	}
	l := &TopicLifecycle{d: d, closes: map[string]bool{}, now: time.Now, seen: map[lifecycleKey]taskSeen{}}
	for _, name := range strings.Split(d.CloseStatuses, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			l.closes[name] = true
		}
	}
	return l, nil
}

// isDone reports whether a task status counts as done/closed: by the status
// type when ClickUp sent one, else by name against topic_close_statuses.
func (l *TopicLifecycle) isDone(status, statusType string) bool {
	if t := strings.ToLower(strings.TrimSpace(statusType)); t != "" {
		return t == "done" || t == "closed"
	}
	return l.closes[strings.ToLower(strings.TrimSpace(status))]
}

func (l *TopicLifecycle) remember(k lifecycleKey, done bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) >= topicLifecycleCap {
		l.seen = map[lifecycleKey]taskSeen{}
	}
	l.seen[k] = taskSeen{done: done, at: l.now()}
}

func (l *TopicLifecycle) lookup(k lifecycleKey) (taskSeen, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.seen[k]
	return s, ok
}

// StatusChanged handles a ClickUp status event: it remembers whether the
// task is done and, when it is, closes the task's topic (in the background)
// unless an execution of the task is still active.
func (l *TopicLifecycle) StatusChanged(workspaceID, taskID, status, statusType string) {
	if l == nil || workspaceID == "" || !taskIDRe.MatchString(taskID) {
		return
	}
	done := l.isDone(status, statusType)
	l.remember(lifecycleKey{workspaceID, taskID}, done)
	if !done {
		return
	}
	l.bg.Add(1)
	go func() {
		defer l.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), topicCloseTimeout)
		defer cancel()
		l.closeTask(ctx, workspaceID, taskID)
	}()
}

// Run sweeps every Interval until ctx ends: the last active execution of a
// done task has ended, so its topic closes then.
func (l *TopicLifecycle) Run(ctx context.Context) error {
	defer l.bg.Wait()
	t := time.NewTicker(l.d.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.Sweep(ctx)
		}
	}
}

// Sweep checks every open task topic once.
func (l *TopicLifecycle) Sweep(ctx context.Context) {
	if l == nil || l.d.Workspaces == nil {
		return
	}
	workspaces, err := l.d.Workspaces(ctx)
	if err != nil {
		log.Print("telegram topic lifecycle: workspaces not listed")
		return
	}
	reads := 0
	for _, ws := range workspaces {
		topics, err := l.d.Store.ListTopics(ctx, ws)
		if err != nil {
			log.Print("telegram topic lifecycle: topics not listed")
			continue
		}
		tasks := map[string]bool{}
		for _, t := range topics {
			if t.SubjectKind == TopicSubjectClickUpTask && t.State == TopicOpen && t.ThreadID > 0 && taskIDRe.MatchString(t.SubjectKey) {
				tasks[t.SubjectKey] = true
			}
		}
		for task := range tasks {
			if ctx.Err() != nil {
				return
			}
			if active, certain := l.hasActive(ctx, ws, task); active || !certain {
				continue
			}
			k := lifecycleKey{ws, task}
			s, known := l.lookup(k)
			if !known || (!s.done && l.now().Sub(s.at) >= topicReadEvery) {
				if l.d.ReadTask == nil || reads >= topicReadsPerSweep {
					continue
				}
				reads++
				rctx, cancel := context.WithTimeout(ctx, taskNameTimeout)
				status, statusType, err := l.d.ReadTask(rctx, ws, task)
				cancel()
				if err != nil {
					l.remember(k, false)
					continue
				}
				s = taskSeen{done: l.isDone(status, statusType)}
				l.remember(k, s.done)
			}
			if s.done {
				cctx, cancel := context.WithTimeout(ctx, topicCloseTimeout)
				l.closeTask(cctx, ws, task)
				cancel()
			}
		}
	}
}

// hasActive reports whether an execution bound to the task is not terminal.
// certain is false when activity could not be determined.
func (l *TopicLifecycle) hasActive(ctx context.Context, ws, taskID string) (active, certain bool) {
	if l.d.Executions == nil || l.d.Reader == nil {
		return false, false
	}
	ids, err := l.d.Executions.ExecutionsByResource(ctx, ws, resourceClickUpTsk, taskID)
	if err != nil {
		return false, false
	}
	if len(ids) > topicLookupLimit*4 {
		return false, false
	}
	for _, id := range ids {
		d, err := l.d.Reader.GetExecutionDetail(ctx, ws, id)
		if err != nil {
			if errors.Is(err, workflow.ErrNotFound) {
				continue
			}
			return false, false
		}
		if !d.State.IsTerminal() {
			return true, true
		}
	}
	return false, true
}

// closeTask closes the open topics of the task in the workspace when no
// execution of it is active.
func (l *TopicLifecycle) closeTask(ctx context.Context, ws, taskID string) {
	if active, certain := l.hasActive(ctx, ws, taskID); active || !certain {
		return
	}
	topics, err := l.d.Store.ListTopics(ctx, ws)
	if err != nil {
		log.Print("telegram topic lifecycle: topics not listed")
		return
	}
	for _, t := range topics {
		if t.SubjectKind != TopicSubjectClickUpTask || t.SubjectKey != taskID || t.State != TopicOpen || t.ThreadID <= 0 {
			continue
		}
		err := l.d.Closer.CloseTopic(ctx, ChatRef{WorkspaceID: ws, IntegrationID: t.IntegrationID, ChatID: t.ChatID}, t.ThreadID)
		if err != nil {
			// A topic that is gone or already closed needs no more tries.
			code := sendCode(err)
			if code != topicCodeNotFound && code != topicCodeClosed {
				log.Printf("telegram topic lifecycle: close failed (%s)", code)
				continue
			}
		}
		if _, err := l.d.Store.SetTopicState(context.WithoutCancel(ctx), ws, t.ID, TopicClosed); err != nil {
			log.Print("telegram topic lifecycle: closed, state not stored")
			continue
		}
		log.Printf("telegram topic lifecycle: closed topic %d (task_done)", t.ID)
	}
}
