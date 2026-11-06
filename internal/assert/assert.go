// Package assert evaluates OpenCollection assertions (Bruno operator subset)
// and capture expressions against a collected response.
//
// Expressions: res.status, res.statusText, res.headers.<name>, res.body.<path>
// (last received message), res.messages.length, res.messages[N].<path>,
// res.messages[*].<path> (every message must pass). "$.<path>" and a bare
// "<path>" mean res.body.<path>. Paths use .key, [N], ['key'] and .length.
package assert

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

// Response is what a request produced.
type Response struct {
	Status     int
	StatusText string
	Headers    map[string]string
	Messages   []json.RawMessage
}

// Add records an event: received messages, headers and trailers, and the status.
func (r *Response) Add(ev core.Event) {
	switch ev.Kind {
	case core.Received:
		r.Messages = append(r.Messages, ev.Data)
	case core.Headers, core.Trailers:
		if r.Headers == nil {
			r.Headers = map[string]string{}
		}
		for k, v := range ev.Meta {
			r.Headers[k] = v
		}
	case core.Status:
		r.Status, r.StatusText = ev.Code, ev.Text
	}
}

func (r *Response) root() (map[string]any, error) {
	msgs := make([]any, len(r.Messages))
	for i, m := range r.Messages {
		if err := json.Unmarshal(m, &msgs[i]); err != nil {
			msgs[i] = string(m) // non-JSON payloads (text frames) compare as strings
		}
	}
	headers := map[string]any{}
	for k, v := range r.Headers {
		headers[strings.ToLower(k)] = v
	}
	root := map[string]any{"status": float64(r.Status), "statusText": r.StatusText, "headers": headers, "messages": msgs}
	if len(msgs) > 0 {
		root["body"] = msgs[len(msgs)-1]
	}
	return root, nil
}

type step struct {
	key  string
	idx  int
	kind byte // 'k' key, 'i' index, '*' wildcard
}

// normalize maps "$", "$.path" and a bare "path" onto the res.* form, minus "res.".
func normalize(e string) string {
	switch {
	case strings.HasPrefix(e, "res."):
		return e[len("res."):]
	case e == "res":
		return ""
	case strings.HasPrefix(e, "$"):
		return "body" + e[1:]
	}
	return "body." + e
}

