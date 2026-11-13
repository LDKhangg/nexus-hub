package storage

import (
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

func TestDecisionMemoryPhase3Isolation(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
	store, err := NewBehaviorStore(pool)
	if err != nil { t.Fatal(err) }

	t.Run("TC-21/owner decision is searchable and citable", func(t *testing.T) {
		d, err := store.AddDecision(ctx, workspaceA, behavior.NewDecision{Topic: "deployment", Question: "Which deployment window?", Answer: "Friday morning", Scope: behavior.DecisionScope{Behavior: "review"}, Source: behavior.SourceAskAnswer})
		if err != nil { t.Fatal(err) }
		got, err := store.SearchReusableDecisions(ctx, workspaceA, "deployment window", 5)
		if err != nil || len(got) != 1 || got[0].ID != d.ID || got[0].Locator() == "" { t.Fatalf("search=%#v err=%v", got, err) }
	})

	t.Run("TC-22/tenant and forgotten decisions are isolated", func(t *testing.T) {
		d, err := store.AddDecision(ctx, workspaceA, behavior.NewDecision{Topic: "isolationword", Question: "isolationword question", Answer: "private answer", Source: behavior.SourceAskAnswer})
		if err != nil { t.Fatal(err) }
		other, err := store.SearchReusableDecisions(ctx, workspaceB, "isolationword", 5)
		if err != nil || len(other) != 0 { t.Fatalf("cross-tenant=%#v err=%v", other, err) }
		if err := store.ForgetDecision(ctx, workspaceA, d.ID); err != nil { t.Fatal(err) }
		forgotten, err := store.SearchReusableDecisions(ctx, workspaceA, "isolationword", 5)
		if err != nil || len(forgotten) != 0 { t.Fatalf("forgotten reused=%#v err=%v", forgotten, err) }
		if _, err := store.GetDecision(ctx, workspaceB, d.ID); err == nil { t.Fatal("cross-tenant get succeeded") }
	})

	t.Run("TC-22/undone answer is never reusable", func(t *testing.T) {
		_, err := store.AddDecision(ctx, workspaceA, behavior.NewDecision{Topic: "rollbackword", Question: "rollbackword question", Answer: "bad answer", Source: behavior.SourceAutoUndone})
		if err != nil { t.Fatal(err) }
		got, err := store.SearchReusableDecisions(ctx, workspaceA, "rollbackword", 5)
		if err != nil || len(got) != 0 { t.Fatalf("undone reused=%#v err=%v", got, err) }
	})
}
