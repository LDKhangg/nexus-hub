package storage

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file holds the BehaviorStore parts for approvals, inbound Telegram
// updates, the profile and decision memory.

var (
	approvalNonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	updateNoteRe    = regexp.MustCompile(`^[a-z_]{0,32}$`)
)

// --- approvals ---

func mapApproval(a sqlc.Approval) behavior.Approval {
	return behavior.Approval{
		ID: a.ID, WorkspaceID: uuidString(a.WorkspaceID), Kind: behavior.ApprovalKind(a.Kind), RunID: a.RunID.Int64,
		ItemID: a.ItemID.Int64, Action: a.Action, Resource: a.Resource, Payload: a.Payload, Options: a.Options,
		Nonce: a.Nonce, ExpiresAt: a.ExpiresAt.Time, Decision: behavior.Decision(a.Decision.String),
		Answer: a.Answer.String, DecidedBy: a.DecidedBy.String, DecidedAt: tsPtr(a.DecidedAt),
		TelegramMessageID: a.TelegramMessageID.Int64, ReplyMessageID: a.ReplyMessageID.Int64,
		PendingAnswer: a.PendingAnswer.String, CreatedAt: a.CreatedAt.Time,
	}
}

func (s *BehaviorStore) CreateApproval(ctx context.Context, workspaceID string, a behavior.NewApproval) (behavior.Approval, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Approval{}, err
	}
	switch a.Kind {
	case behavior.ApprovalItemAnswer, behavior.ApprovalPostDraft, behavior.ApprovalConfirmAction,
		behavior.ApprovalFeedback, behavior.ApprovalFeedbackReason, behavior.ApprovalContactAlias,
		behavior.ApprovalAssistantAction:
	default:
		return behavior.Approval{}, invalid("kind")
	}
	switch {
	case !approvalNonceRe.MatchString(a.Nonce):
		return behavior.Approval{}, invalid("nonce")
	case a.ExpiresAt.IsZero():
		return behavior.Approval{}, invalid("expires_at")
	case a.ItemID != 0 && a.RunID == 0:
		return behavior.Approval{}, invalid("run_id")
	}
	payload, err := objectJSON(a.Payload, "payload")
	if err != nil {
		return behavior.Approval{}, err
	}
	options, err := jsonKind(a.Options, "[]", '[', "options")
	if err != nil {
		return behavior.Approval{}, err
	}
	row, err := s.q.CreateApproval(ctx, sqlc.CreateApprovalParams{
		WorkspaceID: ws, Kind: string(a.Kind), RunID: optInt8(a.RunID), ItemID: optInt8(a.ItemID), Action: a.Action,
		Resource: a.Resource, Payload: payload, Options: options, Nonce: a.Nonce, ExpiresAt: ts(a.ExpiresAt),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return behavior.Approval{}, behavior.ErrConflict
		}
		return behavior.Approval{}, err
	}
	return mapApproval(row), nil
}

func (s *BehaviorStore) GetApproval(ctx context.Context, workspaceID string, id int64) (behavior.Approval, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Approval{}, err
	}
	row, err := s.q.GetApproval(ctx, sqlc.GetApprovalParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return behavior.Approval{}, notFound(err)
	}
	return mapApproval(row), nil
}

func (s *BehaviorStore) GetApprovalByNonce(ctx context.Context, workspaceID, nonce string) (behavior.Approval, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Approval{}, err
	}
	row, err := s.q.GetApprovalByNonce(ctx, sqlc.GetApprovalByNonceParams{WorkspaceID: ws, Nonce: nonce})
	if err != nil {
		return behavior.Approval{}, notFound(err)
	}
	return mapApproval(row), nil
}

func (s *BehaviorStore) Decide(ctx context.Context, workspaceID, nonce string, d behavior.Decision, answer, actor string, now time.Time) (behavior.Approval, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Approval{}, err
	}
	switch d {
	case behavior.DecisionApproved, behavior.DecisionRejected, behavior.DecisionAnswered:
	default:
		return behavior.Approval{}, invalid("decision")
	}
	if strings.TrimSpace(actor) == "" {
		return behavior.Approval{}, invalid("actor")
	}
	if utf8.RuneCountInString(answer) > 4000 {
		return behavior.Approval{}, invalid("answer")
	}
	row, err := s.q.DecideApproval(ctx, sqlc.DecideApprovalParams{
		Decision: string(d), Answer: optText(answer), DecidedBy: actor, WorkspaceID: ws, Nonce: nonce, AtTime: ts(now),
	})
	if err == nil {
		return mapApproval(row), nil
	}
	if !isNoRows(err) {
		return behavior.Approval{}, err
	}
	cur, err := s.q.GetApprovalByNonce(ctx, sqlc.GetApprovalByNonceParams{WorkspaceID: ws, Nonce: nonce})
	if err != nil {
		return behavior.Approval{}, notFound(err)
	}
	if cur.Decision.Valid && cur.Decision.String != string(behavior.DecisionExpired) {
		return behavior.Approval{}, behavior.ErrDecided
	}
	return behavior.Approval{}, behavior.ErrExpired
}

