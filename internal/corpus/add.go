package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/hoiung/groupwarden/internal/normalise"
)

// Added is what Add did.
type Added struct {
	// Path is the new sample file ("" for a duplicate).
	Path string
	// Duplicate is the sample already holding the same text.
	Duplicate string
	// Seeded counts the legit samples copied into a new corpus.
	Seeded int
	// Sample is the sample as saved (redacted).
	Sample Sample
}

// Add saves one pasted message to the corpus in dir, under spam/ or legit/,
// one file per sample named by its normalised hash. The text and push name
// are redacted first (Redact). A message whose normalised text is already in
// the corpus is not saved again; one already saved under the other label is
// an error. When dir holds no corpus yet, the legit samples in seed (laid
// out <class>/*.yaml; nil = none) are copied in first.
func Add(dir, label string, s Sample, public bool, seed fs.FS) (Added, error) {
	var a Added
	if label != Spam && label != Legit {
		return a, fmt.Errorf("label must be %s or %s, not %q", Spam, Legit, label)
	}
	if s.Type == "" {
		s.Type = TypeText
	}
	if !types[s.Type] {
		return a, fmt.Errorf("unknown type %q", s.Type)
	}
	s.Text = strings.TrimRight(s.Text, " \t\r\n")
	if strings.TrimSpace(s.Text) == "" {
		return a, errors.New("the pasted message is empty")
	}
	s.Text = Redact(s.Text, public)
	s.PushName = Redact(s.PushName, public)
	if !exists(filepath.Join(dir, Spam)) && !exists(filepath.Join(dir, Legit)) && seed != nil {
		n, err := copySeed(seed, filepath.Join(dir, Legit))
		if err != nil {
			return a, fmt.Errorf("seed the legit samples: %w", err)
		}
		a.Seeded = n
	}
	existing, err := Load(dir)
	if err != nil {
		return a, err
	}
	h := sampleHash(s)
	for _, e := range existing {
		if sampleHash(e) != h {
			continue
		}
		if e.Label != label {
			return a, fmt.Errorf("this message is already labelled %s at %s", e.Label, e.Path)
		}
		a.Duplicate, a.Sample = e.Path, e
		return a, nil
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return a, err
	}
	path := filepath.Join(dir, label, h[:12]+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return a, err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return a, err
	}
	saved, err := ReadSample(path)
	if err != nil {
		return a, fmt.Errorf("the saved sample does not read back: %w", err)
	}
	a.Path, a.Sample = path, saved
	return a, nil
}

// sampleHash identifies a sample by its type and its normalised text (case
// folded, look-alike letters mapped to one form, spacing collapsed), so the
// same message pasted twice is one sample.
func sampleHash(s Sample) string {
	t := s.Type
	if t == "" {
		t = TypeText
	}
	// Base folds case first: the skeleton alone maps "I" and "i" apart.
	sum := sha256.Sum256([]byte(t + "\x00" + strings.Join(strings.Fields(normalise.Skeleton(normalise.Base(s.Text))), " ")))
	return hex.EncodeToString(sum[:])
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copySeed copies every .yaml file of seed into dst, keeping its path.
func copySeed(seed fs.FS, dst string) (int, error) {
	n := 0
	err := fs.WalkDir(seed, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		raw, err := fs.ReadFile(seed, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(out, raw, 0o600); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}
