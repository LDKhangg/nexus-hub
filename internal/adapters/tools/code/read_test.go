package codetools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListFiles_DenyListFiltered(t *testing.T) {
	reposDir, _ := managedRepo(t, map[string]string{"main.go": "package main\n", ".env": "X=1", "deploy/key.pem": "k", "secrets/a.txt": "s", "docs/readme.md": "r", "config/credentials.json": "{}"})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_list_files")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app"}`)
	for _, bad := range []string{".env", "key.pem", "secrets/", "credentials"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%s listed:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "docs/readme.md") {
		t.Fatalf("missing files:\n%s", out)
	}
	sub := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"docs"}`)
	if strings.Contains(sub, "main.go") || !strings.Contains(sub, "readme.md") {
		t.Fatalf("path filter:\n%s", sub)
	}
}

func TestSearch_FixedStringAndLimits(t *testing.T) {
	files := map[string]string{"a.go": "x := a.b(c)\nfoo\n", ".env": "a.b(c)=secret", "long.txt": "a.b(c) " + strings.Repeat("ữ", 500) + "\n"}
	var many strings.Builder
	for i := 0; i < 80; i++ {
		many.WriteString("needle\n")
	}
	files["many.txt"] = many.String()
	reposDir, _ := managedRepo(t, files)
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_search")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","query":"a.b(c)"}`)
	if !strings.Contains(out, "a.go:1:") || strings.Contains(out, ".env") || strings.Contains(out, "secret") {
		t.Fatalf("fixed search:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 260 {
			t.Fatalf("line not clipped: %d runes", len([]rune(line)))
		}
	}
	many2 := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","query":"needle"}`)
	if n := strings.Count(many2, "many.txt:"); n != 50 {
		t.Fatalf("hits %d", n)
	}
	if out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","query":"zzz-none"}`); !strings.Contains(out, "no matches") {
		t.Fatalf("no-match output %q", out)
	}
	if out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","query":"^fo+$","regex":true}`); !strings.Contains(out, "a.go:2:foo") {
		t.Fatalf("regex:\n%s", out)
	}
}

func TestReadFile_LineRangeNumbered(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	reposDir, _ := managedRepo(t, map[string]string{"f.txt": b.String()})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_read_file")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"f.txt","start_line":10,"end_line":12}`)
	if !strings.Contains(out, "10\tline 10") || !strings.Contains(out, "12\tline 12") || strings.Contains(out, "line 13") || strings.Contains(out, "line 9\n") {
		t.Fatalf("range:\n%s", out)
	}
	all := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"f.txt"}`)
	if !strings.Contains(all, "200\tline 200") || strings.Contains(all, "line 201") || !strings.Contains(all, "500 lines") {
		t.Fatalf("default window:\n%s", all[len(all)-200:])
	}
}

func TestReadFile_UntrackedInvisible(t *testing.T) {
	reposDir, root := managedRepo(t, map[string]string{"a.txt": "committed"})
	writeFile(t, root, "new.txt", "untracked")
	writeFile(t, root, "a.txt", "modified in worktree")
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_read_file")
	_, err := run(t, tl, grant(true, ""), `{"repo":"acme/app","path":"new.txt"}`)
	wantCode(t, err, "not_found")
	if out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"a.txt"}`); !strings.Contains(out, "committed") {
		t.Fatalf("worktree content read: %s", out)
	}
}

func TestReadFile_Binary(t *testing.T) {
	reposDir, _ := managedRepo(t, map[string]string{"bin.dat": "ab\x00cd", "big.txt": strings.Repeat("y", 1<<20+10)})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_read_file")
	_, err := run(t, tl, grant(true, ""), `{"repo":"acme/app","path":"bin.dat"}`)
	wantCode(t, err, "binary_file")
	_, err = run(t, tl, grant(true, ""), `{"repo":"acme/app","path":"big.txt"}`)
	wantCode(t, err, "file_too_large")
}

