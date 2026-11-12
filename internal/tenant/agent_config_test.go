package tenant

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func knownTools() map[string]Tier {
	return map[string]Tier{
		"github_pr_view": TierRead, "github_pr_checks": TierRead, "github_pr_files": TierRead, "github_issue_view": TierRead,
		"code_list_files": TierRead, "code_search": TierRead, "code_read_file": TierRead, "code_outline": TierRead,
		"git_log": TierRead, "git_diff": TierRead, "git_blame": TierRead, "git_status": TierRead, "clickup_get_task": TierRead,
		"github_pr_comment": TierWrite, "git_sync": TierWrite, "git_pull": TierWrite, "git_checkout": TierWrite,
		"git_commit": TierWrite, "git_config_set": TierWrite, "git_push": TierWrite,
		"github_pr_merge": TierDangerous, "github_pr_close": TierDangerous, "github_issue_close": TierDangerous,
		"git_reset_hard": TierDangerous, "git_clean": TierDangerous,
	}
}

const baseAgentJSON = `{"agent":{
  "enabled": true, "confidence_threshold": 0.9, "persona": "terse", "clickup_token_ref": "clickup",
  "tools": {
    "enabled": ["github_pr_view","code_search","git_sync","git_push"],
    "dry_run": true, "max_writes_per_job": 3,
    "github": {
      "accounts": {
        "personal": {"type":"pat","token_ref":"gh_personal","login":"LDKhangg","commit_name":"Nexus Bot","commit_email":"nexus@example.invalid"},
        "workbot":  {"type":"app","app_id":"123","installation_id":"456","private_key_ref":"gh_app_key","commit_name":"nexus[bot]","commit_email":"nexus@example.invalid"}
      },
      "repos": {
        "LDKhangg/nexus-hub": {
          "account": "personal", "local_path": "/srv/checkout", "managed": true,
          "allow": ["github_pr_comment","git_sync","git_push","github_pr_merge"],
          "merge_methods": ["squash"], "merge_base_branches": ["develop"]
        }
      }
    }
  }
}}`

