package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConversationStore persists chat metadata, workflow links and explicit
// instructions. It never accepts or stores raw message bodies.
type ConversationStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewConversationStore(pool *pgxpool.Pool) (*ConversationStore, error) {
	if pool == nil {
		return nil, errors.New("conversation store requires pool")
	}
	return &ConversationStore{pool: pool, q: sqlc.New(pool)}, nil
}

var _ workflow.ConversationStore = (*ConversationStore)(nil)

func conversationError(err error) error {
	if isNoRows(err) {
		return workflow.ErrNotFound
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23503":
		return workflow.ErrNotFound
	case "23505":
		return workflow.ErrConflict
	case "22001", "23502", "23514":
		return workflow.ErrInvalidInput
	default:
		return err
	}
}

func optionalUUID(value, field string) (pgtype.UUID, error) {
	if value == "" {
		return pgtype.UUID{}, nil
	}
	id, err := parseDBID(value)
	if err != nil {
		return pgtype.UUID{}, workflowInvalid(field)
	}
	return id, nil
}

func mapConversation(row sqlc.Conversation) workflow.Conversation {
	return workflow.Conversation{
		ID: row.ID, WorkspaceID: uuidString(row.WorkspaceID), IntegrationID: uuidString(row.IntegrationID),
		ChatID: row.ChatID, UserID: uuidString(row.UserID), Summary: row.Summary,
		SummaryUpdatedAt: tsPtr(row.SummaryUpdatedAt), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func mapConversationMessage(row sqlc.ConversationMessage) workflow.Message {
	return workflow.Message{
		ID: row.ID, WorkspaceID: uuidString(row.WorkspaceID), ConversationID: row.ConversationID,
		Direction: row.Direction, Kind: row.Kind, TelegramMessageID: row.TelegramMessageID,
		ReplyToMessageID: row.ReplyToMessageID.Int64, ExecutionID: row.ExecutionID.Int64,
		CreatedAt: row.CreatedAt.Time,
	}
}

func mapInstruction(row sqlc.WorkflowInstruction) workflow.Instruction {
	return workflow.Instruction{
		WorkspaceID: uuidString(row.WorkspaceID), ExecutionID: row.ExecutionID, Version: row.Version,
		Text: row.Text, ConversationID: row.ConversationID, TelegramUpdateID: row.TelegramUpdateID,
		CreatedAt: row.CreatedAt.Time,
	}
}

func sanitizeText(value string, maxRunes int) string {
	value = strings.ToValidUTF8(value, "")
	capacity := utf8.RuneCountInString(value)
	if maxRunes > 0 {
		capacity = min(capacity, maxRunes)
	}
	clean := make([]rune, 0, capacity)
	for _, r := range value {
		if unicode.IsControl(r) {
			continue
		}
		if maxRunes > 0 && len(clean) == maxRunes {
			break
		}
		clean = append(clean, r)
	}
	return string(clean)
}

func (s *ConversationStore) EnsureConversation(ctx context.Context, conversation workflow.Conversation) (workflow.Conversation, error) {
	ws, err := readWorkspace(conversation.WorkspaceID)
	if err != nil {
		return workflow.Conversation{}, err
	}
	integration, err := parseDBID(conversation.IntegrationID)
	if err != nil {
		return workflow.Conversation{}, workflowInvalid("integration_id")
	}
	user, err := optionalUUID(conversation.UserID, "user_id")
	if err != nil {
		return workflow.Conversation{}, err
	}
	if conversation.ChatID == 0 {
		return workflow.Conversation{}, workflowInvalid("chat_id")
	}
	row, err := s.q.EnsureConversation(ctx, sqlc.EnsureConversationParams{
		WorkspaceID: ws, IntegrationID: integration, ChatID: conversation.ChatID, UserID: user,
	})
	if err != nil {
		return workflow.Conversation{}, conversationError(err)
	}
	return mapConversation(row), nil
}

func (s *ConversationStore) RecordMessage(ctx context.Context, message workflow.Message) (workflow.Message, error) {
	ws, err := readWorkspace(message.WorkspaceID)
	if err != nil {
		return workflow.Message{}, err
	}
	if message.ConversationID <= 0 || message.TelegramMessageID <= 0 ||
		(message.Direction != "in" && message.Direction != "out") ||
		strings.TrimSpace(message.Kind) == "" || len(message.Kind) > 64 ||
		message.ReplyToMessageID < 0 || message.ExecutionID < 0 {
		return workflow.Message{}, workflowInvalid("message metadata")
	}
	row, err := s.q.InsertConversationMessage(ctx, sqlc.InsertConversationMessageParams{
		WorkspaceID: ws, ConversationID: message.ConversationID, Direction: message.Direction,
		Kind: message.Kind, TelegramMessageID: message.TelegramMessageID,
		ReplyToMessageID: workflowInt8(message.ReplyToMessageID), ExecutionID: workflowInt8(message.ExecutionID),
	})
	if err != nil {
		return workflow.Message{}, conversationError(err)
	}
	return mapConversationMessage(row), nil
}

func (s *ConversationStore) BindWorkflow(ctx context.Context, workspaceID string, conversationID, executionID int64, source string) error {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return err
	}
	if conversationID <= 0 || executionID <= 0 || strings.TrimSpace(source) == "" || len(source) > 64 {
		return workflowInvalid("workflow binding")
	}
	return conversationError(s.q.BindConversationWorkflow(ctx, sqlc.BindConversationWorkflowParams{
		WorkspaceID: ws, ConversationID: conversationID, ExecutionID: executionID, Source: source,
	}))
}

func (s *ConversationStore) ListBoundWorkflows(ctx context.Context, workspaceID string, conversationID int64) ([]int64, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if conversationID <= 0 {
		return nil, workflowInvalid("conversation_id")
	}
	rows, err := s.q.ListConversationWorkflowIDs(ctx, sqlc.ListConversationWorkflowIDsParams{WorkspaceID: ws, ConversationID: conversationID})
	return rows, conversationError(err)
}

func (s *ConversationStore) SetSummary(ctx context.Context, workspaceID string, conversationID int64, summary string) error {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return err
	}
	summary = sanitizeText(summary, 0)
	if conversationID <= 0 || len(summary) > 2000 {
		return workflowInvalid("summary")
	}
	_, err = s.q.SetConversationSummary(ctx, sqlc.SetConversationSummaryParams{WorkspaceID: ws, ID: conversationID, Summary: summary})
	return conversationError(err)
}

