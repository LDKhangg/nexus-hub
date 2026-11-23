package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 12 fix-A2 / fix-A3 (R-A-6, R-A2-1..3): the deploy ships and
// installs the systemd unit and rolls back cleanly. The tests run a temp
// COPY of deploy/vps/deploy.sh (paths pointed into a temp dir, the root
// check removed) with stubbed systemctl, install, chown, runuser and
// goose. The production script is never modified.

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TC-B-50 (R-A-6)
func TestPhase12bDeployPackagesTheUnit(t *testing.T) {
	yml := readRepoFile(t, ".github/workflows/deploy.yml")
	if !strings.Contains(yml, "cp deploy/vps/nexus-hub.service dist/pkg/deploy/vps/nexus-hub.service") {
		t.Error("deploy.yml does not copy the unit into the release package")
	}
	if !strings.Contains(yml, "mkdir -p dist/pkg/db dist/pkg/deploy/vps") {
		t.Error("deploy.yml does not create dist/pkg/deploy/vps")
	}
	sh := readRepoFile(t, "deploy/vps/deploy.sh")
	if !strings.Contains(sh, "readonly UNIT_SRC=deploy/vps/nexus-hub.service\n") {
		t.Error("deploy.sh UNIT_SRC does not match the packaged path")
	}
	unit := readRepoFile(t, "deploy/vps/nexus-hub.service")
	for _, want := range []string{"\nKillMode=mixed\n", "\nTimeoutStopSec=100\n", "ExecStart=/opt/nexus-hub/current/nexus-server"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit misses %q", want)
		}
	}
	// deploy.sh and the unit pass bash -n.
	if bash, err := exec.LookPath("bash"); err == nil {
		out, err := exec.Command(bash, "-n", filepath.Join("..", "..", "deploy/vps/deploy.sh")).CombinedOutput()
		if err != nil {
			t.Errorf("bash -n deploy.sh: %v\n%s", err, out)
		}
	}
}

const (
	sha1Prev = "1111111111111111111111111111111111111111"
	sha2New  = "2222222222222222222222222222222222222222"
)

type deployEnv struct {
	t    *testing.T
	dir  string
	port int
	// results
	out  string
	exit int
}

func (d *deployEnv) path(parts ...string) string {
	return filepath.Join(append([]string{d.dir}, parts...)...)
}

