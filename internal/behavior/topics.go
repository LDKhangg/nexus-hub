package behavior

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// MaxTopicNameBytes caps a Telegram topic name built by the router.
const MaxTopicNameBytes = 128

// Codes of the Messenger and TopicClient errors the router reacts to. They
// match the Telegram adapter's fixed codes.
const (
	topicCodeNotForum  = "topic_not_forum"
	topicCodeNoRights  = "topic_no_rights"
	topicCodeClosed    = "topic_closed"
	topicCodeNotFound  = "thread_not_found"
	resourceClickUpTsk = "clickup.task"
)

// TopicClient creates and reopens forum topics (the Telegram Messenger).
type TopicClient interface {
	CreateTopic(ctx context.Context, to ChatRef, name string) (threadID int64, err error)
	ReopenTopic(ctx context.Context, to ChatRef, threadID int64) error
}

// TopicNamer returns a task's title for a topic name. The title is used
// once, to create the topic; it is never stored or logged.
type TopicNamer func(ctx context.Context, workspaceID, integrationID, taskID string) (string, error)

// topicDeleter is implemented by a TopicStore that can drop a binding whose
// Telegram topic no longer exists, so the next send creates a new topic.
type topicDeleter interface {
	DeleteTopic(ctx context.Context, workspaceID string, id int64) error
}

// TopicIntent says where one execution's owner messages belong. Mode is
// the behavior's report.topic: task (or empty), workflow, general, or a
// fixed topic name.
type TopicIntent struct {
	Mode         string
	ExecutionID  int64
	TaskID       string // the ClickUp task, when the run has one
	TaskSource   string // integration whose policy reads the task title
	BehaviorName string
}

// TopicRouterDeps are the router's collaborators. Client may be nil (every
// send goes to General); TaskName and Resources are optional.
type TopicRouterDeps struct {
	Store     TopicStore
	Client    TopicClient
	TaskName  TopicNamer
	Resources workflow.ResourceLister
}

// TopicRouter resolves the Telegram topic of an execution's messages and
// falls back to General on any topic failure: a topic step never loses,
// repeats or fails a send.
type TopicRouter struct {
	d         TopicRouterDeps
	fallbacks atomic.Int64
}

func NewTopicRouter(d TopicRouterDeps) (*TopicRouter, error) {
	if d.Store == nil {
		return nil, errors.New("topic router: missing store")
	}
	return &TopicRouter{d: d}, nil
}

// Fallbacks counts the sends that went to General because of a topic step.
func (r *TopicRouter) Fallbacks() int64 { return r.fallbacks.Load() }

func (r *TopicRouter) fallback(code string) {
	r.fallbacks.Add(1)
	log.Printf("telegram topic: sent to General (%s)", code)
}

// subject returns the topic subject of in, or "" for General.
func (r *TopicRouter) subject(ctx context.Context, ws string, in TopicIntent) (kind, key string) {
	mode := strings.TrimSpace(in.Mode)
	switch mode {
	case ReportTopicGeneral:
		return "", ""
	case "", ReportTopicTask, ReportTopicWorkflow:
	default:
		return TopicSubjectFixed, mode
	}
	if mode != ReportTopicWorkflow {
		taskID := in.TaskID
		if taskID == "" && in.ExecutionID > 0 && r.d.Resources != nil {
			if list, err := r.d.Resources.ListResources(ctx, ws, in.ExecutionID); err == nil {
				for _, b := range list {
					if b.Kind == resourceClickUpTsk && b.ExternalID != "" {
						taskID = b.ExternalID
						break
					}
				}
			}
		}
		if taskID != "" && taskIDRe.MatchString(taskID) {
			return TopicSubjectClickUpTask, taskID
		}
	}
	if in.ExecutionID > 0 {
		return TopicSubjectWorkflow, strconv.FormatInt(in.ExecutionID, 10)
	}
	return "", ""
}

// Destination returns base with the ThreadID of the topic for in, creating
// or reopening the topic on demand. On any failure it returns base itself
// (General).
func (r *TopicRouter) Destination(ctx context.Context, base ChatRef, in TopicIntent) ChatRef {
	base.topic = nil
	if r == nil || base.ChatID == 0 || base.ThreadID != 0 {
		return base
	}
	kind, key := r.subject(ctx, base.WorkspaceID, in)
	if kind == "" {
		return base
	}
	topic, claimed, err := r.d.Store.EnsureTopic(ctx, TopicKey{
		WorkspaceID: base.WorkspaceID, IntegrationID: base.IntegrationID, ChatID: base.ChatID, SubjectKind: kind, SubjectKey: key,
	})
	if err != nil {
		r.fallback("topic_store_failed")
		return base
	}
	if claimed {
		return r.create(ctx, base, topic, kind, key, in)
	}
	if topic.ThreadID <= 0 {
		// Another sender is still creating it; this message goes to General.
		r.fallback("topic_pending")
		return base
	}
	if topic.State == TopicClosed {
		if r.d.Client == nil {
			r.fallback("topic_closed")
			return base
		}
		if err := r.d.Client.ReopenTopic(ctx, base, topic.ThreadID); err != nil {
			if sendCode(err) == topicCodeNotFound {
				r.drop(ctx, base, topic)
			}
			r.fallback(sendCode(err))
			return base
		}
		if _, err := r.d.Store.SetTopicState(ctx, base.WorkspaceID, topic.ID, TopicOpen); err != nil {
			log.Print("telegram topic: reopened, state not stored")
		}
	}
	base.ThreadID = topic.ThreadID
	return base
}