func (s *ConversationStore) GetSummary(ctx context.Context, workspaceID string, conversationID int64) (string, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return "", err
	}
	if conversationID <= 0 {
		return "", workflowInvalid("conversation_id")
	}
	summary, err := s.q.GetConversationSummary(ctx, sqlc.GetConversationSummaryParams{WorkspaceID: ws, ID: conversationID})
	return summary, conversationError(err)
}

func (s *ConversationStore) BindResource(ctx context.Context, binding workflow.ResourceBinding) error {
	ws, err := readWorkspace(binding.WorkspaceID)
	if err != nil {
		return err
	}
	if binding.ExecutionID <= 0 || strings.TrimSpace(binding.Kind) == "" || len(binding.Kind) > 64 ||
		strings.TrimSpace(binding.ExternalID) == "" || len(binding.ExternalID) > 512 {
		return workflowInvalid("resource binding")
	}
	return conversationError(s.q.BindWorkflowResource(ctx, sqlc.BindWorkflowResourceParams{
		WorkspaceID: ws, ExecutionID: binding.ExecutionID, Kind: binding.Kind, ExternalID: binding.ExternalID,
	}))
}

func (s *ConversationStore) ExecutionsByResource(ctx context.Context, workspaceID, kind, externalID string) ([]int64, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(kind) == "" || len(kind) > 64 || strings.TrimSpace(externalID) == "" || len(externalID) > 512 {
		return nil, workflowInvalid("resource")
	}
	rows, err := s.q.ListResourceExecutionIDs(ctx, sqlc.ListResourceExecutionIDsParams{WorkspaceID: ws, Kind: kind, ExternalID: externalID})
	return rows, conversationError(err)
}

func (s *ConversationStore) ListResources(ctx context.Context, workspaceID string, executionID int64) ([]workflow.ResourceBinding, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if executionID <= 0 {
		return nil, workflowInvalid("execution_id")
	}
	rows, err := s.q.ListExecutionResources(ctx, sqlc.ListExecutionResourcesParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return nil, conversationError(err)
	}
	result := make([]workflow.ResourceBinding, len(rows))
	for i, row := range rows {
		result[i] = workflow.ResourceBinding{WorkspaceID: uuidString(row.WorkspaceID), ExecutionID: row.ExecutionID, Kind: row.Kind, ExternalID: row.ExternalID}
	}
	return result, nil
}

func (s *ConversationStore) AppendInstruction(ctx context.Context, request workflow.InstructionRequest) (workflow.Instruction, error) {
	ws, err := readWorkspace(request.WorkspaceID)
	if err != nil {
		return workflow.Instruction{}, err
	}
	text := sanitizeText(request.Text, 2000)
	if request.ExecutionID <= 0 || request.ConversationID <= 0 || request.TelegramUpdateID <= 0 || strings.TrimSpace(text) == "" {
		return workflow.Instruction{}, workflowInvalid("instruction")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Instruction{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	execution, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: request.ExecutionID})
	if err != nil {
		return workflow.Instruction{}, conversationError(err)
	}
	existing, err := q.GetInstructionByTelegramUpdate(ctx, sqlc.GetInstructionByTelegramUpdateParams{
		WorkspaceID: ws, TelegramUpdateID: request.TelegramUpdateID,
	})
	if err == nil {
		if existing.ExecutionID != request.ExecutionID {
			return workflow.Instruction{}, workflow.ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return workflow.Instruction{}, err
		}
		return mapInstruction(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workflow.Instruction{}, conversationError(err)
	}
	if workflow.State(execution.State).IsTerminal() {
		return workflow.Instruction{}, workflow.ErrNotEligible
	}
	row, err := q.InsertWorkflowInstruction(ctx, sqlc.InsertWorkflowInstructionParams{
		WorkspaceID: ws, ExecutionID: request.ExecutionID, Text: text,
		ConversationID: request.ConversationID, TelegramUpdateID: request.TelegramUpdateID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = q.GetInstructionByTelegramUpdate(ctx, sqlc.GetInstructionByTelegramUpdateParams{
			WorkspaceID: ws, TelegramUpdateID: request.TelegramUpdateID,
		})
	}
	if err != nil {
		return workflow.Instruction{}, conversationError(err)
	}
	if row.ExecutionID != request.ExecutionID {
		return workflow.Instruction{}, workflow.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Instruction{}, fmt.Errorf("commit instruction: %w", err)
	}
	return mapInstruction(row), nil
}

func (s *ConversationStore) ListInstructions(ctx context.Context, workspaceID string, executionID int64) ([]workflow.Instruction, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if executionID <= 0 {
		return nil, workflowInvalid("execution_id")
	}
	rows, err := s.q.ListWorkflowInstructions(ctx, sqlc.ListWorkflowInstructionsParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return nil, conversationError(err)
	}
	result := make([]workflow.Instruction, len(rows))
	for i := range rows {
		result[i] = mapInstruction(rows[i])
	}
	return result, nil
}
