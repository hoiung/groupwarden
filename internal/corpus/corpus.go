// Package corpus holds labelled spam and legit samples and tests rules
// against them.
//
// Layout: <dir>/spam/**.yaml and <dir>/legit/<class>/*.yaml, one sample per
// file. A sample records what was pasted, never a real person's details in
// the public corpus.
package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

// Sample types: which WhatsApp message kind the pasted text came from.
const (
	TypeText         = "text"
	TypeImageCaption = "image-caption"
	TypePoll         = "poll"
	TypeContact      = "contact"
	TypeInvite       = "invite"
	TypeEvent        = "event"
)

// Labels.
const (
	Spam  = "spam"
	Legit = "legit"
)

// Sample is one labelled message.
type Sample struct {
	Type     string `yaml:"type"`
	PushName string `yaml:"push_name,omitempty"`
	Note     string `yaml:"note,omitempty"`
	// Mentions lists the user IDs @-mentioned in Text; they are masked the
	// way the WhatsApp adapter masks them, so a mention never reads as a
	// phone number.
	Mentions []string `yaml:"mentions,omitempty"`
	// Quoted is the message this one replies to. It is never scanned: a
	// reply is judged on its own words only.
	Quoted string `yaml:"quoted,omitempty"`
	Text   string `yaml:"text"`

	Label string `yaml:"-"` // spam or legit (from the directory)
	Class string `yaml:"-"` // legit/<class> directory name
	Path  string `yaml:"-"`
}

// Fields turns the pasted text into the fields the adapter would extract
// from that message type.
func (s Sample) Fields() []client.Field {
	var mentions []client.JID
	for _, m := range s.Mentions {
		mentions = append(mentions, client.JID(m+"@s.whatsapp.net"))
	}
	var fs []client.Field
	add := func(name, text string) {
		if strings.TrimSpace(text) != "" {
			fs = append(fs, client.Field{Name: name, Text: text, Match: client.MaskMentions(text, mentions)})
		}
	}
	lines := nonEmptyLines(s.Text)
	switch s.Type {
	case TypeImageCaption:
		add("caption", s.Text)
	case TypePoll:
		if len(lines) > 0 {
			add("poll.question", lines[0])
			for _, o := range lines[1:] {
				add("poll.option", o)
			}
		}
	case TypeContact:
		if len(lines) > 0 {
			add("contact.name", lines[0])
			for _, n := range lines[1:] {
				add("contact.number", n)
			}
		}
	case TypeInvite:
		add("invite.caption", s.Text)
		add("invite.link", "https://chat.whatsapp.com/INVITECODE")
	case TypeEvent:
		if len(lines) > 0 {
			add("event.name", lines[0])
		}
		for _, l := range lines[1:] {
			if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
				add("event.join_link", l)
			} else {
				add("event.description", l)
			}
		}
	default:
		add("body", s.Text)
	}
	return fs
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var types = map[string]bool{TypeText: true, TypeImageCaption: true, TypePoll: true, TypeContact: true, TypeInvite: true, TypeEvent: true}

// ReadSample reads one sample file.
func ReadSample(path string) (Sample, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- files under the operator's corpus dir
	if err != nil {
		return Sample{}, err
	}
	var s Sample
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Sample{}, fmt.Errorf("%s: %w", path, err)
	}
	if s.Type == "" {
		s.Type = TypeText
	}
	if !types[s.Type] {
		return Sample{}, fmt.Errorf("%s: unknown type %q", path, s.Type)
	}
	if strings.TrimSpace(s.Text) == "" {
		return Sample{}, fmt.Errorf("%s: text is empty", path)
	}
	s.Path = path
	return s, nil
}

// Load reads every sample under dir/spam and dir/legit.
func Load(dir string) ([]Sample, error) {
	var out []Sample
	for _, label := range []string{Spam, Legit} {
		root := filepath.Join(dir, label)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && path == root {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return nil
			}
			s, err := ReadSample(path)
			if err != nil {
				return err
			}
			s.Label = label
			if rel, err := filepath.Rel(root, filepath.Dir(path)); err == nil && rel != "." {
				s.Class = strings.Split(filepath.ToSlash(rel), "/")[0]
			} else {
				s.Class = "general"
			}
			out = append(out, s)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ClassResult is one legit class's outcome.
type ClassResult struct {
	Class   string
	Samples int
	Hits    int
}

// Result is a corpus test: every rule evaluated as if confirmed and enforced.
type Result struct {
	Spam, Legit, LegitHits, SpamMissed int
	Classes                            []ClassResult
	// RuleHits counts matches per rule across every sample.
	RuleHits map[string]int
	// Missed are spam samples no acting rule caught; FalseHits are legit
	// samples an acting rule would have deleted.
	Missed, FalseHits []Sample
}

// Run tests rs against every sample in dir.
func Run(rs *rules.Ruleset, dir string) (*Result, error) {
	samples, err := Load(dir)
	if err != nil {
		return nil, err
	}
	r := &Result{RuleHits: map[string]int{}}
	classes := map[string]*ClassResult{}
	for _, s := range samples {
		acts, matched := rs.ActsOn(rules.Input{Fields: s.Fields(), PushName: s.PushName})
		for _, m := range matched {
			r.RuleHits[m]++
		}
		if s.Label == Spam {
			r.Spam++
			if !acts {
				r.SpamMissed++
				r.Missed = append(r.Missed, s)
			}
			continue
		}
		r.Legit++
		c := classes[s.Class]
		if c == nil {
			c = &ClassResult{Class: s.Class}
			classes[s.Class] = c
		}
		c.Samples++
		if acts {
			c.Hits++
			r.LegitHits++
			r.FalseHits = append(r.FalseHits, s)
		}
	}
	for _, c := range classes {
		r.Classes = append(r.Classes, *c)
	}
	sort.Slice(r.Classes, func(i, j int) bool { return r.Classes[i].Class < r.Classes[j].Class })
	return r, nil
}

// Err is nil only when there are both spam and legit samples, no legit
// sample would be deleted and no spam sample is missed ("zero legit hits" and
// "zero spam missed" are never measured over an empty set).
func (r *Result) Err() error {
	var problems []string
	if r.Spam == 0 {
		problems = append(problems, "the corpus has no spam samples")
	}
	if r.Legit == 0 {
		problems = append(problems, "the corpus has no legit samples")
	}
	if r.LegitHits > 0 {
		problems = append(problems, fmt.Sprintf("%d legit sample(s) would be deleted", r.LegitHits))
	}
	if r.SpamMissed > 0 {
		problems = append(problems, fmt.Sprintf("%d spam sample(s) are not caught", r.SpamMissed))
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New("corpus: " + strings.Join(problems, "; "))
}
