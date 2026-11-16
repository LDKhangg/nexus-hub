package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Phase 6b batch B: nh telegram topics|group and nh assistant route-eval
// (REQ-10, REQ-13). DB tests go through setupDB (TEST_DATABASE_URL, loopback).

func b1Decode(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("output is not JSON: %q (%v)", out, err)
	}
	return m
}

// b1Integrations returns the workspace's integration ids by platform.
func b1Integrations(t *testing.T, c *phase4CLI) (telegram, clickup string) {
	t.Helper()
	if err := c.pool.QueryRow(c.ctx, `SELECT id FROM integrations WHERE workspace_id=$1 AND platform='telegram'`, c.ws).Scan(&telegram); err != nil {
		t.Fatal(err)
	}
	if err := c.pool.QueryRow(c.ctx, `SELECT id FROM integrations WHERE workspace_id=$1 AND platform='clickup'`, c.ws).Scan(&clickup); err != nil {
		t.Fatal(err)
	}
	return
}

func b1Seed(t *testing.T, c *phase4CLI, ws, integration, task string, chat, thread int64, closed bool) {
	t.Helper()
	store, err := storage.NewTelegramTopicStore(c.pool)
	if err != nil {
		t.Fatal(err)
	}
	topic, _, err := store.EnsureTopic(c.ctx, behavior.TopicKey{WorkspaceID: ws, IntegrationID: integration, ChatID: chat, SubjectKind: behavior.TopicSubjectClickUpTask, SubjectKey: task})
	if err != nil {
		t.Fatal(err)
	}
	if thread > 0 {
		if _, err := store.SetTopicThread(c.ctx, ws, topic.ID, thread, topic.ReservedAt); err != nil {
			t.Fatal(err)
		}
	}
	if closed {
		if _, err := store.SetTopicState(c.ctx, ws, topic.ID, behavior.TopicClosed); err != nil {
			t.Fatal(err)
		}
	}
}

// TC-B1-90 (REQ-10): `telegram topics` lists subject, thread, state and times
// of this workspace only, with no names.
func TestPhase6bB1_TelegramTopicsCLI(t *testing.T) {
	c := newPhase4CLI(t)
	tg, _ := b1Integrations(t, c)
	out := c.must("telegram", "topics", "--workspace-id", c.ws)
	m := b1Decode(t, out)
	if m["count"] != float64(0) || m["workspace_id"] != c.ws {
		t.Fatalf("empty: %v", m)
	}
	if topics, ok := m["topics"].([]any); !ok || len(topics) != 0 {
		t.Fatalf("topics must be an empty array, got %#v", m["topics"])
	}
	b1Seed(t, c, c.ws, tg, "86abc", -1001234567890, 55, false)
	b1Seed(t, c, c.ws, tg, "86abd", -1001234567890, 56, true)
	b1Seed(t, c, c.ws, tg, "86abe", -1001234567890, 0, false) // reserved, no thread yet
	// Another workspace's topic must not show.
	other := "019a6690-abcd-7000-8000-0000000000aa"
	if _, err := c.pool.Exec(c.ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Other')`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(c.ctx, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES('019a6690-abcd-7000-8000-0000000000ab',$1,'telegram','control','{"chat_id":"-10042","bot_token_env":"T"}')`, other); err != nil {
		t.Fatal(err)
	}
	b1Seed(t, c, other, "019a6690-abcd-7000-8000-0000000000ab", "OTHER-TASK", -1001234567890, 77, false)
	out = c.must("telegram", "topics", "--workspace-id", c.ws)
	m = b1Decode(t, out)
	rows, _ := m["topics"].([]any)
	if m["count"] != float64(3) || len(rows) != 3 {
		t.Fatalf("topics=%v", m)
	}
	byKey := map[string]map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		byKey[row["subject_key"].(string)] = row
		for _, key := range []string{"name", "title", "topic_name", "task_name"} {
			if _, has := row[key]; has {
				t.Fatalf("row has a name field %q", key)
			}
		}
	}
	if byKey["86abc"]["thread_id"] != float64(55) || byKey["86abc"]["state"] != "open" || byKey["86abc"]["subject_kind"] != "clickup_task" || byKey["86abc"]["chat_id"] != float64(-1001234567890) {
		t.Fatalf("open row=%v", byKey["86abc"])
	}
	if byKey["86abd"]["state"] != "closed" || byKey["86abd"]["closed_at"] == nil {
		t.Fatalf("closed row=%v", byKey["86abd"])
	}
	if byKey["86abe"]["state"] != "reserved" {
		t.Fatalf("reserved row=%v", byKey["86abe"])
	}
	if _, leaked := byKey["OTHER-TASK"]; leaked || strings.Contains(out, "OTHER-TASK") {
		t.Fatal("another workspace's topic was listed")
	}
	for _, key := range []string{"created_at", "updated_at", "integration_id"} {
		if byKey["86abc"][key] == nil {
			t.Fatalf("missing %s", key)
		}
	}
}