func (d *deployEnv) write(rel, content string, mode os.FileMode) {
	d.t.Helper()
	p := d.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deployEnv) read(rel string) (string, bool) {
	b, err := os.ReadFile(d.path(rel))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (d *deployEnv) calls() []string {
	s, _ := d.read("calls.log")
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (d *deployEnv) count(call string) int {
	n := 0
	for _, c := range d.calls() {
		if c == call {
			n++
		}
	}
	return n
}

func (d *deployEnv) index(call string) int {
	for i, c := range d.calls() {
		if c == call {
			return i
		}
	}
	return -1
}

// releaseDir fills a release directory (nexus-server, nh, goose, REVISION,
// migrations and an optional unit).
func (d *deployEnv) releaseFiles(root, sha, unit string) {
	d.write(root+"/nexus-server", "#!/bin/sh\nexit 0\n", 0o755)
	d.write(root+"/nh", "#!/bin/sh\nexit 0\n", 0o755)
	d.write(root+"/goose", "#!/bin/sh\nexit 0\n", 0o755)
	d.write(root+"/REVISION", sha+"\n", 0o644)
	d.write(root+"/db/migrations/00001_init.sql", "-- +goose Up\nSELECT 1;\n", 0o644)
	if unit != "" {
		d.write(root+"/deploy/vps/nexus-hub.service", unit, 0o644)
	}
}

type deployOpts struct {
	installedUnit string // "" = no unit installed yet
	staleUnitPrev string // "" = none
	previousUnit  string // unit shipped by the previous release; "-" = none
	newUnit       string // unit shipped by the new release; "" = none
	failReload    bool   // every daemon-reload fails
	failRestart1  bool   // the first restart fails
	noPrevious    bool   // no previous release at all
}

// newDeployEnv builds the temp tree, the patched script and the stubs.
func newDeployEnv(t *testing.T, o deployOpts) *deployEnv {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed; the deploy harness cannot run")
	}
	for _, tool := range []string{"tar", "sha256sum", "find", "mktemp", "install", "cmp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the deploy harness cannot run", tool)
		}
	}
	realInstall, _ := exec.LookPath("install")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot open a loopback listener for the health check")
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	d := &deployEnv{t: t, dir: t.TempDir(), port: l.Addr().(*net.TCPAddr).Port}
	script := readRepoFile(t, "deploy/vps/deploy.sh")
	for old, repl := range map[string]string{
		"readonly ROOT=/opt/nexus-hub\n":                         "readonly ROOT=" + d.path("opt") + "\n",
		"readonly ENV_FILE=/etc/nexus-hub/env\n":                 "readonly ENV_FILE=" + d.path("env") + "\n",
		"readonly UNIT_FILE=/etc/systemd/system/$UNIT.service\n": "readonly UNIT_FILE=" + d.path("systemd/nexus-hub.service") + "\n",
		"readonly HEALTH_PORT=8080\n":                            fmt.Sprintf("readonly HEALTH_PORT=%d\n", d.port),
		"readonly HEALTH_TIMEOUT=30\n":                           "readonly HEALTH_TIMEOUT=1\n",
		"[[ $EUID -eq 0 ]] || die \"must run as root\"\n":        ":\n",
	} {
		if !strings.Contains(script, old) {
			t.Fatalf("deploy.sh no longer contains %q; update the harness", old)
		}
		script = strings.Replace(script, old, repl, 1)
	}
	d.write("deploy.sh", script, 0o755)
	d.write("env", "DATABASE_URL=postgres://u:pw@127.0.0.1/db\n", 0o600)
	for _, dir := range []string{"opt/incoming", "opt/releases", "systemd", "stubs"} {
		if err := os.MkdirAll(d.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Stubs. systemctl is driven by flag files in the temp dir.
	d.write("stubs/systemctl", `#!/bin/bash
echo "$*" >> "`+d.path("calls.log")+`"
case "$1" in
  daemon-reload)
    [[ -e "`+d.path("fail-reload")+`" ]] && exit 1
    exit 0 ;;
  restart)
    if [[ -e "`+d.path("fail-restart-once")+`" ]]; then
      rm -f "`+d.path("fail-restart-once")+`" "`+d.path("active")+`"
      exit 1
    fi
    touch "`+d.path("active")+`"
    exit 0 ;;
  is-active)
    [[ -e "`+d.path("active")+`" ]]
    exit $? ;;
esac
exit 0
`, 0o755)
	d.write("stubs/install", `#!/bin/bash
args=()
while (($#)); do
  case "$1" in
    -o|-g) shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
exec `+realInstall+` "${args[@]}"
`, 0o755)
	d.write("stubs/chown", "#!/bin/sh\nexit 0\n", 0o755)
	d.write("stubs/runuser", "#!/bin/bash\n# runuser -u USER -- cmd args\nshift 3\nexec \"$@\"\n", 0o755)
	// Initial state.
	if o.installedUnit != "" {
		d.write("systemd/nexus-hub.service", o.installedUnit, 0o644)
	}
	if o.staleUnitPrev != "" {
		d.write("systemd/nexus-hub.service.prev", o.staleUnitPrev, 0o644)
	}
	if !o.noPrevious {
		prev := "opt/releases/" + sha1Prev
		unit := o.previousUnit
		if unit == "-" {
			unit = ""
		}
		d.releaseFiles(prev, sha1Prev, unit)
		if err := os.Symlink(d.path(prev), d.path("opt/current")); err != nil {
			t.Fatal(err)
		}
		d.write("active", "", 0o644)
	}
	if o.failReload {
		d.write("fail-reload", "", 0o644)
	}
	if o.failRestart1 {
		d.write("fail-restart-once", "", 0o644)
	}
	_ = bash
	return d
}

// pack builds the release tarball of the new release; it returns its path
// and sha256.
func (d *deployEnv) pack(sha, unit string) (string, string) {
	d.t.Helper()
	stage := d.path("stage-" + sha)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		d.t.Fatal(err)
	}
	d.releaseFiles("stage-"+sha, sha, unit)
	tarball := d.path("opt/incoming/nexus-hub-" + sha + ".tar.gz")
	if out, err := exec.Command("tar", "-C", stage, "-czf", tarball, ".").CombinedOutput(); err != nil {
		d.t.Fatalf("tar: %v %s", err, out)
	}
	sum, err := exec.Command("sha256sum", tarball).Output()
	if err != nil {
		d.t.Fatal(err)
	}
	return tarball, strings.Fields(string(sum))[0]
}