func (s *BehaviorStore) ExpireDue(ctx context.Context, now time.Time, limit int) ([]behavior.Approval, error) {
	rows, err := s.q.ExpireApprovals(ctx, sqlc.ExpireApprovalsParams{AtTime: ts(now), MaxRows: int32(clamp(limit, 1, 1000))})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Approval, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapApproval(r))
	}
	return out, nil
}

func (s *BehaviorStore) SetApprovalMessage(ctx context.Context, workspaceID string, id, messageID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	n, err := s.q.SetApprovalMessage(ctx, sqlc.SetApprovalMessageParams{MessageID: messageID, WorkspaceID: ws, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrNotFound
	}
	return nil
}

func (s *BehaviorStore) SetReplyMessage(ctx context.Context, workspaceID string, id, messageID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	if messageID <= 0 {
		return invalid("message_id")
	}
	n, err := s.q.SetApprovalReplyMessage(ctx, sqlc.SetApprovalReplyMessageParams{MessageID: messageID, WorkspaceID: ws, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrNotFound
	}
	return nil
}

func (s *BehaviorStore) StageAnswer(ctx context.Context, workspaceID string, messageID int64, answer string, now time.Time) (string, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return "", err
	}
	if messageID <= 0 {
		return "", invalid("message_id")
	}
	if strings.TrimSpace(answer) == "" || utf8.RuneCountInString(answer) > 4000 {
		return "", invalid("answer")
	}
	nonce, err := s.q.StageApprovalAnswer(ctx, sqlc.StageApprovalAnswerParams{Answer: answer, WorkspaceID: ws, MessageID: messageID, AtTime: ts(now)})
	if err != nil {
		return "", notFound(err)
	}
	return nonce, nil
}

func (s *BehaviorStore) ClearPendingAnswer(ctx context.Context, workspaceID string, id int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	_, err = s.q.ClearApprovalPendingAnswer(ctx, sqlc.ClearApprovalPendingAnswerParams{WorkspaceID: ws, ID: id})
	return err
}

func (s *BehaviorStore) ListApprovals(ctx context.Context, workspaceID string, runID int64) ([]behavior.Approval, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListApprovalsByRun(ctx, sqlc.ListApprovalsByRunParams{WorkspaceID: ws, RunID: optInt8(runID)})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Approval, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapApproval(r))
	}
	return out, nil
}

// --- inbound Telegram updates ---

// RecordUpdate stores the update once. Command and Args must already be
// capped (64 and 512 characters); longer values are ErrInvalid, never
// silently cut.
func (s *BehaviorStore) RecordUpdate(ctx context.Context, workspaceID string, u behavior.InboundUpdate) (bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return false, err
	}
	switch u.Kind {
	case behavior.UpdateCommand, behavior.UpdateCallback, behavior.UpdateText, behavior.UpdateOther:
	default:
		return false, invalid("kind")
	}
	if utf8.RuneCountInString(u.Command) > 64 || utf8.RuneCountInString(u.Args) > 512 {
		return false, invalid("args")
	}
	if !updateNoteRe.MatchString(u.Note) {
		return false, invalid("note")
	}
	var integration pgtype.UUID
	if u.IntegrationID != "" {
		if integration, err = parseDBID(u.IntegrationID); err != nil {
			return false, err
		}
	}
	_, err = s.q.InsertTelegramUpdate(ctx, sqlc.InsertTelegramUpdateParams{
		WorkspaceID: ws, UpdateID: u.UpdateID, ChatID: u.ChatID, Kind: string(u.Kind), Command: u.Command, Args: u.Args,
		IntegrationID: integration, Note: u.Note,
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *BehaviorStore) ClaimUpdate(ctx context.Context, now time.Time, lease time.Duration) (*behavior.InboundUpdate, error) {
	if lease <= 0 {
		return nil, invalid("lease")
	}
	tok, token, err := newToken()
	if err != nil {
		return nil, err
	}
	row, err := s.q.ClaimTelegramUpdate(ctx, sqlc.ClaimTelegramUpdateParams{Token: token, AtTime: ts(now), LeaseSecs: lease.Seconds()})
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &behavior.InboundUpdate{
		ID: row.ID, WorkspaceID: uuidString(row.WorkspaceID), IntegrationID: uuidString(row.IntegrationID),
		UpdateID: row.UpdateID, ChatID: row.ChatID, Kind: behavior.UpdateKind(row.Kind), Command: row.Command,
		Args: row.Args, Note: row.Note, Attempts: int(row.Attempts),
		ClaimToken: tok, ReceivedAt: row.ReceivedAt.Time,
	}, nil
}

func (s *BehaviorStore) FinishUpdate(ctx context.Context, id int64, token, code string) error {
	t, err := claimToken(token)
	if err != nil {
		return err
	}
	n, err := s.q.FinishTelegramUpdate(ctx, sqlc.FinishTelegramUpdateParams{Code: optText(code), ID: id, Token: t})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrStaleClaim
	}
	return nil
}

