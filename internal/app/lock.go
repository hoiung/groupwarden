package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyRunning: another groupwarden holds the data dir. Two connections
// on one WhatsApp session knock each other off.
var ErrAlreadyRunning = errors.New("another groupwarden is running")

const lockName = "groupwarden.lock"

// Lock holds the single-instance lock on a data dir.
type Lock struct{ f *os.File }

// AcquireLock takes the exclusive lock every WhatsApp-connected command
// needs (pair, run, groups, resolve-link). It fails at once if it is held.
// The lock file then holds "<pid> <command>", so the config sync knows which
// process to ask for a reload (only `run` reloads).
func AcquireLock(dataDir, command string) (*Lock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dataDir, lockName), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- fixed name in the configured data dir
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock data dir: %w", err)
	}
	l := &Lock{f: f}
	if err := f.Truncate(0); err != nil {
		l.Release()
		return nil, fmt.Errorf("write lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+" "+command+"\n"), 0); err != nil {
		l.Release()
		return nil, fmt.Errorf("write lock file: %w", err)
	}
	return l, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits in int
		_ = l.f.Close()
		l.f = nil
	}
}

// LockHolder reports which groupwarden holds dataDir's lock: held is false
// when none does (the file may still name the last holder).
func LockHolder(dataDir string) (pid int, command string, held bool, err error) {
	f, err := os.Open(filepath.Join(dataDir, lockName)) // #nosec G304 -- fixed name in the configured data dir
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	defer func() { _ = f.Close() }()
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); { // #nosec G115 -- a file descriptor fits in int
	case err == nil:
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits in int
		return 0, "", false, nil
	case !errors.Is(err, syscall.EWOULDBLOCK):
		return 0, "", false, fmt.Errorf("read lock: %w", err)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return 0, "", true, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, "", true, fmt.Errorf("the lock file reads %q, not \"<pid> <command>\"", strings.TrimSpace(string(raw)))
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, "", true, fmt.Errorf("the lock file names no process: %q", fields[0])
	}
	return pid, fields[1], true, nil
}