func (d *deployEnv) run(tarball, sum string) {
	d.t.Helper()
	cmd := exec.Command("bash", d.path("deploy.sh"), tarball, sum)
	cmd.Env = append(os.Environ(), "PATH="+d.path("stubs")+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	d.out = string(out)
	d.exit = 0
	if ee, ok := err.(*exec.ExitError); ok {
		d.exit = ee.ExitCode()
	} else if err != nil {
		d.t.Fatalf("run deploy.sh: %v", err)
	}
	if strings.Contains(d.out, "postgres://") || strings.Contains(d.out, "pw@") {
		d.t.Errorf("deploy output leaks DATABASE_URL:\n%s", d.out)
	}
}

func (d *deployEnv) deploy(newUnit string) {
	d.t.Helper()
	tarball, sum := d.pack(sha2New, newUnit)
	d.run(tarball, sum)
}

func (d *deployEnv) currentTarget() string {
	target, err := filepath.EvalSymlinks(d.path("opt/current"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func (d *deployEnv) failMsg() string {
	return fmt.Sprintf("exit=%d\n%s\ncalls=%v", d.exit, d.out, d.calls())
}

const (
	unitOld = "[Service]\nExecStart=/old\n"
	unitNew = "[Service]\nExecStart=/new\n"
)

// TC-B-51 (R-A-6): an unchanged unit is not reloaded and leaves no .prev;
// a stale .prev of an earlier deploy is removed. A changed unit is
// installed before the restart, after a reload, keeping the old one as .prev.
func TestPhase12bDeployUnitUnchangedAndChanged(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitNew, staleUnitPrev: "STALE\n", previousUnit: unitNew})
		d.deploy(unitNew)
		if d.exit != 0 || d.currentTarget() != sha2New {
			t.Fatalf("deploy failed: %s", d.failMsg())
		}
		if d.count("daemon-reload") != 0 || d.count("restart nexus-hub") != 1 {
			t.Fatalf("unchanged unit must restart without reload: %v", d.calls())
		}
		if _, ok := d.read("systemd/nexus-hub.service.prev"); ok {
			t.Fatal("stale .prev was not removed / .prev created for an unchanged unit")
		}
		if got, _ := d.read("systemd/nexus-hub.service"); got != unitNew {
			t.Fatalf("installed unit = %q", got)
		}
	})
	t.Run("changed", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		d.deploy(unitNew)
		if d.exit != 0 || d.currentTarget() != sha2New {
			t.Fatalf("deploy failed: %s", d.failMsg())
		}
		if got, _ := d.read("systemd/nexus-hub.service"); got != unitNew {
			t.Fatalf("installed unit = %q, want the new one", got)
		}
		if prev, ok := d.read("systemd/nexus-hub.service.prev"); !ok || prev != unitOld {
			t.Fatalf(".prev = %q %v, want the replaced unit", prev, ok)
		}
		if r, s := d.index("daemon-reload"), d.index("restart nexus-hub"); r < 0 || s < 0 || r > s || d.count("daemon-reload") != 1 {
			t.Fatalf("want one daemon-reload before restart: %v", d.calls())
		}
		if !strings.Contains(d.out, "healthy:") || !strings.Contains(d.out, "deployed "+sha2New) {
			t.Fatalf("no success message:\n%s", d.out)
		}
	})
	t.Run("first deploy without an installed unit", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{noPrevious: true})
		d.write("active", "", 0o644)
		d.deploy(unitNew)
		if d.exit != 0 {
			t.Fatalf("deploy failed: %s", d.failMsg())
		}
		if got, _ := d.read("systemd/nexus-hub.service"); got != unitNew {
			t.Fatalf("installed unit = %q", got)
		}
		if _, ok := d.read("systemd/nexus-hub.service.prev"); ok {
			t.Fatal(".prev created although no unit was replaced")
		}
	})
}

