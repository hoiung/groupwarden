package action_test

import (
	"bytes"
	"context"
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
			pending, err := k.Store.PendingMedia(k.Ctx, 10)
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
