package action_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestAttachmentOfAForgottenCopyNotKept: `member forget` (or the evidence
// purge) can delete an evidence row while its attachment downloads. The row
// is not brought back and the file is not left in the evidence directory,
// where no row would name it for retention or forget to delete.
func TestAttachmentOfAForgottenCopyNotKept(t *testing.T) {
	for name, fail := range map[string]bool{"saved": false, "download failed": true} {
		t.Run(name, func(t *testing.T) {
			k := modtest.New(t, "")
			var logs bytes.Buffer
			k.Media.Log = slog.New(slog.NewTextHandler(&logs, nil))
			k.Fake.Download = func(ctx context.Context, _ *client.Message) ([]byte, string, string, error) {
				if f, err := k.Store.ForgetMember(ctx, modtest.SpammerM.IDs()); err != nil || f.Evidence == 0 {
					t.Errorf("forget during the download: %+v, %v", f, err)
				}
				if fail {
					return nil, "", "", errors.New("media host unreachable")
				}
				return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
			}
			m := k.Spam("M1", modtest.G1)
			m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 8, Raw: []byte("raw")}
			k.Deliver(m)
			pending, err := k.Store.PendingMedia(k.Ctx, 0, 10)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending attachments %d (%v), want 1", len(pending), err)
			}
			if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
				t.Fatalf("fetched %d (%v)", n, err)
			}
			if _, ok, err := k.Store.Evidence(k.Ctx, pending[0].ID); ok || err != nil {
				t.Fatalf("the forgotten evidence row is back (%v)", err)
			}
			files, _ := os.ReadDir(k.Media.Dir)
			if len(files) != 0 {
				t.Fatalf("files left in the evidence directory: %v", files)
			}
			if !strings.Contains(logs.String(), `msg="the evidence copy was deleted during its download; nothing kept" evidence=`+
				strconv.FormatInt(pending[0].ID, 10)+" file="+strconv.FormatBool(!fail)) {
				t.Fatalf("not logged:\n%s", logs.String())
			}
		})
	}
}

// TestAttachmentWaitsForTheConnection: a download that is not the
// attachment's failure — it never reached WhatsApp (no connection, as at a
// restart before Connected), or the bot's stop cut it short — leaves the
// attachment pending, so it is fetched later instead of failing for good.
func TestAttachmentWaitsForTheConnection(t *testing.T) {
	for _, c := range []struct {
		name    string
		fail    func(cancel context.CancelFunc, ctx context.Context) error
		wantErr error  // what the interrupted Fetch returns
		logged  string // what it logs ("" for nothing)
	}{
		{"not connected", func(context.CancelFunc, context.Context) error {
			return fmt.Errorf("refresh media connections: %w", client.ErrNotConnected)
		}, nil, "attachment downloads wait for the WhatsApp connection"},
		{"stopped mid-download", func(cancel context.CancelFunc, ctx context.Context) error {
			cancel()
			return fmt.Errorf("download: %w", ctx.Err())
		}, context.Canceled, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := modtest.New(t, "")
			ctx, cancel := context.WithCancel(k.Ctx)
			defer cancel()
			interrupted := true
			k.Media.Connected = func() bool { return !interrupted }
			k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
				if interrupted {
					return nil, "", "", c.fail(cancel, ctx)
				}
				return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
			}
			m := k.Spam("M1", modtest.G1)
			m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 8, Raw: []byte("raw")}
			k.Deliver(m)
			if n, err := k.Media.Fetch(ctx); !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) || n != 0 {
				t.Fatalf("interrupted: fetched %d (%v), want 0 (%v)", n, err, c.wantErr)
			}
			pending, err := k.Store.PendingMedia(k.Ctx, 0, 10)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending attachments %d (%v), want it still pending", len(pending), err)
			}
			if c.logged != "" && k.Logged(c.logged) != 1 {
				t.Fatal("the wait is not logged")
			}
			if k.Logged("attachment download failed") != 0 {
				t.Fatal("an interrupted download logged as a failed one")
			}
			interrupted = false
			if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
				t.Fatalf("later: fetched %d (%v)", n, err)
			}
			if e, _, _ := k.Store.Evidence(k.Ctx, pending[0].ID); e.MediaState != store.MediaSaved {
				t.Fatalf("media %+v, want saved", e)
			}
		})
	}
}