// TC-B-52 (R-A2-1): the restart fails: the deploy falls through to the
// health check, rolls back binary and unit, restarts the previous
// release, and exits non-zero.
func TestPhase12bDeployRestartFailureRollsBack(t *testing.T) {
	d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld, failRestart1: true})
	d.deploy(unitNew)
	if d.exit == 0 {
		t.Fatalf("deploy must fail: %s", d.failMsg())
	}
	if d.currentTarget() != sha1Prev {
		t.Fatalf("current -> %q, want rollback to %s\n%s", d.currentTarget(), sha1Prev, d.failMsg())
	}
	if got, _ := d.read("systemd/nexus-hub.service"); got != unitOld {
		t.Fatalf("installed unit = %q, want the previous unit back", got)
	}
	if d.count("restart nexus-hub") != 2 {
		t.Fatalf("want a restart of the new and of the rolled-back release: %v", d.calls())
	}
	if !strings.Contains(d.out, "rolled back current -> ") || !strings.Contains(d.out, "restart of nexus-hub failed") {
		t.Fatalf("rollback not logged:\n%s", d.out)
	}
	if _, err := os.Stat(d.path("active")); err != nil {
		t.Fatal("service is not running after the rollback restart")
	}
}

// TC-B-53 (R-A2-2): daemon-reload fails on the forward install: the binary
// switches back, the replaced unit is restored, and nothing is restarted.
func TestPhase12bDeployReloadFailureRestoresUnit(t *testing.T) {
	d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld, failReload: true})
	d.deploy(unitNew)
	if d.exit == 0 {
		t.Fatalf("deploy must fail: %s", d.failMsg())
	}
	if d.currentTarget() != sha1Prev {
		t.Fatalf("current -> %q, want %s\n%s", d.currentTarget(), sha1Prev, d.failMsg())
	}
	if got, _ := d.read("systemd/nexus-hub.service"); got != unitOld {
		t.Fatalf("installed unit = %q, want the old unit restored", got)
	}
	if d.count("restart nexus-hub") != 0 {
		t.Fatalf("service restarted after a failed install: %v", d.calls())
	}
	if !strings.Contains(d.out, "could not install") {
		t.Fatalf("failure not reported:\n%s", d.out)
	}
}

// TC-B-54 (R-A2-3): rolling back to a release that ships no unit restores
// the pre-deploy unit from .prev.
func TestPhase12bDeployRollbackToUnitlessReleaseRestoresPrev(t *testing.T) {
	d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: "-", failRestart1: true})
	d.deploy(unitNew)
	if d.exit == 0 {
		t.Fatalf("deploy must fail: %s", d.failMsg())
	}
	if d.currentTarget() != sha1Prev {
		t.Fatalf("current -> %q\n%s", d.currentTarget(), d.failMsg())
	}
	if got, _ := d.read("systemd/nexus-hub.service"); got != unitOld {
		t.Fatalf("installed unit = %q, want the pre-deploy unit restored", got)
	}
	if _, ok := d.read("systemd/nexus-hub.service.prev"); ok {
		t.Fatal(".prev must be consumed by the restore")
	}
	if d.count("restart nexus-hub") != 2 {
		t.Fatalf("calls = %v", d.calls())
	}
}