func parse(expr string) ([]step, error) {
	e := normalize(strings.TrimSpace(expr))
	var steps []step
	for i := 0; i < len(e); {
		switch e[i] {
		case '.':
			i++
		case '[':
			end := strings.IndexByte(e[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("bad expression %q: missing ]", expr)
			}
			s, err := bracket(e[i+1 : i+end])
			if err != nil {
				return nil, fmt.Errorf("bad expression %q: %w", expr, err)
			}
			steps = append(steps, s)
			i += end + 1
		default:
			j := i + strings.IndexAny(e[i:]+".", ".[")
			steps = append(steps, step{kind: 'k', key: e[i:j]})
			i = j
		}
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("bad expression %q", expr)
	}
	return steps, nil
}

// bracket parses the inside of [...]: *, 'key', "key" or an index.
func bracket(in string) (step, error) {
	switch {
	case in == "*":
		return step{kind: '*'}, nil
	case len(in) >= 2 && (in[0] == '\'' || in[0] == '"') && in[len(in)-1] == in[0]:
		return step{kind: 'k', key: in[1 : len(in)-1]}, nil
	}
	n, err := strconv.Atoi(in)
	if err != nil {
		return step{}, fmt.Errorf("index %q", in)
	}
	return step{kind: 'i', idx: n}, nil
}

// match is one value an expression resolved to.
type match struct {
	v     any
	found bool
}

func walk(v any, found bool, steps []step) []match {
	if len(steps) == 0 || !found {
		return []match{{v, found}}
	}
	s, rest := steps[0], steps[1:]
	switch s.kind {
	case '*':
		var out []match
		for _, x := range children(v) {
			out = append(out, walk(x, true, rest)...)
		}
		return out
	case 'i':
		x, ok := index(v, s.idx)
		return walk(x, ok, rest)
	}
	x, ok := field(v, s.key)
	return walk(x, ok, rest)
}

// children lists the elements of an array or the values of an object.
func children(v any) []any {
	switch c := v.(type) {
	case []any:
		return c
	case map[string]any:
		out := make([]any, 0, len(c))
		for _, x := range c {
			out = append(out, x)
		}
		return out
	}
	return nil
}

// index returns element i of an array; [-1] is the last element.
func index(v any, i int) (any, bool) {
	a, ok := v.([]any)
	if ok && i < 0 {
		i += len(a)
	}
	if !ok || i < 0 || i >= len(a) {
		return nil, false
	}
	return a[i], true
}

// field returns an object key, or .length of an object, array or string.
func field(v any, key string) (any, bool) {
	if m, ok := v.(map[string]any); ok {
		if x, ok := m[key]; ok {
			return x, true
		}
	}
	if key != "length" {
		return nil, false
	}
	switch c := v.(type) {
	case map[string]any:
		return float64(len(c)), true
	case []any:
		return float64(len(c)), true
	case string:
		return float64(len(c)), true
	}
	return nil, false
}

func eval(expr string, r *Response) ([]match, bool, error) {
	steps, err := parse(expr)
	if err != nil {
		return nil, false, err
	}
	root, err := r.root()
	if err != nil {
		return nil, false, err
	}
	wild := false
	for _, s := range steps {
		wild = wild || s.kind == '*'
	}
	return walk(root, true, steps), wild, nil
}

// Value resolves a capture expression. Captures take the first match.
func Value(expr string, r *Response) (string, bool, error) {
	ms, _, err := eval(expr, r)
	if err != nil || len(ms) == 0 || !ms[0].found || ms[0].v == nil {
		return "", false, err
	}
	if s, ok := ms[0].v.(string); ok {
		return s, true, nil
	}
	b, _ := json.Marshal(ms[0].v)
	return string(b), true, nil
}

// Operators is the supported Bruno operator subset.
var Operators = map[string]bool{
	"eq": true, "neq": true, "gt": true, "gte": true, "lt": true, "lte": true,
	"contains": true, "exists": true, "isString": true, "isNumber": true, "isBool": true,
}

// Check runs one assertion. failure is empty when it passed; err is a
// problem with the assertion itself (unknown operator, bad expression).
func Check(a core.Assertion, r *Response) (failure string, err error) {
	if !Operators[a.Operator] {
		return "", fmt.Errorf("assertion %q: operator %q is not supported (supported: eq neq gt gte lt lte contains exists isString isNumber isBool)", a.Expression, a.Operator)
	}
	ms, wild, err := eval(a.Expression, r)
	if err != nil {
		return "", err
	}
	if wild && len(ms) == 0 {
		return fmt.Sprintf("%s %s %s: no values matched", a.Expression, a.Operator, a.Value), nil
	}
	for _, m := range ms {
		if !check(a.Operator, m, a.Value) {
			got := "<missing>"
			if m.found {
				b, _ := json.Marshal(m.v)
				got = string(b)
			}
			return strings.TrimSpace(fmt.Sprintf("%s %s %s: got %s", a.Expression, a.Operator, a.Value, got)), nil
		}
	}
	return "", nil
}

func check(op string, m match, raw string) bool {
	if op == "exists" {
		return m.found && m.v != nil
	}
	if !m.found {
		return false
	}
	var want any = raw
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		want = raw // unquoted text compares as a string
	}
	switch op {
	case "eq":
		return equal(m.v, want, raw)
	case "neq":
		return !equal(m.v, want, raw)
	case "gt", "gte", "lt", "lte":
		return compare(op, m.v, want)
	case "contains":
		return contains(m.v, want, raw)
	}
	return isType(op, m.v)
}

func compare(op string, got, want any) bool {
	a, ok1 := num(got)
	b, ok2 := num(want)
	if !ok1 || !ok2 {
		return false
	}
	return map[string]bool{"gt": a > b, "gte": a >= b, "lt": a < b, "lte": a <= b}[op]
}

// contains checks a substring of a string, or an element of an array.
func contains(got, want any, raw string) bool {
	switch c := got.(type) {
	case string:
		s, ok := want.(string)
		if !ok {
			s = raw
		}
		return strings.Contains(c, s)
	case []any:
		for _, x := range c {
			if equal(x, want, raw) {
				return true
			}
		}
	}
	return false
}

func isType(op string, v any) bool {
	ok := false
	switch op {
	case "isString":
		_, ok = v.(string)
	case "isNumber":
		_, ok = v.(float64)
	case "isBool":
		_, ok = v.(bool)
	}
	return ok
}

func equal(got, want any, raw string) bool {
	if reflect.DeepEqual(got, want) {
		return true
	}
	if a, ok := num(got); ok {
		if b, ok := num(want); ok {
			return a == b
		}
	}
	s, ok := got.(string)
	return ok && s == raw // "123" header vs eq 123
}

// num accepts numbers and, for protojson int64 values, numeric strings.
func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}
