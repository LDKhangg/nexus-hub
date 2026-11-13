package storage

import (
	"strings"
	"testing"

	dbsqlc "github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestReportingQueries_OverrideResolutionAndTemplateConstraints(t *testing.T) {
	ctx, pool := testDB(t)
	userID := "019a6690-abcd-7000-8000-000000000201"
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO users(id,display_name) VALUES($1,'Owner'); INSERT INTO workspace_members(user_id,workspace_id,role) VALUES($1,$2,'owner')`, userID, workspaceA)
	q := dbsqlc.New(pool)
	basePrefs := dbsqlc.SetReportPreferencesParams{UserID: testUUID(t, userID), WorkspaceID: testUUID(t, workspaceA), Style: "normal", ShowLinks: true}
	if _, err := q.SetReportPreferences(ctx, basePrefs); err != nil {
		t.Fatal(err)
	}
	overridePrefs := basePrefs
	overridePrefs.BehaviorName = pgtype.Text{String: "review", Valid: true}
	overridePrefs.Style = "detailed"
	overridePrefs.ShowAiSummary = true
	if _, err := q.SetReportPreferences(ctx, overridePrefs); err != nil {
		t.Fatal(err)
	}
	resolved, err := q.ResolveReportPreferences(ctx, dbsqlc.ResolveReportPreferencesParams{UserID: testUUID(t, userID), WorkspaceID: testUUID(t, workspaceA), BehaviorName: pgtype.Text{String: "review", Valid: true}})
	if err != nil || resolved.Style != "detailed" || !resolved.ShowAiSummary {
		t.Fatalf("behavior override not resolved: row=%+v err=%v", resolved, err)
	}
	fallback, err := q.ResolveReportPreferences(ctx, dbsqlc.ResolveReportPreferencesParams{UserID: testUUID(t, userID), WorkspaceID: testUUID(t, workspaceA), BehaviorName: pgtype.Text{String: "unknown", Valid: true}})
	if err != nil || fallback.Style != "normal" || !fallback.ShowLinks || fallback.BehaviorName.Valid {
		t.Fatalf("workspace fallback not resolved: row=%+v err=%v", fallback, err)
	}

	global := dbsqlc.SetReportTemplateParams{UserID: testUUID(t, userID), WorkspaceID: testUUID(t, workspaceA), Kind: "notify", Body: strings.Repeat("a", 1000)}
	if _, err := q.SetReportTemplate(ctx, global); err != nil {
		t.Fatalf("1000-character template rejected: %v", err)
	}
	global.Body = "replacement"
	if _, err := q.SetReportTemplate(ctx, global); err != nil {
		t.Fatal(err)
	}
	override := global
	override.BehaviorName = pgtype.Text{String: "review", Valid: true}
	override.Body = "override"
	if _, err := q.SetReportTemplate(ctx, override); err != nil {
		t.Fatal(err)
	}
	otherKind := global
	otherKind.Kind = "fyi"
	otherKind.Body = "other kind"
	if _, err := q.SetReportTemplate(ctx, otherKind); err != nil {
		t.Fatal(err)
	}
	resolvedTemplate, err := q.ResolveReportTemplate(ctx, dbsqlc.ResolveReportTemplateParams{UserID: testUUID(t, userID), WorkspaceID: testUUID(t, workspaceA), Kind: "notify", BehaviorName: pgtype.Text{String: "review", Valid: true}})
	if err != nil || resolvedTemplate.Body != "override" {
		t.Fatalf("template override not resolved: body=%q err=%v", resolvedTemplate.Body, err)
	}
	var notifyGlobal, notifyOverride, fyiGlobal int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='notify' AND behavior_name IS NULL), count(*) FILTER (WHERE kind='notify' AND behavior_name='review'), count(*) FILTER (WHERE kind='fyi' AND behavior_name IS NULL) FROM report_templates`).Scan(&notifyGlobal, &notifyOverride, &fyiGlobal); err != nil {
		t.Fatal(err)
	}
	if notifyGlobal != 1 || notifyOverride != 1 || fyiGlobal != 1 {
		t.Fatalf("template scope/kind uniqueness wrong: notify global=%d override=%d fyi global=%d", notifyGlobal, notifyOverride, fyiGlobal)
	}
	tooLong := global
	tooLong.Kind = "failure"
	tooLong.Body = strings.Repeat("b", 1001)
	_, err = q.SetReportTemplate(ctx, tooLong)
	requirePGCode(t, err, "23514")
}
