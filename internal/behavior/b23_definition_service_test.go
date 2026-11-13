package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type b23DraftStore struct {
	DefinitionStore
	draft     Behavior
	stored    bool
	published int
}

func (s *b23DraftStore) GetBehavior(context.Context, string, string) (Behavior, error) {
	if !s.stored {
		return Behavior{}, ErrNotFound
	}
	return s.draft, nil
}
func (s *b23DraftStore) SaveDraft(_ context.Context, _ string, name string, draft, vars json.RawMessage, source string) (Behavior, error) {
	s.draft = Behavior{Name: name, Draft: draft, DraftVars: vars, DraftSource: source}
	s.stored = true
	return s.draft, nil
}
func (s *b23DraftStore) Publish(_ context.Context, _ string, _ string, check func(Behavior) error) (Version, error) {
	if err := check(s.draft); err != nil {
		return Version{}, err
	}
	s.published++
	return Version{Version: s.published, Document: s.draft.Draft, Vars: s.draft.DraftVars, Source: s.draft.DraftSource}, nil
}

// P3-01 (REQ-1, REQ-24): invalid but parseable drafts remain editable/exportable;
// publish invokes semantic validation and never freezes them. Source is preserved byte-for-byte.
func TestB23ImportKeepsEditableInvalidDraftButPublishRejectsIt(t *testing.T) {
	s := &b23DraftStore{}
	ctx := context.Background()
	source := "# Owner's exact formatting is retained.\nschema: 1\nname: owner-review\ntriggers:\n  - type: telegram.command\n    command: /review\ninstructions: ''\n"
	_, problems, err := ImportDraft(ctx, s, "ws", []byte(source), FormatYAML, nil, nil)
	if err != nil || len(problems) == 0 || !s.stored {
		t.Fatal("owner cannot keep an invalid draft to repair")
	}
	if _, err := PublishDraft(ctx, s, "ws", "owner-review", nil); err == nil || !strings.Contains(err.Error(), "instructions") {
		t.Fatal("invalid draft published without schema explanation")
	}
	if s.published != 0 {
		t.Fatal("invalid publication froze a version")
	}
	exported, err := Export(ctx, s, "ws", "owner-review", 0)
	if err != nil || exported != source {
		t.Fatal("draft export lost original YAML")
	}
	source = strings.Replace(source, "instructions: ''", "instructions: 'Inspect the evidence.'", 1)
	if _, problems, err = ImportDraft(ctx, s, "ws", []byte(source), FormatYAML, nil, nil); err != nil || len(problems) != 0 {
		t.Fatal("corrected draft rejected")
	}
	if _, err := PublishDraft(ctx, s, "ws", "owner-review", nil); err != nil || s.published != 1 {
		t.Fatal("valid corrected draft cannot publish")
	}
}

// P3-01 (REQ-1, REQ-2): syntax/unknown fields fail BEFORE saving and cannot destroy a good draft.
func TestB23UnparseableImportCannotOverwriteExistingDraft(t *testing.T) {
	s := &b23DraftStore{}
	ctx := context.Background()
	raw, err := Encode(b23Doc())
	if err != nil {
		t.Fatal(err)
	}
	if _, problems, err := ImportDraft(ctx, s, "ws", raw, FormatJSON, nil, nil); err != nil || len(problems) != 0 {
		t.Fatal("valid import failed")
	}
	before := s.draft.DraftSource
	for _, source := range []string{`{"name":"owner-review","unsafe":"override"}`, "{broken", `{"name":"../wrong"}`} {
		if _, _, err := ImportDraft(ctx, s, "ws", []byte(source), FormatJSON, nil, nil); err == nil {
			t.Error("invalid source accepted")
		}
		if s.draft.DraftSource != before {
			t.Fatal("invalid import destroyed the previous draft")
		}
	}
	if _, err := Export(ctx, s, "ws", "owner-review", -1); !errors.Is(err, ErrInvalid) {
		t.Fatal("negative version not rejected before store access")
	}
}