// --- profile ---

func (s *BehaviorStore) GetProfile(ctx context.Context, workspaceID string) (behavior.Profile, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Profile{}, err
	}
	row, err := s.q.GetUserProfile(ctx, ws)
	if isNoRows(err) {
		return behavior.DefaultProfile(), nil
	}
	if err != nil {
		return behavior.Profile{}, err
	}
	return mapProfile(row), nil
}

func mapProfile(r sqlc.UserProfile) behavior.Profile {
	return behavior.Profile{
		Language: r.Language, Tone: r.Tone, QuietStart: intPtr(r.QuietStartMinute), QuietEnd: intPtr(r.QuietEndMinute),
		Timezone: r.Timezone, DigestMode: behavior.DigestMode(r.DigestMode), DefaultModel: r.DefaultModel,
		Budgets: r.Budgets, UpdatedAt: r.UpdatedAt.Time,
	}
}

func (s *BehaviorStore) SaveProfile(ctx context.Context, workspaceID string, p behavior.Profile) (behavior.Profile, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Profile{}, err
	}
	if err := p.Validate(); err != nil {
		return behavior.Profile{}, err
	}
	budgets, err := objectJSON(p.Budgets, "budgets")
	if err != nil {
		return behavior.Profile{}, err
	}
	row, err := s.q.UpsertUserProfile(ctx, sqlc.UpsertUserProfileParams{
		WorkspaceID: ws, Language: p.Language, Tone: p.Tone, QuietStartMinute: optInt4(p.QuietStart),
		QuietEndMinute: optInt4(p.QuietEnd), Timezone: p.Timezone, DigestMode: string(p.DigestMode),
		DefaultModel: p.DefaultModel, Budgets: budgets,
	})
	if err != nil {
		return behavior.Profile{}, err
	}
	return mapProfile(row), nil
}

// --- decision memory ---

func mapDecision(r sqlc.DecisionMemory, rank float64) behavior.RememberedDecision {
	var scope behavior.DecisionScope
	_ = json.Unmarshal(r.Scope, &scope) // the column is always an object
	return behavior.RememberedDecision{
		ID: r.ID, Topic: r.Topic, Question: r.Question, Answer: r.Answer, Scope: scope,
		Source: behavior.DecisionSource(r.Source), RunID: r.RunID.Int64, ItemID: r.ItemID.Int64, Rank: rank,
		CreatedAt: r.CreatedAt.Time, ForgottenAt: tsPtr(r.ForgottenAt),
	}
}

func (s *BehaviorStore) AddDecision(ctx context.Context, workspaceID string, d behavior.NewDecision) (behavior.RememberedDecision, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.RememberedDecision{}, err
	}
	switch d.Source {
	case behavior.SourceAskAnswer, behavior.SourceAutoPosted, behavior.SourceAutoUndone:
	default:
		return behavior.RememberedDecision{}, invalid("source")
	}
	switch {
	case utf8.RuneCountInString(d.Topic) > 200:
		return behavior.RememberedDecision{}, invalid("topic")
	case strings.TrimSpace(d.Question) == "" || utf8.RuneCountInString(d.Question) > 4000:
		return behavior.RememberedDecision{}, invalid("question")
	case strings.TrimSpace(d.Answer) == "" || utf8.RuneCountInString(d.Answer) > 4000:
		return behavior.RememberedDecision{}, invalid("answer")
	case d.ItemID != 0 && d.RunID == 0:
		return behavior.RememberedDecision{}, invalid("run_id")
	}
	scope, err := json.Marshal(d.Scope)
	if err != nil {
		return behavior.RememberedDecision{}, err
	}
	row, err := s.q.InsertDecision(ctx, sqlc.InsertDecisionParams{
		WorkspaceID: ws, Topic: d.Topic, Question: d.Question, Answer: d.Answer, Scope: scope, Source: string(d.Source),
		RunID: optInt8(d.RunID), ItemID: optInt8(d.ItemID),
	})
	if err != nil {
		return behavior.RememberedDecision{}, err
	}
	return mapDecision(row, 0), nil
}

