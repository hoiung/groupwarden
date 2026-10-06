// Package backup makes the nightly encrypted copy of groupwarden.db and
// restores one.
//
// Only groupwarden.db is copied (the ledger, ban list, reports, status). The
// WhatsApp session store (whatsmeow.db) never is: it holds the bot's device
// keys, and a restored copy would be an old session WhatsApp has moved past;
// after a loss the bot is paired again instead (docs/runbook.md).
//
// A backup file goes through three states, so a crash never leaves a
// half-written file under a backup's name:
//
//	(none) ──write──► groupwarden-<UTC time>.db.age.partial ──fsync, rename──► groupwarden-<UTC time>.db.age
//	   a .partial left by a crash is deleted by the next run
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/store"
)

const (
	prefix   = "groupwarden-"
	suffix   = ".db.age"
	partial  = ".partial"
	stampFmt = "20060102T150405Z"
)

// backupName matches a finished backup (anything else in the target
// directory is never touched).
var backupName = regexp.MustCompile(`^groupwarden-\d{8}T\d{6}Z\.db\.age$`)

// Options says where a backup goes and how many are kept.
type Options struct {
	TargetDir string // backup.target_dir
	Recipient string // backup.age_recipient: the age public key
	Keep      int    // backup.keep: finished backups kept, newest first
	// ScratchDir holds the plaintext copy while it is encrypted: the data
	// dir (the same disk and permissions as groupwarden.db itself).
	ScratchDir string
	Now        time.Time
	Log        *slog.Logger
}

// Result is what one backup did.
type Result struct {
	File    string   // the new backup's path
	Bytes   int64    // its size
	Removed []string // older backups (and crash leftovers) deleted
}

// Run copies groupwarden.db while the bot keeps using it, encrypts the copy
// to the age recipient into the target directory, and keeps the newest Keep
// backups. The copy carries a restore pause (store.Snapshot), so a bot
// restored from it starts with every action paused.
func Run(ctx context.Context, st *store.Store, o Options) (Result, error) {
	if o.Keep < 1 {
		return Result{}, fmt.Errorf("backup.keep is %d; it must be at least 1", o.Keep)
	}
	rcpt, err := age.ParseX25519Recipient(o.Recipient)
	if err != nil {
		return Result{}, fmt.Errorf("backup.age_recipient: %w", err)
	}
	scratch, err := os.MkdirTemp(o.ScratchDir, ".backup-*")
	if err != nil {
		return Result{}, fmt.Errorf("scratch directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			o.Log.Error("backup: could not delete the plaintext copy", "dir", scratch, "err", err)
		}
	}()
	plain := filepath.Join(scratch, "groupwarden.db")
	if err := st.Snapshot(ctx, plain); err != nil {
		return Result{}, err
	}
	final := filepath.Join(o.TargetDir, prefix+o.Now.UTC().Format(stampFmt)+suffix)
	if _, err := os.Lstat(final); err == nil {
		return Result{}, fmt.Errorf("%s already exists", final)
	}
	n, err := encrypt(plain, final+partial, rcpt)
	if err != nil {
		_ = os.Remove(final + partial)
		return Result{}, err
	}
	if err := os.Rename(final+partial, final); err != nil {
		_ = os.Remove(final + partial)
		return Result{}, fmt.Errorf("finish %s: %w", final, err)
	}
	if err := syncDir(o.TargetDir); err != nil {
		return Result{}, err
	}
	res := Result{File: final, Bytes: n}
	res.Removed, err = prune(o.TargetDir, o.Keep, filepath.Base(final))
	for _, f := range res.Removed {
		o.Log.Info("backup: deleted", "file", f)
	}
	if err != nil {
		return res, fmt.Errorf("the backup was written, but older ones could not be deleted: %w", err)
	}
	o.Log.Info("backup written", "file", final, "bytes", n, "kept", o.Keep)
	return res, nil
}

// encrypt writes plain, age-encrypted to rcpt, to dst (created; it must not
// exist) and syncs it to disk.
func encrypt(plain, dst string, rcpt age.Recipient) (int64, error) {
	in, err := os.Open(plain) // #nosec G304 -- the snapshot this package just wrote
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- a fixed name in backup.target_dir
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}
	w, err := age.Encrypt(out, rcpt)
	if err != nil {
		_ = out.Close()
		return 0, err
	}
	if _, err := io.Copy(w, in); err != nil {
		_ = out.Close()
		return 0, fmt.Errorf("encrypt: %w", err)
	}
	if err := w.Close(); err != nil {
		_ = out.Close()
		return 0, fmt.Errorf("encrypt: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return 0, fmt.Errorf("sync %s: %w", dst, err)
	}
	info, err := out.Stat()
	if err != nil {
		_ = out.Close()
		return 0, err
	}
	return info.Size(), out.Close()
}

// prune deletes all but the newest keep finished backups in dir (current, just
// written, is always kept) and any .partial a crashed run left.
func prune(dir string, keep int, current string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var done []string
	var removed []string
	var errs []error
	for _, e := range entries {
		name := e.Name()
		switch {
		case backupName.MatchString(name):
			done = append(done, name)
		case backupName.MatchString(trimPartial(name)):
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, filepath.Join(dir, name))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(done))) // the UTC stamp sorts by time
	for i, name := range done {
		if i < keep || name == current {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, filepath.Join(dir, name))
	}
	if len(errs) > 0 {
		return removed, errors.Join(errs...)
	}
	return removed, syncDir(dir)
}

func trimPartial(name string) string {
	if len(name) > len(partial) && name[len(name)-len(partial):] == partial {
		return name[:len(name)-len(partial)]
	}
	return ""
}

// syncDir makes a rename or delete in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- backup.target_dir
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// Restore decrypts the backup src with the age identities in identityFile
// into dst (which must not exist), and checks it opens as a groupwarden.db
// that carries the restore pause.
func Restore(ctx context.Context, src, identityFile, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s exists: move it (and any -wal and -shm file beside it) aside first", dst)
	}
	kf, err := os.Open(identityFile) // #nosec G304 -- the operator names the key file
	if err != nil {
		return fmt.Errorf("identity file: %w", err)
	}
	ids, err := age.ParseIdentities(kf)
	_ = kf.Close()
	if err != nil {
		return fmt.Errorf("identity file: %w", err)
	}
	in, err := os.Open(src) // #nosec G304 -- the operator names the backup
	if err != nil {
		return err
	}
	defer in.Close()
	r, err := age.Decrypt(in, ids...)
	if err != nil {
		return fmt.Errorf("decrypt %s: %w", src, err)
	}
	tmp := dst + partial
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- beside the configured groupwarden.db
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = check(ctx, tmp)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restore %s: %w", src, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// check opens path as a groupwarden.db (which applies any newer migrations)
// and confirms it is a backup: every action paused by the restore pause.
func check(ctx context.Context, path string) error {
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	pauses, err := st.Pauses(ctx)
	if err != nil {
		return err
	}
	for _, p := range pauses {
		if p.Source == store.SourceRestore && p.Scope == store.ScopeAll {
			return nil
		}
	}
	return errors.New("the file carries no restore pause, so it is not a groupwarden backup")
}