func TestReadFile_DeniedPath(t *testing.T) {
	reposDir, _ := managedRepo(t, map[string]string{".env": "X=1", "a/.env.local": "Y", "id_rsa": "k", "x.KEY": "k", "s.p12": "p", "db.kdbx": "k", "id_ed25519.pub": "p"})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_read_file")
	for _, p := range []string{".env", "a/.env.local", "id_rsa", "x.KEY", "s.p12", "db.kdbx", "id_ed25519.pub", "secrets/x", "conf/credentials"} {
		_, err := run(t, tl, grant(true, ""), `{"repo":"acme/app","path":"`+p+`"}`)
		wantCode(t, err, "denied_path")
	}
}

func TestReadFile_PathTraversal(t *testing.T) {
	reposDir, _ := managedRepo(t, map[string]string{"a.txt": "a"})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_read_file")
	for _, p := range []string{"../a.txt", "/etc/passwd", "-p", "a/../../x", "a\x00b", ""} {
		_, err := run(t, tl, grant(true, ""), fmt.Sprintf(`{"repo":"acme/app","path":%q}`, p))
		wantCode(t, err, "invalid_input")
	}
}

func TestOutline_Go(t *testing.T) {
	src := "package p\n\ntype Server struct{}\n\nfunc New() *Server { return nil }\n\nfunc (s *Server) Run() error { return nil }\n\nconst X = 1\n"
	reposDir, _ := managedRepo(t, map[string]string{"s.go": src})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_outline")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"s.go"}`)
	for _, want := range []string{"3: type Server", "5: func New", "7: func (*Server) Run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestOutline_Generic(t *testing.T) {
	src := "import os\n\nclass Foo:\n    def bar(self):\n        pass\n\ndef baz():\n    return 1\n"
	reposDir, _ := managedRepo(t, map[string]string{"m.py": src})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "code_outline")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"m.py"}`)
	for _, want := range []string{"3: class Foo", "4: def bar", "7: def baz"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestGitLog_Format(t *testing.T) {
	reposDir, root := managedRepo(t, map[string]string{"a.txt": "1"})
	for i := 0; i < 25; i++ {
		writeFile(t, root, "a.txt", fmt.Sprint(i))
		sh(t, root, "commit", "-qam", fmt.Sprintf("change %d", i))
	}
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "git_log")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","max":50}`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 20 || !strings.Contains(lines[0], "change 24") || !strings.Contains(lines[0], "Ann") {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
}

func TestGitDiff_StatAndCappedPatch(t *testing.T) {
	reposDir, root := managedRepo(t, map[string]string{"a.txt": "old\n", ".env": "A=1\n"})
	base := sh(t, root, "rev-parse", "HEAD")
	writeFile(t, root, "a.txt", strings.Repeat("new line content\n", 2000))
	writeFile(t, root, ".env", "A=supersecret\n")
	sh(t, root, "commit", "-qam", "big")
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "git_diff")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","base":"`+base+`","head":"HEAD"}`)
	if !strings.Contains(out, "a.txt |") || len(out) > 6*1024+200 || !strings.Contains(out, "[truncated]") {
		t.Fatalf("len %d:\n%.400s", len(out), out)
	}
	if strings.Contains(out, "supersecret") || strings.Contains(out, ".env") {
		t.Fatalf("denied file in diff:\n%.600s", out)
	}
}

func TestGitBlame_RangeLimit(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "l%d\n", i)
	}
	reposDir, _ := managedRepo(t, map[string]string{"f.txt": b.String()})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "git_blame")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app","path":"f.txt","start_line":1,"end_line":300}`)
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 100 || !strings.Contains(out, "Ann") {
		t.Fatalf("%d lines", n)
	}
}

func TestGitStatus_ManagedOnly(t *testing.T) {
	reposDir, root := managedRepo(t, map[string]string{"a.txt": "a"})
	writeFile(t, root, "a.txt", "changed")
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "git_status")
	out := mustOK(t, tl, grant(true, ""), `{"repo":"acme/app"}`)
	if !strings.Contains(out, "branch.head main") || !strings.Contains(out, "a.txt") {
		t.Fatalf("status:\n%s", out)
	}
	_, err := run(t, tl, grant(false, root), `{"repo":"acme/app"}`)
	wantCode(t, err, "not_managed")
}

func TestReadTools_LocalPath(t *testing.T) {
	_, root := managedRepo(t, map[string]string{"a.txt": "local content"})
	tl := findTool(t, NewReadTools(newTestRunner(t), t.TempDir()), "code_read_file")
	if out := mustOK(t, tl, grant(false, root), `{"repo":"acme/app","path":"a.txt"}`); !strings.Contains(out, "local content") {
		t.Fatal(out)
	}
}