func decodeConfig(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// mutate decodes the base config and applies fn to the agent map.
func mutate(t *testing.T, fn func(agent map[string]any)) map[string]any {
	m := decodeConfig(t, baseAgentJSON)
	fn(m["agent"].(map[string]any))
	return m
}

func toolsOf(a map[string]any) map[string]any { return a["tools"].(map[string]any) }
func githubOf(a map[string]any) map[string]any {
	return toolsOf(a)["github"].(map[string]any)
}
func repoOf(a map[string]any) map[string]any {
	return githubOf(a)["repos"].(map[string]any)["LDKhangg/nexus-hub"].(map[string]any)
}
func accountOf(a map[string]any, name string) map[string]any {
	return githubOf(a)["accounts"].(map[string]any)[name].(map[string]any)
}

func TestParseAgentConfig(t *testing.T) {
	t.Run("absent is disabled with read defaults", func(t *testing.T) {
		c, err := ParseAgentConfig(map[string]any{"clickup_team_id": "1"}, knownTools())
		if err != nil {
			t.Fatal(err)
		}
		if c.Enabled || c.ConfidenceThreshold != 0.85 || c.ClickUpTokenRef != "clickup" || c.Tools.MaxWritesPerJob != 2 {
			t.Fatalf("got %+v", c)
		}
		if len(c.Tools.Enabled) != 13 {
			t.Fatalf("default enabled = %v", c.Tools.Enabled)
		}
		for _, name := range c.Tools.Enabled {
			if knownTools()[name] != TierRead {
				t.Fatalf("default enabled non-read tool %s", name)
			}
		}
	})
	t.Run("full config", func(t *testing.T) {
		c, err := ParseAgentConfig(decodeConfig(t, baseAgentJSON), knownTools())
		if err != nil {
			t.Fatal(err)
		}
		if !c.Enabled || c.ConfidenceThreshold != 0.9 || c.Persona != "terse" || !c.Tools.DryRun || c.Tools.MaxWritesPerJob != 3 {
			t.Fatalf("got %+v", c)
		}
		acc := c.Tools.Accounts["workbot"]
		if acc.Name != "workbot" || acc.Type != "app" || acc.AppID != "123" || acc.InstallationID != "456" || acc.PrivateKeyRef != "gh_app_key" {
			t.Fatalf("app account %+v", acc)
		}
		r := c.Tools.Repos["LDKhangg/nexus-hub"]
		if r.Repo != "LDKhangg/nexus-hub" || r.Account != "personal" || !r.Managed || r.LocalPath != "/srv/checkout" {
			t.Fatalf("repo %+v", r)
		}
		if !reflect.DeepEqual(r.PushBranches, []string{"nexus/*"}) || !reflect.DeepEqual(r.ProtectedBranches, []string{"main", "master"}) || !r.RequireChecksGreen {
			t.Fatalf("repo defaults %+v", r)
		}
		if !reflect.DeepEqual(r.MergeBaseBranches, []string{"develop"}) || !reflect.DeepEqual(r.MergeMethods, []string{"squash"}) {
			t.Fatalf("repo merge %+v", r)
		}
	})
	t.Run("require_checks_green false honored", func(t *testing.T) {
		c, err := ParseAgentConfig(mutate(t, func(a map[string]any) { repoOf(a)["require_checks_green"] = false }), knownTools())
		if err != nil || c.Tools.Repos["LDKhangg/nexus-hub"].RequireChecksGreen {
			t.Fatalf("got %+v %v", c, err)
		}
	})

	bad := map[string]func(a map[string]any){
		"threshold high":           func(a map[string]any) { a["confidence_threshold"] = 1.5 },
		"threshold zero":           func(a map[string]any) { a["confidence_threshold"] = 0.0 },
		"enabled not bool":         func(a map[string]any) { a["enabled"] = "yes" },
		"unknown enabled tool":     func(a map[string]any) { toolsOf(a)["enabled"] = []any{"rm_rf"} },
		"unknown allow tool":       func(a map[string]any) { repoOf(a)["allow"] = []any{"rm_rf"} },
		"read tool in allow":       func(a map[string]any) { repoOf(a)["allow"] = []any{"code_search"} },
		"missing account":          func(a map[string]any) { repoOf(a)["account"] = "ghost" },
		"pat without login":        func(a map[string]any) { delete(accountOf(a, "personal"), "login") },
		"pat without token_ref":    func(a map[string]any) { delete(accountOf(a, "personal"), "token_ref") },
		"pat without commit name":  func(a map[string]any) { delete(accountOf(a, "personal"), "commit_name") },
		"pat without commit email": func(a map[string]any) { delete(accountOf(a, "personal"), "commit_email") },
		"unknown account type":     func(a map[string]any) { accountOf(a, "personal")["type"] = "oauth" },
		"app non numeric id":       func(a map[string]any) { accountOf(a, "workbot")["app_id"] = "12a" },
		"app non numeric inst":     func(a map[string]any) { accountOf(a, "workbot")["installation_id"] = "" },
		"app bad key ref":          func(a map[string]any) { accountOf(a, "workbot")["private_key_ref"] = "Bad Ref" },
		"bad token ref":            func(a map[string]any) { accountOf(a, "personal")["token_ref"] = "../x" },
		"bad clickup ref":          func(a map[string]any) { a["clickup_token_ref"] = "UPPER" },
		"merge method fast":        func(a map[string]any) { repoOf(a)["merge_methods"] = []any{"fast"} },
		"relative local path":      func(a map[string]any) { repoOf(a)["local_path"] = "srv/checkout" },
		"unclean local path":       func(a map[string]any) { repoOf(a)["local_path"] = "/srv/../etc" },
		"unmanaged workspace tool": func(a map[string]any) { repoOf(a)["managed"] = false },
		"push pattern star":        func(a map[string]any) { repoOf(a)["push_branches"] = []any{"*"} },
		"push pattern dotdot":      func(a map[string]any) { repoOf(a)["push_branches"] = []any{"nexus/../*"} },
		"push pattern dash":        func(a map[string]any) { repoOf(a)["push_branches"] = []any{"-f"} },
		"push pattern inner star":  func(a map[string]any) { repoOf(a)["push_branches"] = []any{"nexus/*/x"} },
		"max writes 6":             func(a map[string]any) { toolsOf(a)["max_writes_per_job"] = 6.0 },
		"max writes fractional":    func(a map[string]any) { toolsOf(a)["max_writes_per_job"] = 1.5 },
		"bad repo key": func(a map[string]any) {
			g := githubOf(a)["repos"].(map[string]any)
			g["not-a-repo"] = g["LDKhangg/nexus-hub"]
		},
		"repo key dotdot": func(a map[string]any) {
			g := githubOf(a)["repos"].(map[string]any)
			g["owner/.."] = g["LDKhangg/nexus-hub"]
		},
		"repo key extra slash": func(a map[string]any) {
			g := githubOf(a)["repos"].(map[string]any)
			g["a/b/c"] = g["LDKhangg/nexus-hub"]
		},
		"persona too long":         func(a map[string]any) { a["persona"] = strings.Repeat("p", 2001) },
		"allow not list":           func(a map[string]any) { repoOf(a)["allow"] = "git_push" },
		"protected branch invalid": func(a map[string]any) { repoOf(a)["protected_branches"] = []any{"a..b"} },
	}
	for name, fn := range bad {
		t.Run(name, func(t *testing.T) {
			if c, err := ParseAgentConfig(mutate(t, fn), knownTools()); err == nil {
				t.Fatalf("accepted: %+v", c)
			}
		})
	}
}

func TestMatchBranchPattern(t *testing.T) {
	cases := []struct {
		pattern, branch string
		want            bool
	}{
		{"nexus/*", "nexus/feat", true},
		{"nexus/*", "nexus/a/b", true},
		{"nexus/*", "nexus/", false},
		{"nexus/*", "feature/x", false},
		{"nexus/*", "nexusx/feat", false},
		{"main", "main", true},
		{"main", "main2", false},
		{"rel-*", "rel-1.0", true},
	}
	for _, tc := range cases {
		if got := MatchBranchPattern(tc.pattern, tc.branch); got != tc.want {
			t.Errorf("%q %q = %v", tc.pattern, tc.branch, got)
		}
	}
}

func TestValidRepoAndRef(t *testing.T) {
	for _, r := range []string{"LDKhangg/nexus-hub", "a/b.c_d"} {
		if !ValidRepo(r) {
			t.Errorf("%s rejected", r)
		}
	}
	for _, r := range []string{"", "a", "a/", "/b", "a/b/c", "a/..", "a/.", "-a/b", "a b/c"} {
		if ValidRepo(r) {
			t.Errorf("%s accepted", r)
		}
	}
	if !ValidCredentialRef("gh_app.key-1") || ValidCredentialRef("_x") || ValidCredentialRef("") || ValidCredentialRef(strings.Repeat("a", 64)) {
		t.Error("credential ref validation wrong")
	}
}
