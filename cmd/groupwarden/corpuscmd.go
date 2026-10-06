package main

import (
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/corpus"
	publiccorpus "github.com/hoiung/groupwarden/tests/corpus"
)

// corpusTest evaluates every rule as if confirmed and enforced against the
// labelled samples in dir. One `CLASS legit/<class> <n> hits=<h>` line per
// legit class, the rule hits, every missed spam (with the acting rule that
// came closest and the conditions it failed) and every legit false hit (with
// the rules that would delete it), then one TOTAL line. Exit 1 when any legit
// sample would be deleted, any spam is missed, or there are no spam or no
// legit samples.
func (e *env) corpusTest(cfgPath, dir string) int {
	if dir == "" {
		fmt.Fprintln(e.stderr, "usage: groupwarden corpus test --config <file> --corpus <dir>")
		return exitUsage
	}
	l, err := config.Read(cfgPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "config: %v\n", err)
		return exitFail
	}
	r, err := corpus.Run(l.Rules, dir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return exitFail
	}
	for _, c := range r.Classes {
		fmt.Fprintf(e.stdout, "CLASS legit/%s %d hits=%d\n", c.Class, c.Samples, c.Hits)
	}
	names := make([]string, 0, len(r.RuleHits))
	for n := range r.RuleHits {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(e.stdout, "RULE %s hits=%d\n", n, r.RuleHits[n])
	}
	for _, m := range r.Missed {
		if m.NoRule {
			fmt.Fprintf(e.stdout, "MISSED spam %s: no rule that deletes is enabled\n", m.Sample.Path)
			continue
		}
		fmt.Fprintf(e.stdout, "MISSED spam %s: closest rule %s, failed %s\n", m.Sample.Path, m.Closest.Rule,
			strings.Join(m.Closest.Failed, " | "))
	}
	for _, f := range r.FalseHits {
		fmt.Fprintf(e.stdout, "FALSE-HIT legit/%s %s: deleted by %s\n", f.Sample.Class, f.Sample.Path, strings.Join(f.Rules, ", "))
	}
	fmt.Fprintf(e.stdout, "TOTAL spam=%d legit=%d legit_hits=%d spam_missed=%d\n", r.Spam, r.Legit, r.LegitHits, r.SpamMissed)
	if err := r.Err(); err != nil {
		fmt.Fprintln(e.stderr, strings.TrimPrefix(err.Error(), "corpus: "))
		return exitFail
	}
	return exitOK
}

// corpusAdd saves the message on stdin to the corpus in dir (see corpus.Add):
// `ADDED <file>` or `DUPLICATE <file>`, after `SEEDED <n> ...` when it created
// the corpus. Exit 1 when the message is already saved under the other label.
func (e *env) corpusAdd(dir, label string, s corpus.Sample, public bool) int {
	if dir == "" || label == "" {
		fmt.Fprintln(e.stderr, "usage: groupwarden corpus add --label spam|legit --corpus <dir> [--type <type>] "+
			"[--push-name <text>] [--note <text>] [--public] < message.txt")
		return exitUsage
	}
	raw, err := io.ReadAll(e.stdin)
	if err != nil {
		fmt.Fprintf(e.stderr, "read the message from stdin: %v\n", err)
		return exitFail
	}
	s.Text = string(raw)
	seed, err := fs.Sub(publiccorpus.Legit, "legit")
	if err != nil {
		fmt.Fprintf(e.stderr, "public legit samples: %v\n", err)
		return exitFail
	}
	if public {
		seed = nil // the public corpus is the seed itself
	}
	a, err := corpus.Add(dir, label, s, public, seed)
	if err != nil {
		fmt.Fprintf(e.stderr, "corpus add: %v\n", err)
		return exitFail
	}
	if a.Seeded > 0 {
		fmt.Fprintf(e.stdout, "SEEDED %d legit samples from the public set into %s/legit\n", a.Seeded, dir)
	}
	if a.Duplicate != "" {
		fmt.Fprintf(e.stdout, "DUPLICATE %s\n", a.Duplicate)
		return exitOK
	}
	fmt.Fprintf(e.stdout, "ADDED %s\n", a.Path)
	return exitOK
}
