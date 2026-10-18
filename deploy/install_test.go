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
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/store"
)

type node struct {
	t                           *testing.T
	root, good, ctlLog, sudoLog string
	systemctl, sudo             string
	backup                      string // backup.target_dir
	script                      string // the install script run ("" = this directory's)
	dir                         string // the directory it runs in ("" = this one)
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
	// `groupwarden backup` refuses a target on the data dir's disk: /dev/shm
	// is a mount of its own.
	backupDir, err := os.MkdirTemp("/dev/shm", "groupwarden-backup-")
	if err != nil {
		t.Fatalf("a backup target on another disk: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(backupDir) })
	n.backup = backupDir
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	n.write("root/var/lib/groupwarden/config/config.yaml", "data_dir: "+state+"\nsecrets_file: "+
		filepath.Join(n.root, "etc/groupwarden/secrets.env")+"\nconfig_sync_minutes: 15\nbackup:\n  target_dir: "+
		n.backup+"\n  age_recipient: "+id.Recipient().String()+"\n", 0o600)
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
	script, err := filepath.Abs(script)
	if err != nil {
		n.t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = n.dir
	cmd.Env = append(os.Environ(), append([]string{"GW_ROOT=" + n.root, "SUDO=" + n.sudo, "SYSTEMCTL=" + n.systemctl,
		"GW_HEALTH_WAIT=4"}, env...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
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
		"[Service]\nReadWritePaths=-"+n.backup+"\n"; got != want {
		t.Fatalf("backup drop-in %q; want %q", got, want)
	}
	for _, u := range []string{"groupwarden.service", "groupwarden-sync.service", "groupwarden-backup.service",
		"groupwarden-backup.timer", "groupwarden-backup-failed.service"} {
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
	// Healthy again: nothing to restore, so the backup is not offered.
	if code != 1 || !strings.Contains(out, "ROLLED BACK to the previous binary and units; it is healthy") ||
		strings.Contains(out, "restore the backup") {
		t.Fatalf("unhealthy upgrade: exit %d\n%s", code, out)
	}
	// The previous binary took a backup before the restart (the first install
	// had nothing before it to take one).
	backedUp := strings.Index(out, "backup before the restart: ok ")
	if backedUp < 0 || backedUp > strings.Index(out, "restarted groupwarden.service") ||
		!strings.Contains(readFile(t, n.sudoLog), "-u groupwarden "+bin+".previous backup --config "+live+"\n") ||
		strings.Count(readFile(t, n.sudoLog), " backup --config ") != 1 {
		t.Fatalf("no backup by the previous binary before the restart:\n%s\n%s", out, readFile(t, n.sudoLog))
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
	// The same file named bare, run from its own directory while a good
	// groupwarden is first on PATH: the check runs that file, not the PATH one.
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "groupwarden"), []byte("#!/bin/sh\n[ \"$1\" = fatal-exit-code ] && { echo 77; exit 0; }\nexec "+
		n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.dir = bare
	code, out = n.install([]string{"PATH=" + filepath.Dir(n.good) + ":" + os.Getenv("PATH")}, "--binary", "groupwarden")
	n.dir = ""
	if code != 1 || !strings.Contains(out, "the units expect 78") {
		t.Fatalf("a bare --binary name checked another file: exit %d\n%s", code, out)
	}

	for _, args := range [][]string{nil, {"--binary"}, {"--version", "x", "--binary", "y"}, {"--frobnicate"}} {
		if code, out := n.install(nil, args...); code != 2 {
			t.Fatalf("install %v: exit %d; want usage (2)\n%s", args, code, out)
		}
	}
}

// TestInstallPutsBackWhenTheNewBinaryRejectsTheConfig: an upgrade whose
// binary rejects the live config (it cannot render the sync timer from it)
// fails with the previous binary and units back and nothing restarted, so
// the next restart, timer run or install never picks up the failed build.
func TestInstallPutsBackWhenTheNewBinaryRejectsTheConfig(t *testing.T) {
	n := newNode(t)
	if code, out := n.install(nil, "--binary", n.good); code != 0 {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	restarts := strings.Count(n.calls(), "restart groupwarden.service\n")
	strict := filepath.Join(t.TempDir(), "groupwarden-strict")
	if err := os.WriteFile(strict, []byte("#!/bin/sh\n[ \"$1\" = schedule ] && { echo 'config: unknown key' >&2; exit 1; }\nexec "+
		n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := n.install(nil, "--binary", strict)
	if code != 1 || !strings.Contains(out, "the previous binary and units are back in place") {
		t.Fatalf("an upgrade that rejects the live config: exit %d\n%s", code, out)
	}
	if got, want := n.read("usr/local/bin/groupwarden"), readFile(t, n.good); got != want {
		t.Fatal("the failed build was left installed")
	}
	if c := strings.Count(n.calls(), "restart groupwarden.service\n"); c != restarts {
		t.Fatalf("restarted after a failed install:\n%s", n.calls())
	}
}

// TestInstallBacksUpBeforeTheRestart: an upgrade whose new binary moves the
// database to a schema the previous binary refuses ends rolled back but down,
// and says which backup (taken before the restart) to restore; a backup that
// fails stops the install before anything restarts.
func TestInstallBacksUpBeforeTheRestart(t *testing.T) {
	n := newNode(t)
	moved := filepath.Join(t.TempDir(), "schema-moved")
	prev := filepath.Join(t.TempDir(), "groupwarden-prev")
	if err := os.WriteFile(prev, []byte("#!/bin/sh\n[ \"$1\" = healthcheck ] && [ -f "+moved+" ] && "+
		"{ echo 'groupwarden.db is at schema version 11 but this binary knows only 10'; exit 1; }\nexec "+
		n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := n.install(nil, "--binary", prev); code != 0 {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	newer := filepath.Join(t.TempDir(), "groupwarden-newer")
	if err := os.WriteFile(newer, []byte("#!/bin/sh\n[ \"$1\" = healthcheck ] && { touch "+moved+"; echo 'running: no'; exit 1; }\nexec "+
		n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := n.install(nil, "--binary", newer)
	if code != 1 || !strings.Contains(out, "ROLLED BACK, but the previous binary is not healthy either") ||
		!strings.Contains(out, "restore the backup taken before the restart (ok groupwarden-") {
		t.Fatalf("a rollback the previous binary cannot run: exit %d\n%s", code, out)
	}
	files, err := os.ReadDir(n.backup)
	if err != nil || len(files) != 1 {
		t.Fatalf("backups %v %v", files, err)
	}

	// A backup that fails stops the install: nothing restarts and the
	// installed binary stays the previous one.
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(n.backup); err != nil {
		t.Fatal(err)
	}
	restarts := strings.Count(n.calls(), "restart groupwarden.service\n")
	code, out = n.install(nil, "--binary", n.good)
	if code != 1 || !strings.Contains(out, "the backup before the restart failed") ||
		strings.Count(n.calls(), "restart groupwarden.service\n") != restarts {
		t.Fatalf("a failed backup: exit %d\n%s\n%s", code, out, n.calls())
	}
	if got, want := n.read("usr/local/bin/groupwarden"), readFile(t, prev); got != want {
		t.Fatal("the new binary was left installed after a failed backup")
	}
}

// TestInstallWithNoDatabaseYet: with a previous binary but no groupwarden.db
// (nothing has run yet), there is nothing to back up: the install goes on to
// the restart instead of stopping.
func TestInstallWithNoDatabaseYet(t *testing.T) {
	n := newNode(t)
	if code, out := n.install(nil, "--binary", n.good); code != 0 {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(n.root, "var/lib/groupwarden/groupwarden.db")); err != nil {
		t.Fatal(err)
	}
	restarts := strings.Count(n.calls(), "restart groupwarden.service\n")
	_, out := n.install(nil, "--binary", n.good)
	if !strings.Contains(out, "no groupwarden.db yet: nothing to back up before the restart") ||
		strings.Contains(out, "the backup before the restart failed") ||
		strings.Count(n.calls(), "restart groupwarden.service\n") == restarts {
		t.Fatalf("no database yet: the install did not go on to the restart:\n%s\n%s", out, n.calls())
	}
}

// TestInstallStopsOnAFailedBackup: a backup that fails for a reason naming the
// database (not that there is none yet) stops the install too, with the
// previous units back as well as the previous binary.
func TestInstallStopsOnAFailedBackup(t *testing.T) {
	n := newNode(t)
	failing := filepath.Join(t.TempDir(), "groupwarden-prev")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\n[ \"$1\" = backup ] && "+
		"{ echo 'FAILED: copy groupwarden.db: no space left on device'; exit 1; }\nexec "+n.good+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := n.install(nil, "--binary", failing); code != 0 {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	// The previous version's unit differs from the one this version installs.
	previous := n.read("etc/systemd/system/groupwarden.service") + "# the previous version's unit\n"
	if err := os.WriteFile(filepath.Join(n.root, "etc/systemd/system/groupwarden.service"), []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	restarts := strings.Count(n.calls(), "restart groupwarden.service\n")
	code, out := n.install(nil, "--binary", n.good)
	if code != 1 || !strings.Contains(out, "the backup before the restart failed") ||
		strings.Count(n.calls(), "restart groupwarden.service\n") != restarts {
		t.Fatalf("a failed backup: exit %d\n%s\n%s", code, out, n.calls())
	}
	if n.read("etc/systemd/system/groupwarden.service") != previous {
		t.Fatal("the previous unit was not put back after a failed backup")
	}
}

// TestInstallEnabledWithNoLiveConfig: with the service enabled but no live
// config (moved away by hand), the previous binary has no config to back up
// with, so the install does not try.
func TestInstallEnabledWithNoLiveConfig(t *testing.T) {
	n := newNode(t)
	if code, out := n.install(nil, "--binary", n.good); code != 0 {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(n.root, "var/lib/groupwarden/config/config.yaml")); err != nil {
		t.Fatal(err)
	}
	_, out := n.install(nil, "--binary", n.good)
	if strings.Contains(out, "the backup before the restart failed") || strings.Count(readFile(t, n.sudoLog), " backup --config ") != 0 {
		t.Fatalf("a backup was tried with no live config:\n%s\n%s", out, readFile(t, n.sudoLog))
	}
}

// TestBackupUnitHasAFailureHandler: a backup unit that fails before
// `groupwarden backup` records anything (systemd could not open the target,
// the time limit) starts groupwarden-backup-failed.service, which runs
// `backup-failed` and has no access to the target, so it starts whatever
// state the target is in.
func TestBackupUnitHasAFailureHandler(t *testing.T) {
	directives := func(unit string) []string {
		var d []string
		for _, l := range strings.Split(readFile(t, unit), "\n") {
			if l != "" && !strings.HasPrefix(l, "#") {
				d = append(d, l)
			}
		}
		return d
	}
	backup := strings.Join(directives("groupwarden-backup.service"), "\n")
	if head, _, _ := strings.Cut(backup, "[Service]"); !strings.Contains(head, "\nOnFailure=groupwarden-backup-failed.service\n") {
		t.Fatalf("the backup unit's [Unit] section starts no failure handler:\n%s", head)
	}
	handler := directives("groupwarden-backup-failed.service")
	run := "ExecStart=/usr/local/bin/groupwarden backup-failed --config /var/lib/groupwarden/config/config.yaml"
	if !slices.Contains(handler, run) {
		t.Fatalf("the failure handler does not run backup-failed:\n%s", strings.Join(handler, "\n"))
	}
	for _, d := range handler {
		if strings.HasPrefix(d, "ReadWritePaths=") || strings.HasPrefix(d, "RequiresMountsFor=") {
			t.Fatalf("the failure handler depends on the target (%s): it would fail with the backup", d)
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
