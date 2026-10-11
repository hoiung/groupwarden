package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// Connected reports whether WhatsApp is connected (nil: always). A
	// download that gets no answer while it is not ends the pass; while it
	// is, only that attachment's media servers failed.
	Connected func() bool
	Log       *slog.Logger
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	// initOnce: Wake is called from the inbox worker while Run starts.
	initOnce sync.Once
	wake     chan struct{}
	// noAnswer records, per evidence row, the downloads that got no answer
	// while WhatsApp was connected and when the next may go. Fetch runs on
	// one goroutine. In memory: a restart gives each attachment its tries
	// again.
	noAnswer map[int64]noAnswerState
}

// noAnswerState is one attachment's downloads without an answer.
type noAnswerState struct {
	tries int
	next  time.Time
}

// An attachment whose download gets no answer from WhatsApp's media servers
// while WhatsApp itself is connected (a file that never finishes within the
// download timeout, a media path no server takes) is tried again after
// mediaNoAnswerWait, doubling each time (2, 4, 8 and 16 minutes), and after
// mediaNoAnswerTries is recorded as not available instead of being
// downloaded again until the evidence purge. Spacing the tries in time, not
// passes (every new attachment wakes a pass), means only an outage of about
// half an hour gives an attachment up.
const (
	mediaNoAnswerTries = 5
	mediaNoAnswerWait  = 2 * time.Minute
)

func (f *MediaFetcher) now() time.Time {
	if f.Now == nil {
		return time.Now()
	}
	return f.Now()
}

func (f *MediaFetcher) init() {
	f.initOnce.Do(func() { f.wake = make(chan struct{}, 1) })
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

// Fetch downloads every pending attachment once, in order, and returns how
// many it settled. An attachment that gets no answer never holds up the ones
// after it: while WhatsApp is connected it stays pending and waits before its
// next try (and is settled as failed after mediaNoAnswerTries); while WhatsApp
// is not connected the pass ends, and the supervisor wakes it at Connected.
func (f *MediaFetcher) Fetch(ctx context.Context) (int, error) {
	if f.noAnswer == nil {
		f.noAnswer = map[int64]noAnswerState{}
	}
	n := 0
	var after int64
	seen := map[int64]bool{}
	for {
		pending, err := f.Store.PendingMedia(ctx, after, 10)
		if err != nil {
			return n, err
		}
		if len(pending) == 0 {
			// The rows no longer pending (settled, purged, forgotten) drop
			// their counts.
			for id := range f.noAnswer {
				if !seen[id] {
					delete(f.noAnswer, id)
				}
			}
			return n, nil
		}
		for _, e := range pending {
			after = e.ID
			seen[e.ID] = true
			if s, ok := f.noAnswer[e.ID]; ok && f.now().Before(s.next) {
				continue
			}
			err := f.fetchOne(ctx, e)
			if errors.Is(err, client.ErrNotConnected) {
				if f.Connected != nil && !f.Connected() {
					f.Log.Info("attachment downloads wait for the WhatsApp connection", "evidence", e.ID)
					return n, nil
				}
				var settled bool
				if settled, err = f.unanswered(ctx, e, err); err == nil && !settled {
					continue
				}
			}
			if err != nil {
				return n, err
			}
			delete(f.noAnswer, e.ID)
			n++
		}
	}
}

// unanswered handles a download of e that got no answer while WhatsApp is
// connected: it stays pending, waiting longer after each such answer, until
// its mediaNoAnswerTries-th, which settles it as failed. It reports whether e
// was settled.
func (f *MediaFetcher) unanswered(ctx context.Context, e store.Evidence, cause error) (bool, error) {
	s := f.noAnswer[e.ID]
	s.tries++
	if s.tries < mediaNoAnswerTries {
		wait := mediaNoAnswerWait << (s.tries - 1)
		s.next = f.now().Add(wait)
		f.noAnswer[e.ID] = s
		f.Log.Warn("attachment download got no answer; the others go first, it is tried again later",
			"evidence", e.ID, "tries", s.tries, "wait", wait.String(), "err", mask.IDs(cause.Error()))
		return false, nil
	}
	why := fmt.Sprintf("WhatsApp's media servers gave no answer in %d tries: %s", s.tries, mask.IDs(cause.Error()))
	f.Log.Warn("attachment download failed", "evidence", e.ID, "err", why)
	return true, f.settle(ctx, e.ID, store.MediaFailed, "", why, e.MediaSize)
}

// fetchOne downloads one attachment. The size the post declared is the
// sender's claim, so the download itself is limited to
// evidence.max_attachment_mb; a file over it is settled as too large, never
// left pending to be fetched again.
func (f *MediaFetcher) fetchOne(ctx context.Context, e store.Evidence) error {
	msg := &client.Message{Chat: client.JID(e.Chat), Sender: client.JID(e.Sender), ID: e.MsgID,
		Media: &client.Media{Kind: e.MediaKind, MimeType: e.MediaMime, FileName: e.MediaName, Size: e.MediaSize, Raw: e.MediaRaw}}
	limitMB := f.Config.Current().Config.Evidence.MaxAttachmentMB
	data, mimeType, name, err := f.Adapter.DownloadMedia(ctx, msg, int64(limitMB)<<20)
	if errors.Is(err, client.ErrMediaTooLarge) {
		f.Log.Info("attachment over the size limit; not kept", "evidence", e.ID, "declared_bytes", e.MediaSize, "limit_mb", limitMB)
		return f.settle(ctx, e.ID, store.MediaTooLarge, "", "", 0)
	}
	// A download that got no answer (no connection, the media servers not
	// reached in time) is not yet the attachment's failure: Fetch decides.
	// One the bot's stop cut short stays pending.
	if errors.Is(err, client.ErrNotConnected) {
		return err
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		f.Log.Warn("attachment download failed", "evidence", e.ID, "err", mask.IDs(err.Error()))
		return f.settle(ctx, e.ID, store.MediaFailed, "", mask.IDs(err.Error()), e.MediaSize)
	}
	size := uint64(len(data))
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return f.saveFailed(ctx, e, size, err)
	}
	path := filepath.Join(f.Dir, fmt.Sprintf("%d%s", e.ID, extension(mimeType, name, e.MediaName)))
	if err := writeFile(path, data); err != nil {
		return f.saveFailed(ctx, e, size, err)
	}
	return f.settle(ctx, e.ID, store.MediaSaved, path, "", size)
}

