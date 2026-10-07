package whatsmeow

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
)

// downloadTimeout bounds one attachment download, the library's retries
// included, so a stalled connection cannot hold up the attachments queued
// behind it. The largest file kept (50 MiB, the schema's maximum for
// evidence.max_attachment_mb) arrives within it at 1.5 Mbit/s.
const downloadTimeout = 5 * time.Minute

// mediaOverhead is how far an encrypted attachment's body can run past the
// file: AES-CBC padding (1 to 16 bytes) and the 10-byte MAC.
const mediaOverhead = 16 + 10

type maxBytesKey struct{}

// withMaxBytes marks the downloads made with ctx as allowed at most n bytes
// of file.
func withMaxBytes(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, maxBytesKey{}, n)
}

func maxBytesOf(ctx context.Context) (int64, bool) {
	n, ok := ctx.Value(maxBytesKey{}).(int64)
	return n, ok
}

// mediaClient is the HTTP client the library downloads media with. The size
// an attachment declares is the sender's claim, and the library reads a
// download whole into memory, so the limit is enforced on the response:
// mediaTransport refuses a body declared longer than the cap and stops
// reading one that runs past it. App-state blobs, the library's other media
// downloads, carry no cap: they are the bot account's own data.
func mediaClient() *http.Client {
	return &http.Client{Transport: mediaTransport{next: http.DefaultTransport.(*http.Transport).Clone()}}
}

type mediaTransport struct{ next http.RoundTripper }

func (t mediaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	limit, capped := maxBytesOf(req.Context())
	if err != nil || !capped {
		return resp, err
	}
	body := &cappedBody{ReadCloser: resp.Body, limit: limit, left: limit + mediaOverhead}
	if resp.ContentLength > body.left {
		body.left = -1 // refused before any of it is read
	}
	resp.Body = body
	return resp, nil
}

// cappedBody fails once more than left bytes would be read (at once when
// left is below zero). Its error is not a network error, so the library does
// not fetch the file again.
type cappedBody struct {
	io.ReadCloser
	limit, left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	if b.left -= int64(n); b.left < 0 {
		return 0, tooLarge(b.limit)
	}
	return n, err
}

func tooLarge(limit int64) error {
	return fmt.Errorf("%w of %d bytes", client.ErrMediaTooLarge, limit)
}
