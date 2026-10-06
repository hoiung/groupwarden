package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"github.com/hoiung/groupwarden/internal/corpus"
)

// LastGoodName is the copy of the last config that passed every check,
// kept in data_dir so a restart with a rejected file still runs.
const LastGoodName = "config.last-good.yaml"

// Rejected is a candidate config that failed a check while another keeps
// running.
type Rejected struct {
	Reason  error
	Running string // hash of the config still in use
}

func (r *Rejected) Error() string {
	return fmt.Sprintf("REJECTED: %v, still running v%s", r.Reason, r.Running)
}

func (r *Rejected) Unwrap() error { return r.Reason }

// checkCorpus runs the corpus test when corpus_dir is set: a config that
// misses a spam sample or would delete a legit one is not loaded.
func checkCorpus(l *Loaded) error {
	if l.Config.CorpusDir == "" {
		return nil
	}
	res, err := corpus.Run(l.Rules, l.Config.CorpusDir)
	if err != nil {
		return fmt.Errorf("corpus: %w", err)
	}
	return res.Err()
}

// Holder holds the running config and swaps it whole on reload.
type Holder struct {
	path string
	cur  atomic.Pointer[Loaded]
	mu   sync.Mutex // one reload at a time
}

// Current is the config in use. Read it once per decision: a reload swaps
// the pointer, never the contents.
func (h *Holder) Current() *Loaded { return h.cur.Load() }

// NewHolder holds an already loaded config (tests and one-shot commands).
func NewHolder(path string, l *Loaded) *Holder {
	h := &Holder{path: path}
	h.cur.Store(l)
	return h
}

// Boot loads the config at path. When it fails a check but data_dir holds a
// last good copy, that copy runs and rejected says why the file was refused.
// With no usable last good copy the error is returned and the bot must not
// start.
func Boot(path string) (h *Holder, rejected *Rejected, err error) {
	l, err := Load(path)
	if err == nil {
		if err := saveLastGood(l); err != nil {
			return nil, nil, err
		}
		return NewHolder(path, l), nil, nil
	}
	dataDir := peekDataDir(path)
	if dataDir == "" {
		return nil, nil, err
	}
	raw, rerr := os.ReadFile(filepath.Join(dataDir, LastGoodName)) // #nosec G304 -- operator-configured data dir
	if rerr != nil {
		return nil, nil, err
	}
	lg, lerr := Parse(raw, filepath.Dir(path))
	if lerr != nil {
		return nil, nil, fmt.Errorf("%w (and the last good copy no longer loads: %v)", err, lerr)
	}
	return NewHolder(path, lg), &Rejected{Reason: err, Running: lg.Hash}, nil
}

// Reload builds and checks the file again and swaps it in whole. On any
// failure the running config stays and the error is a *Rejected.
func (h *Holder) Reload() (*Loaded, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, err := Load(h.path)
	if err != nil {
		return nil, &Rejected{Reason: err, Running: h.Current().Hash}
	}
	if err := saveLastGood(l); err != nil {
		return nil, &Rejected{Reason: err, Running: h.Current().Hash}
	}
	h.cur.Store(l)
	return l, nil
}

// saveLastGood writes the file atomically (temp file + rename) into data_dir.
func saveLastGood(l *Loaded) error {
	if err := os.MkdirAll(l.Config.DataDir, 0o700); err != nil {
		return fmt.Errorf("keep last good config: %w", err)
	}
	dst := filepath.Join(l.Config.DataDir, LastGoodName)
	tmp, err := os.CreateTemp(l.Config.DataDir, LastGoodName+".*")
	if err != nil {
		return fmt.Errorf("keep last good config: %w", err)
	}
	_, werr := tmp.Write(l.Raw)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("keep last good config: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("keep last good config: %w", err)
	}
	return nil
}

// peekDataDir reads only data_dir from a file that may fail other checks,
// so the last good copy can be found. "" when even that is unreadable.
func peekDataDir(path string) string {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names the config file
	if err != nil {
		return ""
	}
	var top struct {
		DataDir string `yaml:"data_dir"`
	}
	if yaml.Unmarshal(raw, &top) != nil || top.DataDir == "" {
		return ""
	}
	if !filepath.IsAbs(top.DataDir) {
		return filepath.Join(filepath.Dir(path), top.DataDir)
	}
	return top.DataDir
}
