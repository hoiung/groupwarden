package action

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// evidenceMessage rebuilds the message an evidence copy holds, for a fire-time
// re-decision with the current config.
func evidenceMessage(e store.Evidence) (*client.Message, error) {
	var fields []client.Field
	if err := json.Unmarshal([]byte(e.Fields), &fields); err != nil {
		return nil, fmt.Errorf("the evidence copy cannot be read: %w", err)
	}
	return &client.Message{Chat: client.JID(e.Chat), Sender: client.JID(e.Sender), SenderAlt: client.JID(e.SenderAlt),
		ID: e.MsgID, TargetID: e.TargetID, Time: e.MsgTime, PushName: e.PushName, Fields: fields}, nil
}

// MediaFetcher saves the attachment of each evidence copy to the evidence
// dir. It runs on its own, so a download never delays a delete; a failed
// download is recorded on the copy.
type MediaFetcher struct {
	Store   *store.Store
	Adapter client.Adapter
	Config  *config.Holder
	// Dir is where attachment files go (data_dir/evidence).
	Dir string
	Log *slog.Logger

	wake chan struct{}
}

func (f *MediaFetcher) init() {
	if f.wake == nil {
		f.wake = make(chan struct{}, 1)
	}
}

// Wake asks the fetcher to look for attachments now.
func (f *MediaFetcher) Wake() {
	f.init()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Run downloads pending attachments until ctx ends.
func (f *MediaFetcher) Run(ctx context.Context) {
	f.init()
	for {
		if _, err := f.Fetch(ctx); err != nil && ctx.Err() == nil {
			f.Log.Error("attachment download pass failed", "err", mask.IDs(err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-time.After(time.Minute):
		}
	}
}

// Fetch downloads every pending attachment and returns how many it settled.
func (f *MediaFetcher) Fetch(ctx context.Context) (int, error) {
	n := 0
	for {
		pending, err := f.Store.PendingMedia(ctx, 10)
		if err != nil || len(pending) == 0 {
			return n, err
		}
		for _, e := range pending {
			if err := f.fetchOne(ctx, e); err != nil {
				return n, err
			}
			n++
		}
	}
}

func (f *MediaFetcher) fetchOne(ctx context.Context, e store.Evidence) error {
	msg := &client.Message{Chat: client.JID(e.Chat), Sender: client.JID(e.Sender), ID: e.MsgID,
		Media: &client.Media{Kind: e.MediaKind, MimeType: e.MediaMime, FileName: e.MediaName, Size: e.MediaSize, Raw: e.MediaRaw}}
	data, mimeType, name, err := f.Adapter.DownloadMedia(ctx, msg)
	if err != nil {
		f.Log.Warn("attachment download failed", "evidence", e.ID, "err", mask.IDs(err.Error()))
		return f.Store.SetMedia(ctx, e.ID, store.MediaFailed, "", mask.IDs(err.Error()))
	}
	limit := int64(f.Config.Current().Config.Evidence.MaxAttachmentMB) << 20
	if int64(len(data)) > limit {
		return f.Store.SetMedia(ctx, e.ID, store.MediaTooLarge, "", "")
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return f.Store.SetMedia(ctx, e.ID, store.MediaFailed, "", err.Error())
	}
	path := filepath.Join(f.Dir, fmt.Sprintf("%d%s", e.ID, extension(mimeType, name, e.MediaName)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return f.Store.SetMedia(ctx, e.ID, store.MediaFailed, "", err.Error())
	}
	return f.Store.SetMedia(ctx, e.ID, store.MediaSaved, path, "")
}

// extension picks a file extension from the file name, else the MIME type.
func extension(mimeType string, names ...string) string {
	for _, n := range names {
		if ext := filepath.Ext(n); ext != "" && len(ext) <= 8 && !strings.ContainsAny(ext, `/\`) {
			return strings.ToLower(ext)
		}
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
