package assert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

var resp = &Response{
	Status: 0, StatusText: "OK",
	Headers: map[string]string{"X-Count": "123", "content-type": "application/grpc"},
	Messages: []json.RawMessage{
		[]byte(`{"text":"a","n":1,"big":"9007199254740993"}`),
		[]byte(`{"text":"b","n":2,"tags":["x","y"],"ok":true,"user":{"id":7,"my key":"v"}}`),
	},
}

// tc is an assertion test case; core.Assertion literals must be keyed (go vet).
type tc struct{ e, o, v string }

func (c tc) A() core.Assertion { return core.Assertion{Expression: c.e, Operator: c.o, Value: c.v} }

func TestCheck(t *testing.T) {
	pass := []tc{
		{"res.status", "eq", "0"},
		{"res.statusText", "eq", "OK"},
		{"res.statusText", "eq", `"OK"`},
		{"res.headers.x-count", "eq", "123"},
		{"res.headers.x-count", "gt", "100"},
		{"res.body.text", "eq", "b"},
		{"$.user.id", "eq", "7"},
		{"user['my key']", "eq", "v"},
		{"res.messages.length", "eq", "2"},
		{"res.messages[0].text", "eq", "a"},
		{"res.messages[-1].n", "gte", "2"},
		{"res.messages[*].n", "gt", "0"},
		{"res.messages[*].text", "isString", ""},
		{"res.messages[0].big", "gt", "1"},
		{"res.body.tags", "contains", "x"},
		{"res.body.tags.length", "lte", "2"},
		{"res.body.text", "contains", "b"},
		{"res.body.ok", "isBool", ""},
		{"res.body.n", "isNumber", ""},
		{"res.body.user", "exists", ""},
		{"res.body.text", "neq", "a"},
	}
	for _, a := range pass {
		if f, err := Check(a.A(), resp); f != "" || err != nil {
			t.Errorf("%v: failure=%q err=%v", a, f, err)
		}
	}
	fail := []tc{
		{"res.status", "eq", "3"},
		{"res.messages[*].n", "eq", "2"},
		{"res.body.missing", "exists", ""},
		{"res.body.missing", "eq", "x"},
		{"res.body.text", "gt", "1"},
		{"res.body.n", "isString", ""},
		{"res.messages[5].text", "eq", "a"},
		{"res.messages[*].nope[*]", "eq", "1"},
	}
	for _, a := range fail {
		if f, err := Check(a.A(), resp); f == "" || err != nil {
			t.Errorf("%v: want failure, got failure=%q err=%v", a, f, err)
		}
	}
	if f, _ := Check(tc{"res.status", "eq", "3"}.A(), resp); !strings.Contains(f, "got 0") {
		t.Errorf("failure message lacks actual value: %q", f)
	}
	for _, a := range []tc{{"res.status", "matches", "x"}, {"res.messages[x]", "eq", "1"}, {"res.body[0", "eq", "1"}} {
		if _, err := Check(a.A(), resp); err == nil {
			t.Errorf("%v: want config error", a)
		}
	}
}

func TestValue(t *testing.T) {
	for expr, want := range map[string]string{"res.body.text": "b", "user.id": "7", "res.body.tags": `["x","y"]`} {
		if got, ok, err := Value(expr, resp); !ok || err != nil || got != want {
			t.Errorf("Value(%s) = %q %v %v, want %q", expr, got, ok, err, want)
		}
	}
	if _, ok, _ := Value("res.body.none", resp); ok {
		t.Error("missing value reported as found")
	}
	if _, ok, _ := Value("res.body.text", &Response{}); ok {
		t.Error("value found with no messages")
	}
}
