package store

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// windowsFSTypes are the filesystem types a Windows drive mounted into WSL
// shows up as. SQLite's locking is unreliable there, so groupwarden refuses
// to keep its databases on them.
var windowsFSTypes = map[string]bool{"9p": true, "drvfs": true}

// CheckLinuxFS refuses path when it lives on a Windows-mounted filesystem,
// judged by the filesystem type of the mount holding it (not its prefix).
func CheckLinuxFS(path string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mount table: %w", err)
	}
	defer f.Close()
	return checkFS(path, f)
}

func checkFS(path string, mountinfo io.Reader) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	point, fstype, opts, err := mountFor(mountinfo, abs)
	if err != nil {
		return err
	}
	if windowsFSTypes[fstype] || strings.Contains(opts, "aname=drvfs") {
		return fmt.Errorf("%s is on a Windows-mounted filesystem (%s at %s); keep groupwarden's data on the Linux filesystem", path, fstype, point)
	}
	return nil
}

// mountFor finds the longest mount point containing abs in a mountinfo table
// and returns that mount point, its filesystem type and its super options.
func mountFor(r io.Reader, abs string) (point, fstype, superOpts string, err error) {
	best := -1
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// Format: id parent major:minor root mountpoint opts [optional...] - fstype source superopts
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		tail := strings.Fields(post)
		if len(fields) < 5 || len(tail) < 1 {
			continue
		}
		mp := unescapeMount(fields[4])
		if !within(abs, mp) || len(mp) <= best {
			continue
		}
		best = len(mp)
		point, fstype = mp, tail[0]
		superOpts = ""
		if len(tail) >= 3 {
			superOpts = tail[2]
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", "", fmt.Errorf("read mount table: %w", err)
	}
	if best < 0 {
		return "", "", "", fmt.Errorf("no mount holds %s", abs)
	}
	return point, fstype, superOpts, nil
}

func within(path, mountPoint string) bool {
	if mountPoint == "/" {
		return true
	}
	return path == mountPoint || strings.HasPrefix(path, mountPoint+"/")
}

// unescapeMount decodes the octal escapes mountinfo uses for spaces and tabs.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