// TC-B-55: a release without a unit keeps the installed unit and deploys.
func TestPhase12bDeployReleaseWithoutUnitKeepsInstalledUnit(t *testing.T) {
	d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
	d.deploy("")
	if d.exit != 0 || d.currentTarget() != sha2New {
		t.Fatalf("deploy failed: %s", d.failMsg())
	}
	if got, _ := d.read("systemd/nexus-hub.service"); got != unitOld {
		t.Fatalf("installed unit = %q, want it kept", got)
	}
	if d.count("daemon-reload") != 0 {
		t.Fatalf("unexpected reload: %v", d.calls())
	}
	if _, ok := d.read("systemd/nexus-hub.service.prev"); ok {
		t.Fatal(".prev created without a replaced unit")
	}
	if !strings.Contains(d.out, "ships no unit file") {
		t.Fatalf("missing log line:\n%s", d.out)
	}
}

// TC-B-55 (abuse): bad input is refused before anything changes.
func TestPhase12bDeployRefusesBadInputBeforeAnyChange(t *testing.T) {
	check := func(t *testing.T, d *deployEnv, label string) {
		t.Helper()
		if d.exit == 0 {
			t.Fatalf("%s: deploy must fail:\n%s", label, d.out)
		}
		if d.currentTarget() != sha1Prev || len(d.calls()) != 0 {
			t.Fatalf("%s: state changed: current=%s calls=%v", label, d.currentTarget(), d.calls())
		}
		if got, _ := d.read("systemd/nexus-hub.service"); got != unitOld {
			t.Fatalf("%s: unit changed", label)
		}
	}
	t.Run("wrong checksum", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		tarball, _ := d.pack(sha2New, unitNew)
		d.run(tarball, strings.Repeat("0", 64))
		check(t, d, "checksum")
		if !strings.Contains(d.out, "checksum mismatch") {
			t.Fatalf("output:\n%s", d.out)
		}
	})
	t.Run("malformed checksum", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		tarball, _ := d.pack(sha2New, unitNew)
		d.run(tarball, "not-a-checksum; rm -rf /")
		check(t, d, "malformed checksum")
	})
	t.Run("tarball name without a 40-hex sha", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		tarball, sum := d.pack(sha2New, unitNew)
		bad := d.path("opt/incoming/nexus-hub-latest.tar.gz")
		if err := os.Rename(tarball, bad); err != nil {
			t.Fatal(err)
		}
		d.run(bad, sum)
		check(t, d, "bad name")
	})
	t.Run("tarball outside the incoming directory", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		tarball, sum := d.pack(sha2New, unitNew)
		elsewhere := d.path("nexus-hub-" + sha2New + ".tar.gz")
		if err := os.Rename(tarball, elsewhere); err != nil {
			t.Fatal(err)
		}
		d.run(elsewhere, sum)
		check(t, d, "outside incoming")
	})
	t.Run("symlinked tarball", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		tarball, sum := d.pack(sha2New, unitNew)
		real := d.path("real.tar.gz")
		if err := os.Rename(tarball, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, tarball); err != nil {
			t.Fatal(err)
		}
		d.run(tarball, sum)
		check(t, d, "symlink")
	})
	t.Run("REVISION does not match the file name", func(t *testing.T) {
		d := newDeployEnv(t, deployOpts{installedUnit: unitOld, previousUnit: unitOld})
		stage := d.path("stage-x")
		if err := os.MkdirAll(stage, 0o755); err != nil {
			t.Fatal(err)
		}
		d.releaseFiles("stage-x", "3333333333333333333333333333333333333333", unitNew)
		tarball := d.path("opt/incoming/nexus-hub-" + sha2New + ".tar.gz")
		if out, err := exec.Command("tar", "-C", stage, "-czf", tarball, ".").CombinedOutput(); err != nil {
			t.Fatalf("tar: %v %s", err, out)
		}
		sumOut, _ := exec.Command("sha256sum", tarball).Output()
		d.run(tarball, strings.Fields(string(sumOut))[0])
		check(t, d, "revision mismatch")
	})
}
