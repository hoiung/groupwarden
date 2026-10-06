package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/hoiung/groupwarden/schema"
)

const schemaURL = "https://github.com/hoiung/groupwarden/schema/config.schema.json"

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	schemaDoc   map[string]any
	compileErr  error
)

// configSchema compiles the embedded schema once, with the "duration"
// keyword registered.
func configSchema() (*jsonschema.Schema, map[string]any, error) {
	compileOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.Config))
		if err != nil {
			compileErr = fmt.Errorf("embedded schema: %w", err)
			return
		}
		schemaDoc, _ = doc.(map[string]any)
		c := jsonschema.NewCompiler()
		// Draft 2020-12 ignores a registered vocabulary unless assertions
		// are switched on; without this the duration bounds never run.
		c.AssertVocabs()
		c.RegisterVocabulary(durationVocab())
		if err := c.AddResource(schemaURL, doc); err != nil {
			compileErr = fmt.Errorf("embedded schema: %w", err)
			return
		}
		compiled, compileErr = c.Compile(schemaURL)
	})
	return compiled, schemaDoc, compileErr
}

// ---- the "duration" keyword: {"minimum": "1m", "maximum": "47h"} ----------

type durationBound struct{ min, max time.Duration }

func durationVocab() *jsonschema.Vocabulary {
	return &jsonschema.Vocabulary{
		URL: "https://github.com/hoiung/groupwarden/schema/vocab/duration",
		Compile: func(_ *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
			raw, ok := obj["duration"].(map[string]any)
			if !ok {
				return nil, nil
			}
			var b durationBound
			for key, dst := range map[string]*time.Duration{"minimum": &b.min, "maximum": &b.max} {
				s, _ := raw[key].(string)
				d, err := time.ParseDuration(s)
				if err != nil {
					return nil, fmt.Errorf("duration.%s %q: %w", key, s, err)
				}
				*dst = d
			}
			return &b, nil
		},
	}
}

func (b *durationBound) Validate(ctx *jsonschema.ValidatorContext, v any) {
	s, ok := v.(string)
	if !ok {
		return
	}
	d, err := time.ParseDuration(s)
	switch {
	case err != nil:
		ctx.AddError(&durationKind{got: s, msg: fmt.Sprintf("%q is not a duration like \"47h\" or \"90m\"", s)})
	case d < b.min:
		ctx.AddError(&durationKind{got: s, msg: fmt.Sprintf("%s is below the minimum %s", s, short(b.min))})
	case d > b.max:
		ctx.AddError(&durationKind{got: s, msg: fmt.Sprintf("%s is above the maximum %s", s, short(b.max))})
	}
}

type durationKind struct{ got, msg string }

func (*durationKind) KeywordPath() []string                     { return []string{"duration"} }
func (k *durationKind) LocalizedString(*message.Printer) string { return k.msg }

// short prints 47h0m0s as 47h.
func short(d time.Duration) string {
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	return strings.TrimSuffix(s, "0m")
}

// ---- defaults ----------------------------------------------------------------

// applyDefaults fills every absent property that has a schema default,
// descending into objects, so the schema is the only place defaults live.
func applyDefaults(v any, sch map[string]any) {
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	sch = deref(sch)
	props, _ := sch["properties"].(map[string]any)
	for _, name := range sortedNames(props) {
		ps := deref(asMap(props[name]))
		cur, present := obj[name]
		if !present {
			def, has := ps["default"]
			if !has {
				continue
			}
			cur = deepCopy(def)
			obj[name] = cur
		}
		applyDefaults(cur, ps)
		if arr, ok := cur.([]any); ok {
			items := deref(asMap(ps["items"]))
			for _, it := range arr {
				applyDefaults(it, items)
			}
		}
	}
	if ap := deref(asMap(sch["additionalProperties"])); ap != nil {
		for name, val := range obj {
			if _, declared := props[name]; !declared {
				applyDefaults(val, ap)
			}
		}
	}
}

// deref follows a local "$ref": "#/$defs/x".
func deref(s map[string]any) map[string]any {
	for s != nil {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		defs, _ := schemaDoc["$defs"].(map[string]any)
		next := asMap(defs[strings.TrimPrefix(ref, "#/$defs/")])
		for k, v := range s { // sibling keywords (default, description) beside $ref
			if k != "$ref" {
				if next == nil {
					next = map[string]any{}
				}
				merged := map[string]any{k: v}
				for nk, nv := range next {
					merged[nk] = nv
				}
				next = merged
			}
		}
		s = next
	}
	return s
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func sortedNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func deepCopy(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	out, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return v
	}
	return out
}

// ---- validation and plain-English errors --------------------------------------

var printer = message.NewPrinter(language.English)