func (s *BehaviorStore) GetDecision(ctx context.Context, workspaceID string, id int64) (behavior.RememberedDecision, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.RememberedDecision{}, err
	}
	row, err := s.q.GetDecision(ctx, sqlc.GetDecisionParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return behavior.RememberedDecision{}, notFound(err)
	}
	return mapDecision(row, 0), nil
}

// SearchDecisions returns at most limit (1..50) live decisions; a blank
// query returns none.
func (s *BehaviorStore) SearchDecisions(ctx context.Context, workspaceID, query, behaviorName string, limit int) ([]behavior.RememberedDecision, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(query) > 1000 {
		return nil, invalid("query")
	}
	rows, err := s.q.SearchDecisions(ctx, sqlc.SearchDecisionsParams{Query: query, WorkspaceID: ws, Behavior: behaviorName, MaxRows: int32(clamp(limit, 1, 50))})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.RememberedDecision, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapDecision(sqlc.DecisionMemory{
			ID: r.ID, WorkspaceID: r.WorkspaceID, Topic: r.Topic, Question: r.Question, Answer: r.Answer, Scope: r.Scope,
			Source: r.Source, RunID: r.RunID, ItemID: r.ItemID, CreatedAt: r.CreatedAt, ForgottenAt: r.ForgottenAt,
		}, r.Rank))
	}
	return out, nil
}

// SearchReusableDecisions is SearchDecisions over every scope for reuse as
// evidence: undone decisions, and decisions about an item whose auto write
// was undone, are left out.
func (s *BehaviorStore) SearchReusableDecisions(ctx context.Context, workspaceID, query string, limit int) ([]behavior.RememberedDecision, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(query) > 1000 {
		return nil, invalid("query")
	}
	rows, err := s.q.SearchReusableDecisions(ctx, sqlc.SearchReusableDecisionsParams{Query: query, WorkspaceID: ws, MaxRows: int32(clamp(limit, 1, 50))})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.RememberedDecision, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapDecision(sqlc.DecisionMemory{
			ID: r.ID, WorkspaceID: r.WorkspaceID, Topic: r.Topic, Question: r.Question, Answer: r.Answer, Scope: r.Scope,
			Source: r.Source, RunID: r.RunID, ItemID: r.ItemID, CreatedAt: r.CreatedAt, ForgottenAt: r.ForgottenAt,
		}, r.Rank))
	}
	return out, nil
}

// ListDecisions pages decisions newest first; limit is 1..100.
func (s *BehaviorStore) ListDecisions(ctx context.Context, workspaceID string, includeForgotten bool, limit, offset int) ([]behavior.RememberedDecision, error) {
	return s.ListDecisionsFor(ctx, workspaceID, "", includeForgotten, limit, offset)
}

// ListDecisionsFor pages the decisions of one behavior scope (every scope
// when behaviorName is empty) newest first; limit is 1..100.
func (s *BehaviorStore) ListDecisionsFor(ctx context.Context, workspaceID, behaviorName string, includeForgotten bool, limit, offset int) ([]behavior.RememberedDecision, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	if err := validatePage(int32(limit), int32(offset)); err != nil {
		return nil, invalid("page")
	}
	rows, err := s.q.ListDecisions(ctx, sqlc.ListDecisionsParams{WorkspaceID: ws, IncludeForgotten: includeForgotten, Behavior: behaviorName, MaxRows: int32(limit), Skip: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.RememberedDecision, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapDecision(r, 0))
	}
	return out, nil
}

func (s *BehaviorStore) ForgetDecision(ctx context.Context, workspaceID string, id int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	n, err := s.q.ForgetDecision(ctx, sqlc.ForgetDecisionParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrNotFound
	}
	return nil
}