func TestRef_Validation(t *testing.T) {
	reposDir, _ := managedRepo(t, map[string]string{"a.txt": "a"})
	tl := findTool(t, NewReadTools(newTestRunner(t), reposDir), "git_log")
	for _, ref := range []string{"-p", "--output=/tmp/x", "a..b", "x@{1}", "a//b", "x.lock", "a b", "x/"} {
		_, err := run(t, tl, grant(true, ""), fmt.Sprintf(`{"repo":"acme/app","ref":%q}`, ref))
		wantCode(t, err, "invalid_input")
	}
	_, err := run(t, tl, grant(true, ""), `{"repo":"acme/app","ref":"nope"}`)
	wantCode(t, err, "not_found")
	_, err = run(t, tl, grant(true, ""), `{"repo":"acme/app","bogus":1}`)
	wantCode(t, err, "invalid_input")
}

// markerScript writes an executable script that creates marker when run.
func markerScript(t *testing.T) (script, marker string) {
	dir := t.TempDir()
	marker = filepath.Join(dir, "PWNED")
	script = filepath.Join(dir, "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat \"$@\" 2>/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func TestHardening_FsmonitorNotExecuted(t *testing.T) {
	reposDir, root := managedRepo(t, map[string]string{"a.txt": "a"})
	script, marker := markerScript(t)
	sh(t, root, "config", "core.fsmonitor", script)
	sh(t, root, "config", "core.pager", script)
	writeFile(t, root, "a.txt", "dirty")
	ts := NewReadTools(newTestRunner(t), reposDir)
	// TD3-4 / TD-4 (REQ-F1): owner decision rejects program config,
	// even when command-line hardening could neutralize the program.
	_, err := run(t, findTool(t, ts, "git_status"), grant(true, ""), `{"repo":"acme/app"}`)
	wantCode(t, err, "unsafe_repo_config")
	mustOK(t, findTool(t, ts, "git_log"), grant(true, ""), `{"repo":"acme/app"}`)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("fsmonitor/pager executed")
	}
}

func TestHardening_ExternalDiffAndTextconvNotExecuted(t *testing.T) {
	reposDir, root := managedRepo(t, map[string]string{"a.txt": "a\n", ".gitattributes": "* diff=evil\n"})
	base := sh(t, root, "rev-parse", "HEAD")
	script, marker := markerScript(t)
	sh(t, root, "config", "diff.external", script)
	sh(t, root, "config", "diff.evil.textconv", script)
	sh(t, root, "config", "diff.evil.command", script)
	writeFile(t, root, "a.txt", "b\n")
	sh(t, root, "commit", "-qam", "b")
	ts := NewReadTools(newTestRunner(t), reposDir)
	mustOK(t, findTool(t, ts, "git_diff"), grant(true, ""), `{"repo":"acme/app","base":"`+base+`","head":"HEAD"}`)
	mustOK(t, findTool(t, ts, "git_blame"), grant(true, ""), `{"repo":"acme/app","path":"a.txt","start_line":1,"end_line":1}`)
	mustOK(t, findTool(t, ts, "code_search"), grant(true, ""), `{"repo":"acme/app","query":"b"}`)
	mustOK(t, findTool(t, ts, "code_read_file"), grant(true, ""), `{"repo":"acme/app","path":"a.txt"}`)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("external diff/textconv executed")
	}
}

func TestRepoUnavailable(t *testing.T) {
	ts := NewReadTools(newTestRunner(t), t.TempDir())
	_, err := run(t, findTool(t, ts, "code_list_files"), grant(true, ""), `{"repo":"acme/app"}`)
	wantCode(t, err, "repo_unavailable")
	_, err = run(t, findTool(t, ts, "code_list_files"), grant(false, ""), `{"repo":"acme/app"}`)
	wantCode(t, err, "repo_unavailable")
	_, err = run(t, findTool(t, ts, "code_list_files"), grant(false, t.TempDir()), `{"repo":"acme/app"}`)
	wantCode(t, err, "repo_unavailable")
}