// validate checks t against the schema and returns plain-English problems.
func validate(t *tree) Problems {
	sch, _, err := configSchema()
	if err != nil {
		return Problems{{Msg: err.Error()}}
	}
	err = sch.Validate(t.root)
	var ve *jsonschema.ValidationError
	if err == nil || !errors.As(err, &ve) {
		if err != nil {
			return Problems{{Msg: err.Error()}}
		}
		return nil
	}
	var probs Problems
	seen := map[string]bool{}
	for _, leaf := range leaves(ve) {
		p := explain(t, leaf)
		if k := p.String(); !seen[k] {
			seen[k] = true
			probs = append(probs, p)
		}
	}
	return probs
}

// leaves returns the innermost errors (the ones that say what is wrong).
func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func dotted(path []string) string {
	if len(path) == 0 {
		return "the config"
	}
	return strings.Join(path, ".")
}

func explain(t *tree, e *jsonschema.ValidationError) Problem {
	path := e.InstanceLocation
	pos := t.where(path)
	at := func(p position, msg string) Problem { return Problem{Line: p.line, Column: p.col, Msg: msg} }
	key := dotted(path)
	switch k := e.ErrorKind.(type) {
	case *kind.Type:
		if contains(k.Want, "string") && pos.raw != "" && !pos.quoted {
			return at(pos, fmt.Sprintf("%s must be in quotes", pos.raw))
		}
		return at(pos, fmt.Sprintf("%s must be %s", key, typeWords(k.Want)))
	case *kind.Minimum:
		return at(pos, fmt.Sprintf("%s %s is below the minimum %s", key, ratString(k.Got), ratString(k.Want)))
	case *kind.Maximum:
		return at(pos, fmt.Sprintf("%s %s is above the maximum %s", key, ratString(k.Got), ratString(k.Want)))
	case *kind.Enum:
		var want []string
		for _, w := range k.Want {
			want = append(want, fmt.Sprint(w))
		}
		return at(pos, fmt.Sprintf("%s must be one of %s (got %v)", key, strings.Join(want, ", "), k.Got))
	case *kind.Required:
		return at(pos, fmt.Sprintf("%s is required", dotted(append(append([]string{}, path...), k.Missing...))))
	case *kind.AdditionalProperties:
		child := append(append([]string{}, path...), k.Properties[0])
		return at(t.keyAt(child), fmt.Sprintf("%s is not a setting groupwarden knows", dotted(child)))
	case *kind.MinItems:
		return at(pos, fmt.Sprintf("%s needs at least %d entr%s", key, k.Want, plural(k.Want)))
	case *kind.MinLength:
		return at(pos, fmt.Sprintf("%s must not be empty", key))
	case *kind.MinProperties, *kind.MaxProperties:
		return at(pos, fmt.Sprintf("%s must be exactly one of all, any, words or has", key))
	case *kind.Pattern:
		// The value is not repeated: a URL or key can carry a secret, and this
		// message reaches logs and the admin chat.
		return at(pos, fmt.Sprintf("%s is not in the expected form", key))
	case *kind.PropertyNames:
		child := append(append([]string{}, path...), k.Property)
		return at(t.keyAt(child), fmt.Sprintf("%q: names use lower-case letters, digits, - and _", k.Property))
	case *durationKind:
		return at(pos, fmt.Sprintf("%s %s", key, k.msg))
	}
	return at(pos, fmt.Sprintf("%s: %s", key, e.ErrorKind.LocalizedString(printer)))
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func typeWords(want []string) string {
	words := map[string]string{"string": "text", "integer": "a whole number", "number": "a number",
		"boolean": "true or false", "array": "a list", "object": "a section of settings", "null": "empty"}
	var out []string
	for _, w := range want {
		out = append(out, words[w])
	}
	return strings.Join(out, " or ")
}

func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return fmt.Sprint(f)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// wordPaths lists every word and phrase position: they must be quoted, so
// "0800" or "NO" are never read as a number or a boolean.
func wordPaths(root any) [][]string {
	var out [][]string
	add := func(prefix []string, v any) {
		for i := range asSlice(v) {
			out = append(out, append(append([]string{}, prefix...), fmt.Sprint(i)))
		}
	}
	top := asMap(root)
	for name, list := range asMap(top["word_lists"]) {
		add([]string{"word_lists", name}, list)
	}
	add([]string{"never_match"}, top["never_match"])
	for id, c := range asMap(top["communities"]) {
		for name, list := range asMap(asMap(c)["word_lists"]) {
			add([]string{"communities", id, "word_lists", name}, list)
		}
	}
	return out
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// unquotedWords reports words and phrases written without quotes.
func unquotedWords(t *tree) Problems {
	var probs Problems
	for _, p := range wordPaths(t.root) {
		pos, ok := t.at[pathKey(p)]
		if ok && !pos.quoted {
			probs = append(probs, Problem{Line: pos.line, Column: pos.col, Msg: fmt.Sprintf("%s must be in quotes", pos.raw)})
		}
	}
	return probs
}