// TestAttachmentWithoutAnAnswerDoesNotHoldTheQueue: while WhatsApp is
// connected, an attachment whose download gets no answer (a file that never
// finishes in time, a media path no server takes) does not hold up the ones
// queued after it, and after mediaNoAnswerTries passes it is settled as not
// available instead of being downloaded again until the evidence purge.
func TestAttachmentWithoutAnAnswerDoesNotHoldTheQueue(t *testing.T) {
	k := modtest.New(t, "")
	k.Media.Connected = func() bool { return true }
	k.Fake.Download = func(_ context.Context, m *client.Message) ([]byte, string, string, error) {
		if m.ID == "M1" {
			return nil, "", "", fmt.Errorf("download: %w: context deadline exceeded", client.ErrNotConnected)
		}
		return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
	}
	for _, id := range []string{"M1", "M2"} {
		m := k.Spam(id, modtest.G1)
		m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 8, Raw: []byte("raw")}
		k.Deliver(m)
	}
	pending, err := k.Store.PendingMedia(k.Ctx, 0, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending attachments %d (%v), want 2", len(pending), err)
	}
	stuck, next := pending[0].ID, pending[1].ID
	if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
		t.Fatalf("first pass: settled %d (%v), want the second attachment", n, err)
	}
	if e, _, _ := k.Store.Evidence(k.Ctx, next); e.MediaState != store.MediaSaved {
		t.Fatalf("the attachment after the stuck one: %+v, want saved", e)
	}
	for pass := 2; pass < 5; pass++ {
		if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 0 {
			t.Fatalf("pass %d: settled %d (%v), want the stuck one still pending", pass, n, err)
		}
	}
	if e, _, _ := k.Store.Evidence(k.Ctx, stuck); e.MediaState != store.MediaPending {
		t.Fatalf("after 4 passes: %+v, want still pending", e)
	}
	if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
		t.Fatalf("fifth pass: settled %d (%v), want the stuck one", n, err)
	}
	e, _, _ := k.Store.Evidence(k.Ctx, stuck)
	if e.MediaState != store.MediaFailed || !strings.Contains(e.MediaError, "no answer in 5 tries") {
		t.Fatalf("after 5 passes: %+v, want failed saying no answer in 5 tries", e)
	}
	if n := k.Fake.Count("DownloadMedia M1"); n != 5 {
		t.Fatalf("the stuck attachment was asked for %d times, want 5", n)
	}
}

// TestAttachmentSaveFailureLoggedAndLeavesNoFile: an attachment that
// downloaded but cannot be written — the evidence directory cannot be made,
// the disk is full, or the file cannot be put in place — is logged, settled as failed with the
// reason, and leaves no file behind (retention deletes only the files a row
// records). A save that works leaves only the final file.
func TestAttachmentSaveFailureLoggedAndLeavesNoFile(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, final string){
		"no evidence directory": func(t *testing.T, dir, _ string) {
			if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"file not put in place": func(t *testing.T, _, final string) {
			if err := os.MkdirAll(filepath.Join(final, "occupied"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		// The write itself fails (no space left): the file being written is
		// /dev/full. A save that wrote straight to the final name would leave
		// that name holding a partial file and record it as saved.
		"disk full": func(t *testing.T, dir, final string) {
			if _, err := os.Stat("/dev/full"); err != nil {
				t.Skip("no /dev/full on this system")
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/dev/full", final+".part"); err != nil {
				t.Fatal(err)
			}
		},
		"saved": func(*testing.T, string, string) {},
	}
	for name, setUp := range cases {
		t.Run(name, func(t *testing.T) {
			k := modtest.New(t, "")
			var logs bytes.Buffer
			k.Media.Log = slog.New(slog.NewTextHandler(&logs, nil))
			k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
				return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
			}
			m := k.Spam("M1", modtest.G1)
			m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 8, Raw: []byte("raw")}
			k.Deliver(m)
			pending, err := k.Store.PendingMedia(k.Ctx, 0, 10)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending attachments %d (%v), want 1", len(pending), err)
			}
			id := pending[0].ID
			final := filepath.Join(k.Media.Dir, strconv.FormatInt(id, 10)+".jpg")
			setUp(t, k.Media.Dir, final)
			if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
				t.Fatalf("fetched %d (%v)", n, err)
			}
			e, _, _ := k.Store.Evidence(k.Ctx, id)
			if _, err := os.Lstat(final + ".part"); err == nil {
				t.Fatal("a partial file is left behind")
			}
			if name == "saved" {
				if b, err := os.ReadFile(final); e.MediaState != store.MediaSaved || e.MediaPath != final || err != nil || string(b) != "JPEGDATA" {
					t.Fatalf("media %+v, file %q (%v)", e, b, err)
				}
				return
			}
			if e.MediaState != store.MediaFailed || e.MediaPath != "" || !strings.HasPrefix(e.MediaError, "could not save it: ") {
				t.Fatalf("media %+v, want failed with the reason", e)
			}
			if !strings.Contains(logs.String(), `msg="could not save an attachment" evidence=`+strconv.FormatInt(id, 10)) {
				t.Fatalf("the failure is not logged:\n%s", logs.String())
			}
			if fi, err := os.Stat(final); err == nil && !fi.IsDir() {
				t.Fatal("a file was left under the final name")
			}
		})
	}
}
