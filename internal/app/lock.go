package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrAlreadyRunning: another groupwarden holds the data dir. Two connections
// on one WhatsApp session knock each other off.
var ErrAlreadyRunning = errors.New("another groupwarden is running")

// Lock holds the single-instance lock on a data dir.
type Lock struct{ f *os.File }

// AcquireLock takes the exclusive lock every WhatsApp-connected command
// needs (pair, run, groups, resolve-link). It fails at once if it is held.
func AcquireLock(dataDir string) (*Lock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "groupwarden.lock"), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- fixed name in the configured data dir
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
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits in int
		_ = l.f.Close()
		l.f = nil
	}
}
