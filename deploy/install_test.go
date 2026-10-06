// Package deploy holds the tests of the install script (deploy/install.sh):
// they run it for real against a scratch root (GW_ROOT), a fake systemctl
// and the real groupwarden binary.
package deploy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/store"
)

type node struct {
	t                           *testing.T
	root, good, ctlLog, sudoLog string
	systemctl, sudo             string
	script                      string // the install script run ("" = this directory's)
}

// newNode builds the binary and a scratch root holding a live config and a
// groupwarden.db whose status reads healthy. Its sudo logs each call and runs
// the command as the test user (dropping `-u <user>`).
func newNode(t *testing.T) *node {
	t.Helper()
	tmp := t.TempDir()
	n := &node{t: t, root: filepath.Join(tmp, "root"), good: filepath.Join(tmp, "groupwarden"),
		ctlLog: filepath.Join(tmp, "systemctl.log"), systemctl: filepath.Join(tmp, "systemctl"),
		sudoLog: filepath.Join(tmp, "sudo.log"), sudo: filepath.Join(tmp, "sudo")}
	if out, err := exec.Command("go", "build", "-o", n.good, "../cmd/groupwarden").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	n.write("systemctl", "#!/bin/sh\necho \"$*\" >> "+n.ctlLog+"\n[ \"$1\" = is-enabled ] && exit \"${ENABLED_EXIT:-0}\"\nexit 0\n", 0o755)
	n.write("sudo", "#!/bin/sh\necho \"$*\" >> "+n.sudoLog+"\n[ \"$1\" = -u ] && shift 2\nexec \"$@\"\n", 0o755)
	state := filepath.Join(n.root, "var/lib/groupwarden")
	n.mkdir("var/lib/groupwarden/config")
	n.mkdir("srv/backup")
	n.write("root/var/lib/groupwarden/config/config.yaml", "data_dir: "+state+"\nsecrets_file: "+
		filepath.Join(n.root, "etc/groupwarden/secrets.env")+"\nconfig_sync_minutes: 15\nbackup:\n  target_dir: "+
		filepath.Join(n.root, "srv/backup")+"\n", 0o600)
	st, err := store.Open(context.Background(), filepath.Join(state, "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetStatus(context.Background(), map[string]string{store.StatusHeartbeat: "", store.StatusConnected: "1",
		store.StatusDeaf: "0", store.StatusConfigHash: "55786e8be51a", store.StatusTelegramOK: "1"}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (n *node) mkdir(rel string) {
	n.t.Helper()
	if err := os.MkdirAll(filepath.Join(n.root, rel), 0o700); err != nil {
		n.t.Fatal(err)
	}
}

// write writes a file under the scratch dir (rel is relative to root's parent).
func (n *node) write(rel, body string, mode os.FileMode) {
	n.t.Helper()
	p := filepath.Join(filepath.Dir(n.root), rel)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		n.t.Fatal(err)
	}
}

// install runs the script and returns its exit code and output.
func (n *node) install(env []string, args ...string) (int, string) {
	n.t.Helper()
	script := n.script
	if script == "" {
		script = "install.sh"
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"GW_ROOT=" + n.root, "SUDO=" + n.sudo, "SYSTEMCTL=" + n.systemctl,
		"GW_HEALTH_WAIT=4"}, env...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		n.t.Fatal(err)
	}
	return code, out.String()
}

func (n *node) read(rel string) string {
	n.t.Helper()
	b, err := os.ReadFile(filepath.Join(n.root, rel))
	if err != nil {
		n.t.Fatal(err)
	}
	return string(b)
}

func (n *node) calls() string {
	b, _ := os.ReadFile(n.ctlLog)
	return string(b)
}