// create makes the Telegram topic for a claimed reservation.
func (r *TopicRouter) create(ctx context.Context, base ChatRef, topic Topic, kind, key string, in TopicIntent) ChatRef {
	release := func() {
		if err := r.d.Store.ReleaseTopic(context.WithoutCancel(ctx), base.WorkspaceID, topic.ID); err != nil {
			log.Print("telegram topic: reservation not released")
		}
	}
	if r.d.Client == nil {
		release()
		r.fallback("topic_unavailable")
		return base
	}
	threadID, err := r.d.Client.CreateTopic(ctx, base, r.name(ctx, base, kind, key, in))
	if err != nil || threadID <= 0 {
		release()
		code := "topic_create_failed"
		if err != nil {
			code = sendCode(err)
		}
		r.fallback(code)
		return base
	}
	if _, err := r.d.Store.SetTopicThread(context.WithoutCancel(ctx), base.WorkspaceID, topic.ID, threadID); err != nil {
		// The topic exists in Telegram; use it for this message. The
		// binding is retried by a later send once the lease runs out.
		log.Print("telegram topic: created, thread id not stored")
	}
	base.ThreadID = threadID
	return base
}

// name builds the topic name. It is sanitized, never stored and never
// logged.
func (r *TopicRouter) name(ctx context.Context, base ChatRef, kind, key string, in TopicIntent) string {
	switch kind {
	case TopicSubjectFixed:
		return sanitizeTopicName(key, MaxTopicNameBytes)
	case TopicSubjectClickUpTask:
		if r.d.TaskName != nil {
			src := in.TaskSource
			if title, err := r.d.TaskName(ctx, base.WorkspaceID, src, key); err == nil {
				if t := sanitizeTopicName(title, MaxTopicNameBytes); t != "" {
					return sanitizeTopicName(key+" · "+t, MaxTopicNameBytes)
				}
			}
		}
		return sanitizeTopicName(key, MaxTopicNameBytes)
	}
	name := strings.TrimSpace(in.BehaviorName)
	if name == "" {
		return sanitizeTopicName("WF-"+key, MaxTopicNameBytes)
	}
	return sanitizeTopicName("WF-"+key+" · "+name, MaxTopicNameBytes)
}

// sanitizeTopicName replaces control characters, collapses blanks and cuts
// to max bytes on a rune boundary.
func sanitizeTopicName(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s = strings.TrimSpace(s)
	}
	return s
}

// Recover reacts to a failed send to a topic. When it returns ok, the
// caller sends the same message once more to the returned chat (the send
// was rejected, so nothing was delivered); otherwise the error stands.
func (r *TopicRouter) Recover(ctx context.Context, chat ChatRef, err error) (ChatRef, bool) {
	if r == nil || chat.ThreadID == 0 || err == nil {
		return chat, false
	}
	general := chat
	general.ThreadID, general.topic = 0, nil
	switch code := sendCode(err); code {
	case topicCodeNotFound:
		if topic, gerr := r.d.Store.TopicByThread(ctx, chat.WorkspaceID, chat.IntegrationID, chat.ChatID, chat.ThreadID); gerr == nil {
			r.drop(ctx, chat, topic)
		}
		r.fallback(code)
		return general, true
	case topicCodeClosed:
		if r.d.Client != nil && r.d.Client.ReopenTopic(ctx, chat, chat.ThreadID) == nil {
			if topic, gerr := r.d.Store.TopicByThread(ctx, chat.WorkspaceID, chat.IntegrationID, chat.ChatID, chat.ThreadID); gerr == nil {
				_, _ = r.d.Store.SetTopicState(ctx, chat.WorkspaceID, topic.ID, TopicOpen)
			}
			chat.topic = nil
			return chat, true
		}
		r.fallback(code)
		return general, true
	case topicCodeNotForum, topicCodeNoRights:
		r.fallback(code)
		return general, true
	}
	return chat, false
}

// drop forgets a topic whose thread no longer exists in Telegram, so the
// next send creates a new one; a store that cannot delete marks it closed.
func (r *TopicRouter) drop(ctx context.Context, chat ChatRef, topic Topic) {
	ctx = context.WithoutCancel(ctx)
	if d, ok := r.d.Store.(topicDeleter); ok {
		if err := d.DeleteTopic(ctx, chat.WorkspaceID, topic.ID); err == nil {
			return
		}
	}
	if _, err := r.d.Store.SetTopicState(ctx, chat.WorkspaceID, topic.ID, TopicClosed); err != nil {
		log.Print("telegram topic: lost topic not marked")
	}
}

// ActionThread returns the topic thread of an agent report (an outbound
// action of an execution), or 0 for General. It satisfies the relay's
// thread router.
func (r *TopicRouter) ActionThread(ctx context.Context, workspaceID, integrationID string, chatID int64, executionID int64) int64 {
	if r == nil || executionID <= 0 {
		return 0
	}
	base := ChatRef{WorkspaceID: workspaceID, IntegrationID: integrationID, ChatID: chatID}
	return r.Destination(ctx, base, TopicIntent{Mode: ReportTopicTask, ExecutionID: executionID, TaskSource: integrationID, BehaviorName: "agent"}).ThreadID
}

// ActionThreadRejected handles a rejected agent report send: it returns the
// thread to resend to (0: General) and whether to resend once.
func (r *TopicRouter) ActionThreadRejected(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64, err error) (int64, bool) {
	to, ok := r.Recover(ctx, ChatRef{WorkspaceID: workspaceID, IntegrationID: integrationID, ChatID: chatID, ThreadID: threadID}, err)
	return to.ThreadID, ok
}
