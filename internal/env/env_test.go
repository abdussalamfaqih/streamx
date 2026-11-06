package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "opencollection.yml"), `opencollection: 1.0.0
config:
  environments:
    - name: base
      variables:
        - name: host
          value: base.local
        - name: scheme
          value: grpc
`)
	write(t, filepath.Join(root, "environments", "local.yml"), `name: local
extends: base
dotEnvFilePath: .env
variables:
  - name: host
    value: localhost:50051
  - name: url
    value: "{{scheme}}://{{host}}"
  - name: typed
    value: {type: number, data: "42"}
  - name: pick
    value:
      - title: a
        value: one
      - title: b
        selected: true
        value: two
  - name: off
    value: x
    disabled: true
  - name: token
    secret: true
  - name: fromOS
    secret: true
  - name: unset
    secret: true
`)
	write(t, filepath.Join(root, ".env"), "# comment\ntoken=\"s3cr\\\"et-token\"\nexport OTHER=x\n")
	return root
}

func TestLoadAndExpand(t *testing.T) {
	root := setup(t)
	t.Setenv("fromOS", "os-secret-value")
	t.Setenv("STREAMX_PE", "pe-secret-value")
	e, err := Load(root, "local")
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"{{url}}":                      "grpc://localhost:50051",
		"{{ typed }}/{{pick}}":         "42/two",
		"Bearer {{token}}":             `Bearer s3cr\"et-token`,
		"{{fromOS}}":                   "os-secret-value",
		"{{process.env.STREAMX_PE}}":   "pe-secret-value",
		"{{process.env.OTHER}}":        "x",
		"no {{ placeholders here":      "no {{ placeholders here",
		`{"literal": "{{not a var}}"}`: `{"literal": "{{not a var}}"}`,
	} {
		got, err := e.Expand(in)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"{{off}}", "{{unset}}", "{{nope}}", "{{process.env.STREAMX_NOT_SET_X}}"} {
		if _, err := e.Expand(in); err == nil || !strings.Contains(err.Error(), "undefined") {
			t.Errorf("Expand(%q) err = %v, want undefined", in, err)
		}
	}
}

func TestRedact(t *testing.T) {
	root := setup(t)
	t.Setenv("fromOS", "os-secret-value")
	t.Setenv("STREAMX_PE", "pe-secret-value")
	e, err := Load(root, "local")
	if err != nil {
		t.Fatal(err)
	}
	e.Expand("{{process.env.STREAMX_PE}}") // referenced process.env values become secrets
	e.Set("captured", "captured-token", true)
	e.Set("plain", "localhost", false)

	secrets := []string{`s3cr"et-token`, "os-secret-value", "pe-secret-value", "captured-token"}
	in := make(chan core.Event, 1)
	in <- core.Event{
		Kind: core.Received,
		Data: []byte(`{"token":"s3cr\"et-token","host":"localhost"}`),
		Meta: map[string]string{"authorization": "Bearer os-secret-value"},
		Err:  "denied for pe-secret-value / captured-token",
	}
	close(in)
	ev := <-e.RedactEvents(in)
	all := string(ev.Data) + ev.Meta["authorization"] + ev.Err
	for _, s := range secrets {
		if strings.Contains(all, s) || strings.Contains(all, `s3cr\"et-token`) {
			t.Errorf("secret %q leaked: %s", s, all)
		}
	}
	if !strings.Contains(string(ev.Data), "localhost") || !strings.Contains(string(ev.Data), mask) {
		t.Errorf("over- or under-redacted: %s", ev.Data)
	}
}

func TestExpandRequestDoesNotTouchOriginal(t *testing.T) {
	e := New()
	e.Set("v", "X", false)
	orig := core.Request{URL: "{{v}}", Headers: []core.KV{{Name: "h", Value: "{{v}}"}}, Messages: []string{"{{v}}"},
		Assertions: []core.Assertion{{Expression: "res.status", Operator: "eq", Value: "{{v}}"}}}
	got, err := e.ExpandRequest(orig)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "X" || got.Headers[0].Value != "X" || got.Messages[0] != "X" || got.Assertions[0].Value != "X" {
		t.Errorf("not expanded: %+v", got)
	}
	if orig.Headers[0].Value != "{{v}}" || orig.Messages[0] != "{{v}}" || orig.Assertions[0].Value != "{{v}}" {
		t.Errorf("original mutated: %+v", orig)
	}
}

func TestMissingEnvironment(t *testing.T) {
	if _, err := Load(t.TempDir(), "prod"); err == nil || !strings.Contains(err.Error(), `"prod" not found`) {
		t.Errorf("err = %v", err)
	}
}

func TestCaptureMasksOnlyLongValues(t *testing.T) {
	e := New()
	e.Capture("id", "1")
	e.Capture("token", "eyJhbGciOi.long")
	if got := e.Redact(`{"id":1,"n":10,"token":"eyJhbGciOi.long"}`); got != `{"id":1,"n":10,"token":"****"}` {
		t.Errorf("got %s", got)
	}
	if v, _ := e.Expand("{{id}}/{{token}}"); v != "1/eyJhbGciOi.long" {
		t.Errorf("captures not usable as variables: %s", v)
	}
}