// TestInstallRendersRestartsAndRollsBack: a first install puts the binary and
// units in place, renders the sync timer from config_sync_minutes and the
// backup unit's write access from backup.target_dir, restarts the service
// and passes the health check; an upgrade to a binary that fails the health
// check is rolled back to the previous binary (exit 1); nothing is restarted
// while the service is not enabled; and with no live config yet the units
// keep their defaults.
func TestInstallRendersRestartsAndRollsBack(t *testing.T) {
	n := newNode(t)
	code, out := n.install(nil, "--binary", n.good)
	if code != 0 || !strings.Contains(out, "groupwarden is healthy") {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(n.root, "usr/local/bin/groupwarden.previous")); !os.IsNotExist(err) {
		t.Fatal("a first install kept a previous binary")
	}
	timer := n.read("etc/systemd/system/groupwarden-sync.timer")
	if !strings.Contains(timer, "\nOnCalendar=*-*-* *:00/15:00\n") || strings.Count(timer, "OnCalendar=") != 1 {
		t.Fatalf("sync timer not rendered from config_sync_minutes 15:\n%s", timer)
	}
	if got, want := n.read("etc/systemd/system/groupwarden-backup.service.d/target.conf"),
		"[Service]\nReadWritePaths="+filepath.Join(n.root, "srv/backup")+"\n"; got != want {
		t.Fatalf("backup drop-in %q; want %q", got, want)
	}
	for _, u := range []string{"groupwarden.service", "groupwarden-sync.service", "groupwarden-backup.service",
		"groupwarden-backup.timer"} {
		if got, want := n.read("etc/systemd/system/"+u), readFile(t, u); got != want {
			t.Fatalf("%s installed differs from deploy/%s", u, u)
		}
	}
	for _, c := range []string{"daemon-reload", "restart groupwarden.service",
		"try-restart groupwarden-sync.timer groupwarden-backup.timer"} {
		if !strings.Contains(n.calls(), c+"\n") {
			t.Fatalf("systemctl %s not called:\n%s", c, n.calls())
		}
	}
	// The state directory is the service user's alone (0700): the live config
	// is looked for, read and health-checked as that user, not as the operator.
	live := filepath.Join(n.root, "var/lib/groupwarden/config/config.yaml")
	bin := filepath.Join(n.root, "usr/local/bin/groupwarden")
	sudoLog := readFile(t, n.sudoLog)
	for _, c := range []string{"-u groupwarden test -f " + live, "-u groupwarden " + bin + " schedule sync --config " + live,
		"-u groupwarden " + bin + " backup-dir --config " + live, "-u groupwarden " + bin + " healthcheck --config " + live} {
		if !strings.Contains(sudoLog, c+"\n") {
			t.Fatalf("not run as the service user: %s\n%s", c, sudoLog)
		}
	}

	// An upgrade that fails the health check is rolled back.
	bad := filepath.Join(t.TempDir(), "groupwarden-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\n[ \"$1\" = healthcheck ] && { echo 'running: no'; exit 1; }\nexec "+
		n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out = n.install(nil, "--binary", bad)
	if code != 1 || !strings.Contains(out, "ROLLED BACK to the previous binary and units; it is healthy") {
		t.Fatalf("unhealthy upgrade: exit %d\n%s", code, out)
	}
	if got, want := n.read("usr/local/bin/groupwarden"), readFile(t, n.good); got != want {
		t.Fatal("the rolled-back binary is not the previous one")
	}
	if c := strings.Count(n.calls(), "restart groupwarden.service\n"); c != 3 {
		t.Fatalf("%d restarts after install, upgrade and rollback; want 3:\n%s", c, n.calls())
	}

	// Not enabled yet: installed, nothing restarted.
	before := strings.Count(n.calls(), "restart groupwarden.service\n")
	code, out = n.install([]string{"ENABLED_EXIT=1"}, "--binary", n.good)
	if code != 0 || !strings.Contains(out, "not enabled yet") ||
		strings.Count(n.calls(), "restart groupwarden.service\n") != before {
		t.Fatalf("install while not enabled: exit %d\n%s\n%s", code, out, n.calls())
	}

	// No live config yet (before the first sync): defaults, no drop-in.
	if err := os.Remove(filepath.Join(n.root, "var/lib/groupwarden/config/config.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(n.root, "etc/systemd/system")); err != nil {
		t.Fatal(err)
	}
	code, out = n.install([]string{"ENABLED_EXIT=1"}, "--binary", n.good)
	if code != 0 || !strings.Contains(out, "no live config") {
		t.Fatalf("install before the first sync: exit %d\n%s", code, out)
	}
	if n.read("etc/systemd/system/groupwarden-sync.timer") != readFile(t, "groupwarden-sync.timer") {
		t.Fatal("the sync timer was rendered without a config")
	}
	if _, err := os.Stat(filepath.Join(n.root, "etc/systemd/system/groupwarden-backup.service.d")); !os.IsNotExist(err) {
		t.Fatal("a backup drop-in was written without a config")
	}

	// A binary whose fatal exit code is not the one the units expect is refused.
	odd := filepath.Join(t.TempDir(), "groupwarden-odd")
	if err := os.WriteFile(odd, []byte("#!/bin/sh\n[ \"$1\" = fatal-exit-code ] && { echo 77; exit 0; }\nexec "+
		n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := n.install(nil, "--binary", odd); code != 1 || !strings.Contains(out, "the units expect 78") {
		t.Fatalf("fatal-exit-code 77: exit %d\n%s", code, out)
	}

	for _, args := range [][]string{nil, {"--binary"}, {"--version", "x", "--binary", "y"}, {"--frobnicate"}} {
		if code, out := n.install(nil, args...); code != 2 {
			t.Fatalf("install %v: exit %d; want usage (2)\n%s", args, code, out)
		}
	}
}

// TestInstallBuildsThePinnedVersion: --version builds that commit of the
// clone the script sits in (from a throwaway clone, so the binary carries its
// commit, and the units come from that commit) and refuses a version the
// clone does not have. The clone is a scratch repo holding this source.
func TestInstallBuildsThePinnedVersion(t *testing.T) {
	src := t.TempDir()
	for _, p := range []string{"go.mod", "go.sum", "cmd", "internal", "schema", "tests", "deploy"} {
		if out, err := exec.Command("cp", "-r", filepath.Join("..", p), src).CombinedOutput(); err != nil {
			t.Fatalf("copy %s: %v\n%s", p, err, out)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", src, "-c", "user.name=test", "-c",
			"user.email=test@" + "example.invalid"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("add", "-A")
	git("commit", "--quiet", "-m", "release")
	commit := git("rev-parse", "HEAD")
	n := newNode(t)
	n.script = filepath.Join(src, "deploy/install.sh")
	code, out := n.install([]string{"ENABLED_EXIT=1"}, "--version", commit)
	if code != 0 || !strings.Contains(out, "building "+commit) {
		t.Fatalf("install --version %s: exit %d\n%s", commit, code, out)
	}
	ver, err := exec.Command("go", "version", "-m", filepath.Join(n.root, "usr/local/bin/groupwarden")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ver), "vcs.revision="+commit) {
		t.Fatalf("the installed binary does not carry commit %s:\n%s", commit, ver)
	}
	if code, out := n.install(nil, "--version", "no-such-tag-"+time.Now().Format("150405")); code != 1 ||
		!strings.Contains(out, "no such version") {
		t.Fatalf("unknown version: exit %d\n%s", code, out)
	}
}

// readFile reads path (relative paths: this directory, deploy/).
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
