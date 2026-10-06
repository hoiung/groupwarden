package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/corpus"
)

// corpusTest evaluates every rule as if confirmed and enforced against the
// labelled samples in dir. One `CLASS legit/<class> <n> hits=<h>` line per
// legit class, the rule hits, every missed spam and legit false hit, then
// one TOTAL line. Exit 1 when any legit sample would be deleted, any spam is
// missed, or there are no spam or no legit samples.
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
	for _, s := range r.Missed {
		fmt.Fprintf(e.stdout, "MISSED spam %s\n", s.Path)
	}
	for _, s := range r.FalseHits {
		fmt.Fprintf(e.stdout, "FALSE-HIT legit/%s %s\n", s.Class, s.Path)
	}
	fmt.Fprintf(e.stdout, "TOTAL spam=%d legit=%d legit_hits=%d spam_missed=%d\n", r.Spam, r.Legit, r.LegitHits, r.SpamMissed)
	if err := r.Err(); err != nil {
		fmt.Fprintln(e.stderr, strings.TrimPrefix(err.Error(), "corpus: "))
		return exitFail
	}
	return exitOK
}