// saveFailed records an attachment that downloaded but could not be written
// to the evidence directory (no space, no permission): it is logged, and the
// row is settled as failed, which the admin chat shows (in the report, or in
// a reply under it when the report went out first).
func (f *MediaFetcher) saveFailed(ctx context.Context, e store.Evidence, size uint64, err error) error {
	f.Log.Error("could not save an attachment", "evidence", e.ID, "err", err)
	return f.settle(ctx, e.ID, store.MediaFailed, "", "could not save it: "+err.Error(), size)
}

// settle records a download's outcome on its evidence row. A row deleted
// during the download (`member forget`, the evidence purge) is not
// recreated, and the file written for it is removed: no row names it, so
// retention and forget would never delete it.
func (f *MediaFetcher) settle(ctx context.Context, id int64, state, path, errText string, size uint64) error {
	ok, err := f.Store.SettleMedia(ctx, id, state, path, errText, size)
	if err != nil || ok {
		return err
	}
	f.Log.Info("the evidence copy was deleted during its download; nothing kept", "evidence", id, "file", path != "")
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) { // #nosec G703 -- the file this fetcher just wrote under data_dir
		f.Log.Error("could not delete the attachment of a deleted evidence copy", "evidence", id, "err", err)
	}
	return nil
}

// writeFile writes data under a temporary name and renames it into place, so
// a short write never leaves a file under the final name. On failure the
// temporary file is removed too: retention and `member forget` delete only the
// files a row records, so a leftover would keep the member's media for good.
func writeFile(path string, data []byte) error {
	part := path + ".part"
	err := os.WriteFile(part, data, 0o600)
	if err == nil {
		err = os.Rename(part, path)
	}
	if err != nil {
		if rmErr := os.Remove(part); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("the partial file is left behind: %w", rmErr))
		}
	}
	return err
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
