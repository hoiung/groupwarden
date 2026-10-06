package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Problem is one thing wrong with a config file, at a line and column.
type Problem struct {
	Line, Column int
	Msg          string
}

func (p Problem) String() string {
	switch {
	case p.Line > 0 && p.Column > 0:
		return fmt.Sprintf("line %d: %s (column %d)", p.Line, p.Msg, p.Column)
	case p.Line > 0:
		return fmt.Sprintf("line %d: %s", p.Line, p.Msg)
	}
	return p.Msg
}

// Problems is every problem found in one file, in line order.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

func (ps Problems) sorted() Problems {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Line != ps[j].Line {
			return ps[i].Line < ps[j].Line
		}
		return ps[i].Column < ps[j].Column
	})
	return ps
}

// position is where a node sits in the file and how it was written.
type position struct {
	line, col int
	quoted    bool
	raw       string
}

// tree is a parsed YAML file as plain values (map[string]any, []any,
// string, int64, float64, bool, nil) plus each node's position, keyed by
// path (path elements joined with "\x00").
type tree struct {
	root any
	at   map[string]position // a value's own node
	keys map[string]position // the key node of a mapping member
}

func pathKey(path []string) string { return strings.Join(path, "\x00") }

// YAML 1.2 core schema resolution of plain scalars (yaml.org/spec/1.2.2
// §10.3.2): YAML 1.1 forms such as NO, yes, on stay strings.
var (
	nullRe  = regexp.MustCompile(`^(~|null|Null|NULL|)$`)
	boolRe  = regexp.MustCompile(`^(true|True|TRUE|false|False|FALSE)$`)
	intRe   = regexp.MustCompile(`^[-+]?[0-9]+$`)
	octRe   = regexp.MustCompile(`^0o[0-7]+$`)
	hexRe   = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	floatRe = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
	infRe   = regexp.MustCompile(`^[-+]?(\.inf|\.Inf|\.INF)$`)
	nanRe   = regexp.MustCompile(`^(\.nan|\.NaN|\.NAN)$`)
	yamlErr = regexp.MustCompile(`^yaml: line ([0-9]+): (.*)$`)
)

// parseYAML reads one YAML document into a tree.
func parseYAML(raw []byte) (*tree, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, Problems{{Msg: "the config file is empty"}}
		}
		return nil, yamlProblem(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, Problems{{Line: extra.Line, Msg: "only one YAML document is allowed (remove the extra ---)"}}
	} else if !errors.Is(err, io.EOF) {
		return nil, yamlProblem(err)
	}
	t := &tree{at: map[string]position{}, keys: map[string]position{}}
	var probs Problems
	t.root = t.convert(&doc, nil, &probs)
	if len(probs) > 0 {
		return nil, probs.sorted()
	}
	return t, nil
}

func yamlProblem(err error) Problems {
	msg := err.Error()
	if m := yamlErr.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1])
		return Problems{{Line: line, Msg: m[2]}}
	}
	return Problems{{Msg: strings.TrimPrefix(msg, "yaml: ")}}
}

func (t *tree) convert(n *yaml.Node, path []string, probs *Problems) any {
	here := position{line: n.Line, col: n.Column, raw: n.Value}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		*probs = append(*probs, Problem{n.Line, n.Column, "anchors (&) and aliases (*) are not supported; write the value out"})
		return nil
	}
	if n.Style&yaml.TaggedStyle != 0 {
		*probs = append(*probs, Problem{n.Line, n.Column, fmt.Sprintf("tags like %s are not supported; use quotes for text", n.Tag)})
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return t.convert(n.Content[0], path, probs)
	case yaml.MappingNode:
		t.at[pathKey(path)] = here
		m := make(map[string]any, len(n.Content)/2)
		first := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				*probs = append(*probs, Problem{k.Line, k.Column, "a key must be plain text"})
				continue
			}
			if k.Value == "<<" && k.Style == 0 {
				*probs = append(*probs, Problem{k.Line, k.Column, "merge keys (<<) are not supported; write the values out"})
				continue
			}
			if line, dup := first[k.Value]; dup {
				*probs = append(*probs, Problem{k.Line, k.Column, fmt.Sprintf("%s is defined twice (first at line %d)", k.Value, line)})
				continue
			}
			first[k.Value] = k.Line
			if lk := strings.ToLower(k.Value); lk == "regex" || lk == "pattern" {
				*probs = append(*probs, Problem{k.Line, k.Column, fmt.Sprintf(
					"%q: raw regular expressions are not allowed; write words and phrases in a word list, with * at a word end", k.Value)})
				continue
			}
			child := append(append([]string{}, path...), k.Value)
			t.keys[pathKey(child)] = position{line: k.Line, col: k.Column, raw: k.Value}
			m[k.Value] = t.convert(v, child, probs)
		}
		return m
	case yaml.SequenceNode:
		t.at[pathKey(path)] = here
		s := make([]any, 0, len(n.Content))
		for i, c := range n.Content {
			s = append(s, t.convert(c, append(append([]string{}, path...), strconv.Itoa(i)), probs))
		}
		return s
	case yaml.ScalarNode:
		here.quoted = n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0
		t.at[pathKey(path)] = here
		v, err := resolve(n.Value, here.quoted)
		if err != nil {
			*probs = append(*probs, Problem{n.Line, n.Column, err.Error()})
		}
		return v
	}
	*probs = append(*probs, Problem{n.Line, n.Column, "unsupported YAML node"})
	return nil
}

// resolve types a scalar by the YAML 1.2 core schema. Quoted scalars are
// always text, so "0800" and "1.10" keep every character.
func resolve(v string, quoted bool) (any, error) {
	if quoted {
		return v, nil
	}
	switch {
	case nullRe.MatchString(v):
		return nil, nil
	case boolRe.MatchString(v):
		return strings.ToLower(v) == "true", nil
	case intRe.MatchString(v):
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i, nil
		}
		return nil, fmt.Errorf("%s is too large a number", v)
	case octRe.MatchString(v):
		i, err := strconv.ParseInt(v[2:], 8, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is too large a number", v)
		}
		return i, nil
	case hexRe.MatchString(v):
		i, err := strconv.ParseInt(v[2:], 16, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is too large a number", v)
		}
		return i, nil
	case floatRe.MatchString(v):
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is not a usable number", v)
		}
		return f, nil
	case infRe.MatchString(v), nanRe.MatchString(v):
		return math.NaN(), fmt.Errorf("%s is not a usable number", v)
	}
	return v, nil
}

// where returns the position for a path, walking up to the nearest node
// that has one.
func (t *tree) where(path []string) position {
	for i := len(path); i >= 0; i-- {
		if p, ok := t.at[pathKey(path[:i])]; ok {
			return p
		}
	}
	return position{}
}

// keyAt returns the key node position of a mapping member, else where().
func (t *tree) keyAt(path []string) position {
	if p, ok := t.keys[pathKey(path)]; ok {
		return p
	}
	return t.where(path)
}