// TC-B1-91 (REQ-10, human): bad topics invocations fail with usage text.
func TestPhase6bB1_TelegramTopicsCLIUsage(t *testing.T) {
	c := newPhase4CLI(t)
	for _, args := range [][]string{
		{"telegram", "topics"},
		{"telegram", "topics", "--workspace-id", "not-a-uuid"},
		{"telegram", "topics", "--workspace-id", c.ws, "extra"},
		{"telegram", "topics", "--workspace-id"},
		{"telegram", "topics", "--bogus", "1"},
	} {
		out, err := c.run(args...)
		if err == nil || out != "" || !strings.Contains(err.Error(), "telegram topics") {
			t.Fatalf("%v: out=%q err=%v", args, out, err)
		}
	}
}

// TC-B1-92 (REQ-10): `telegram group show` and `set`.
func TestPhase6bB1_TelegramGroupCLI(t *testing.T) {
	c := newPhase4CLI(t)
	tg, source := b1Integrations(t, c)
	show := func() map[string]any {
		t.Helper()
		return b1Decode(t, c.must("telegram", "group", "show", "--workspace-id", c.ws, "--integration-id", tg))
	}
	before := show()
	if before["owner_chat_id_set"] != false || before["chat_id"] != float64(-10042) || before["uses_global_owner_chat"] != false || before["integration_id"] != tg {
		t.Fatalf("before=%v", before)
	}
	var cfgBefore string
	if err := c.pool.QueryRow(c.ctx, `SELECT config::text FROM integrations WHERE id=$1`, tg).Scan(&cfgBefore); err != nil {
		t.Fatal(err)
	}
	out := c.must("telegram", "group", "set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "-1001234567890")
	set := b1Decode(t, out)
	if set["status"] != "set" || set["chat_id"] != float64(-1001234567890) {
		t.Fatalf("set=%v", set)
	}
	after := show()
	if after["owner_chat_id_set"] != true || after["chat_id"] != float64(-1001234567890) {
		t.Fatalf("after=%v", after)
	}
	// The rest of the config is kept (chat_id, bot token reference).
	var chat, token, owner string
	if err := c.pool.QueryRow(c.ctx, `SELECT config->>'chat_id', coalesce(config->>'bot_token_env', config->>'bot_token_ref',''), config->>'owner_chat_id' FROM integrations WHERE id=$1`, tg).Scan(&chat, &token, &owner); err != nil {
		t.Fatal(err)
	}
	if chat != "-10042" || token == "" || owner != "-1001234567890" || !strings.Contains(cfgBefore, "bot_token") {
		t.Fatalf("config chat=%q token=%q owner=%q before=%s", chat, token, owner, cfgBefore)
	}
	// Set again with a new id (the group became a supergroup).
	c.must("telegram", "group", "set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", " -1009876543210 ")
	if got := show(); got["chat_id"] != float64(-1009876543210) {
		t.Fatalf("second set: %v", got)
	}
	if strings.Contains(out+cfgBefore, "private-value") {
		t.Fatal("secret in output")
	}
	_ = source
}

// TC-B1-93 (REQ-10, abuse): invalid group input changes nothing.
func TestPhase6bB1_TelegramGroupCLIValidation(t *testing.T) {
	c := newPhase4CLI(t)
	tg, source := b1Integrations(t, c)
	snapshot := func() string {
		var cfg string
		if err := c.pool.QueryRow(c.ctx, `SELECT string_agg(config::text, '|' ORDER BY id) FROM integrations WHERE workspace_id=$1`, c.ws).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	before := snapshot()
	other := "019a6690-abcd-7000-8000-0000000000aa"
	if _, err := c.pool.Exec(c.ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Other')`, other); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"zero":                {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "0"},
		"not a number":        {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "abc"},
		"empty":               {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", ""},
		"missing flag":        {"set", "--workspace-id", c.ws, "--integration-id", tg},
		"float":               {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "-100.5"},
		"overflow":            {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "99999999999999999999"},
		"hex":                 {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "0x10"},
		"username":            {"set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "@mygroup"},
		"clickup integration": {"set", "--workspace-id", c.ws, "--integration-id", source, "--chat-id", "-1001234567890"},
		"show clickup":        {"show", "--workspace-id", c.ws, "--integration-id", source},
		"other workspace":     {"set", "--workspace-id", other, "--integration-id", tg, "--chat-id", "-1001234567890"},
		"unknown integration": {"set", "--workspace-id", c.ws, "--integration-id", "019a6690-abcd-7000-8000-0000000000ff", "--chat-id", "-1001234567890"},
		"bad workspace id":    {"set", "--workspace-id", "x", "--integration-id", tg, "--chat-id", "-1001234567890"},
		"bad integration id":  {"set", "--workspace-id", c.ws, "--integration-id", "x", "--chat-id", "-1001234567890"},
		"show with chat id":   {"show", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "-1001234567890"},
		"extra arg":           {"show", "--workspace-id", c.ws, "--integration-id", tg, "more"},
		"unknown action":      {"unset", "--workspace-id", c.ws, "--integration-id", tg},
		"no action":           {},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := c.run(append([]string{"telegram", "group"}, args...)...)
			if err == nil || out != "" {
				t.Fatalf("accepted: out=%q err=%v", out, err)
			}
			if snapshot() != before {
				t.Fatal("a rejected command changed the stored config")
			}
		})
	}
	// A disabled integration is refused too.
	if _, err := c.pool.Exec(c.ctx, `UPDATE integrations SET is_enabled=false WHERE id=$1`, tg); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run("telegram", "group", "set", "--workspace-id", c.ws, "--integration-id", tg, "--chat-id", "-1001234567890"); err == nil || out != "" {
		t.Fatalf("disabled integration accepted: %q %v", out, err)
	}
}

// TC-B1-94 (REQ-13): route-eval argument and file errors, before any model call.
func TestPhase6bB1_RouteEvalArgumentErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const ws = "019a6690-abcd-7000-8000-000000000001"
	long := strings.Repeat("a", 2001)
	var many strings.Builder
	for i := 0; i < 201; i++ {
		many.WriteString("answer|hi\n")
	}
	for name, args := range map[string][]string{
		"no args":          {},
		"unknown sub":      {"other"},
		"no workspace":     {"route-eval"},
		"bad workspace":    {"route-eval", "--workspace-id", "x"},
		"extra arg":        {"route-eval", "--workspace-id", ws, "more"},
		"bad history":      {"route-eval", "--workspace-id", ws, "--thread-history", "weird"},
		"missing file":     {"route-eval", "--workspace-id", ws, "--file", filepath.Join(dir, "nope.txt")},
		"bad expected":     {"route-eval", "--workspace-id", ws, "--file", write("a.txt", "maybe|hello\n")},
		"no separator":     {"route-eval", "--workspace-id", ws, "--file", write("b.txt", "answer hello\n")},
		"empty message":    {"route-eval", "--workspace-id", ws, "--file", write("c.txt", "answer|   \n")},
		"too long message": {"route-eval", "--workspace-id", ws, "--file", write("d.txt", "answer|"+long+"\n")},
		"too many cases":   {"route-eval", "--workspace-id", ws, "--file", write("e.txt", many.String())},
		"only comments":    {"route-eval", "--workspace-id", ws, "--file", write("f.txt", "# nothing\n\n")},
		"empty file":       {"route-eval", "--workspace-id", ws, "--file", write("g.txt", "")},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := assistantCommand(ctx, nil, args, &out, func(string) (string, bool) { return "", false })
			if err == nil || out.Len() != 0 {
				t.Fatalf("out=%q err=%v", out.String(), err)
			}
			if strings.Contains(err.Error(), "hello") {
				t.Fatalf("error echoes the case text: %v", err)
			}
		})
	}
	// A valid file with comments, blanks, case-insensitive expectations and
	// the escalate alias parses.
	cases, err := readRouteEvalCases(write("ok.txt", "# c\n\n ANSWER | chào em \nescalate|làm đi\nwork|a|b\ndeep|x\n"))
	if err != nil || len(cases) != 4 {
		t.Fatalf("cases=%+v err=%v", cases, err)
	}
	if cases[0] != (routeEvalCase{"answer", "chào em"}) || cases[1].expected != "escalate" || cases[2].text != "a|b" {
		t.Fatalf("cases=%+v", cases)
	}
	// The boundary: 2000 bytes and 200 cases are fine.
	var edge strings.Builder
	for i := 0; i < 200; i++ {
		edge.WriteString("answer|" + strings.Repeat("a", 2000) + "\n")
	}
	if got, err := readRouteEvalCases(write("edge.txt", edge.String())); err != nil || len(got) != 200 {
		t.Fatalf("edge cases=%d err=%v", len(got), err)
	}
}

