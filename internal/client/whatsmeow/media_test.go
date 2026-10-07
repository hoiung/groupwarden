package whatsmeow

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	wm "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"

	"github.com/hoiung/groupwarden/internal/client"
	gwstore "github.com/hoiung/groupwarden/internal/store"
)

// newConfiguredClient is a library client on a fresh session store, set up
// as Open sets it up.
func newConfiguredClient(t *testing.T) *wm.Client {
	t.Helper()
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite", gwstore.DSN(filepath.Join(t.TempDir(), "whatsmeow.db")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cli := wm.NewClient(device, nil)
	configure(cli)
	return cli
}

// encryptMedia encrypts plain the way WhatsApp stores an attachment: AES-CBC
// with PKCS#7 padding, then a 10-byte HMAC, keyed from a random media key.
func encryptMedia(t *testing.T, plain []byte, kind wm.MediaType) (body, mediaKey, encSHA, plainSHA []byte) {
	t.Helper()
	mediaKey = make([]byte, 32)
	if _, err := rand.Read(mediaKey); err != nil {
		t.Fatal(err)
	}
	keys := hkdfutil.SHA256(mediaKey, nil, []byte(kind), 112)
	iv, cipherKey, macKey := keys[:16], keys[16:48], keys[48:80]
	ciphertext, err := cbcutil.Encrypt(cipherKey, iv, bytes.Clone(plain))
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(ciphertext)
	body = append(ciphertext, h.Sum(nil)[:10]...)
	enc, sum := sha256.Sum256(body), sha256.Sum256(plain)
	return body, mediaKey, enc[:], sum[:]
}

type mediaServer struct {
	*httptest.Server
	requests atomic.Int32
}

// serveMedia answers every request with body, declaring its length or (by
// flushing as it goes) not.
func serveMedia(t *testing.T, body []byte, declareLength bool) *mediaServer {
	t.Helper()
	s := &mediaServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.requests.Add(1)
		if declareLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		for rest := body; len(rest) > 0; {
			n := min(len(rest), 32<<10)
			if _, err := w.Write(rest[:n]); err != nil {
				return
			}
			rest = rest[n:]
			if !declareLength {
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// TestMediaDownloadCapped: the library's own download path, on the client
// Open sets up, stops at the cap a download carries (DownloadMedia sets it).
// The size a message declares is the sender's claim, so the cap is what keeps
// a file out of memory. A body whose declared length is over the cap is
// refused before it is read; one with no declared length stops one byte past
// it; neither is fetched again (the library retries network errors up to five
// times). A file exactly at the cap, padding and MAC included, downloads
// whole, and a request with no cap (the library's app-state blobs) is not
// limited.
func TestMediaDownloadCapped(t *testing.T) {
	cli := newConfiguredClient(t)
	internals := cli.DangerousInternals()
	const limit = 64 << 10
	for _, declareLength := range []bool{true, false} {
		t.Run("declared length "+strconv.FormatBool(declareLength), func(t *testing.T) {
			ctx := withMaxBytes(context.Background(), limit)
			// A multiple of 16, so the padding is a whole block: the most an
			// encrypted body can add to the file.
			atCap := bytes.Repeat([]byte("a"), limit)
			body, key, encSHA, plainSHA := encryptMedia(t, atCap, wm.MediaDocument)
			srv := serveMedia(t, body, declareLength)
			data, err := internals.DownloadAndDecrypt(ctx, srv.URL+"/m", key, wm.MediaDocument, encSHA, plainSHA)
			if err != nil || !bytes.Equal(data, atCap) {
				t.Fatalf("a file at the cap: %d bytes, err=%v", len(data), err)
			}

			over := bytes.Repeat([]byte("b"), 4*limit)
			body, key, encSHA, plainSHA = encryptMedia(t, over, wm.MediaDocument)
			srv = serveMedia(t, body, declareLength)
			data, err = internals.DownloadAndDecrypt(ctx, srv.URL+"/m", key, wm.MediaDocument, encSHA, plainSHA)
			if !errors.Is(err, client.ErrMediaTooLarge) || data != nil {
				t.Fatalf("a file over the cap: %d bytes, err=%v, want client.ErrMediaTooLarge", len(data), err)
			}
			if n := srv.requests.Load(); n != 1 {
				t.Fatalf("the file over the cap was requested %d times, want once", n)
			}

			srv = serveMedia(t, over, declareLength)
			if data, err := internals.DownloadMedia(context.Background(), srv.URL+"/m"); err != nil || len(data) != len(over) {
				t.Fatalf("a download with no cap: %d bytes, err=%v", len(data), err)
			}
		})
	}
}

// endless is a body that never ends; it counts what was read from it.
type endless struct{ read int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.read += len(p)
	return len(p), nil
}

type answer struct{ resp *http.Response }

func (a answer) RoundTrip(*http.Request) (*http.Response, error) { return a.resp, nil }

// TestCappedBodyReadsNoFurther: however much the reader asks for, a capped
// body takes at most one byte past the cap from the connection when no length
// is declared, and nothing at all when the declared length is over the cap.
func TestCappedBodyReadsNoFurther(t *testing.T) {
	const limit = 1000
	cases := []struct {
		declared int64
		most     int
	}{
		{-1, limit + mediaOverhead + 1},
		{limit + mediaOverhead + 1, 0},
	}
	for _, c := range cases {
		src := &endless{}
		tr := mediaTransport{next: answer{&http.Response{StatusCode: http.StatusOK, ContentLength: c.declared, Body: io.NopCloser(src)}}}
		req, err := http.NewRequestWithContext(withMaxBytes(context.Background(), limit), http.MethodGet, "http://media.invalid/m", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); !errors.Is(err, client.ErrMediaTooLarge) {
			t.Fatalf("declared %d: err=%v, want client.ErrMediaTooLarge", c.declared, err)
		}
		if src.read > c.most {
			t.Fatalf("declared %d: read %d bytes from the connection, want at most %d", c.declared, src.read, c.most)
		}
	}
}
