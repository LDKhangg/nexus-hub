package storage

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func parseDBID(id string) (pgtype.UUID, error) {
	if id == "" {
		return pgtype.UUID{}, fmt.Errorf("id must not be empty")
	}
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse UUID: %w", err)
	}
	return parsed, nil
}

func parseScopedIDs(workspaceID, id string) (pgtype.UUID, pgtype.UUID, error) {
	workspace, err := parseDBID(workspaceID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("workspace ID: %w", err)
	}
	key, err := parseDBID(id)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return workspace, key, nil
}

func repositoryError(operation string, err, notFound error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, notFound)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func encodeObject(value map[string]any) ([]byte, error) {
	if value == nil {
		value = map[string]any{}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON object: %w", err)
	}
	return data, nil
}

func validatePage(limit, offset int32) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("pagination requires limit 1..100 and nonnegative offset")
	}
	return nil
}

func nullableText(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

func textPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