// TC-B1-95 (REQ-13): a case matches only when the route is the expected one;
// escalate accepts work and deep; a router failure never matches.
func TestPhase6bB1_RouteEvalMatching(t *testing.T) {
	res := func(route, errCode string) behavior.RouteEvalResult {
		return behavior.RouteEvalResult{Route: route, RouterError: errCode}
	}
	for _, tc := range []struct {
		expected string
		got      behavior.RouteEvalResult
		want     bool
	}{
		{"answer", res("answer", ""), true},
		{"answer", res("work", ""), false},
		{"answer", res("deep", ""), false},
		{"work", res("work", ""), true},
		{"work", res("deep", ""), false},
		{"work", res("answer", ""), false},
		{"deep", res("deep", ""), true},
		{"deep", res("work", ""), false},
		{"escalate", res("work", ""), true},
		{"escalate", res("deep", ""), true},
		{"escalate", res("answer", ""), false},
		{"answer", res("work_fallback", "timeout"), false},
		{"work", res("work_fallback", "timeout"), false},
		{"escalate", res("work_fallback", "timeout"), false},
		{"answer", res("answer", "bad_json"), false},
		{"", res("", ""), true},
	} {
		if got := routeMatches(tc.expected, tc.got); got != tc.want {
			t.Errorf("routeMatches(%q, %+v)=%v want %v", tc.expected, tc.got, got, tc.want)
		}
	}
	// The built-in set covers the planned groups and only valid expectations.
	groups := map[string]int{}
	for _, c := range defaultRouteEvalCases {
		switch c.expected {
		case "answer", "work", "deep":
			groups[c.expected]++
		default:
			t.Fatalf("built-in case with expectation %q", c.expected)
		}
		if strings.TrimSpace(c.text) == "" || len(c.text) > 2000 {
			t.Fatalf("bad built-in case %q", c.text)
		}
	}
	if groups["answer"] == 0 || groups["work"] == 0 || groups["deep"] == 0 {
		t.Fatalf("built-in groups=%v", groups)
	}
	joined := ""
	for _, c := range defaultRouteEvalCases {
		joined += strings.ToLower(c.text) + "\n"
	}
	for _, need := range []string{"hi", "cảm ơn", "task", "wf-", "ừ làm đi", "thiết kế"} {
		if !strings.Contains(joined, need) {
			t.Errorf("built-in set misses a %q case", need)
		}
	}
}

// TC-B1-96 (REQ-13): with a provider that has no structured calls, route-eval
// fails with a non-zero exit and prints nothing.
func TestPhase6bB1_RouteEvalProviderWithoutStructuredCalls(t *testing.T) {
	c := newPhase4CLI(t)
	script := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.must("settings", "set", "claude_cli_path", script)
	c.must("settings", "set", "llm_provider", "claude_cli")
	out, err := c.run("assistant", "route-eval", "--workspace-id", c.ws)
	if err == nil || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !strings.Contains(err.Error(), "route-eval") {
		t.Fatalf("err=%v", err)
	}
}
