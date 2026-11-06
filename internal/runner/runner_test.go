package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/abdussalamfaqih/streamx/internal/core"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/grpc"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/http"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/ws"
	"github.com/abdussalamfaqih/streamx/internal/runner"
	"github.com/abdussalamfaqih/streamx/internal/testserver"
)

var protoDir, _ = filepath.Abs("../../testdata/protos")

func serve(t *testing.T, host string, opts ...grpc.ServerOption) string {
	t.Helper()
	s, err := testserver.New(protoDir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	return host + ":" + port
}

func run(t *testing.T, o runner.Options, paths ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	o.Out = &out
	code := runner.Run(context.Background(), paths, o)
	return code, out.String()
}

// TestExampleCollection runs the whole example collection: every gRPC call
// type, both descriptor sources, TLS, mTLS, HTTP, SSE, WebSocket, and the
// HTTP-token-to-gRPC chain. It also checks the transcript never holds the token.
func TestExampleCollection(t *testing.T) {
	certs, err := testserver.NewCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(testserver.HTTP())
	t.Cleanup(web.Close)
	vars := map[string]string{
		"host":     serve(t, "localhost"),
		"tlsHost":  serve(t, "localhost", grpc.Creds(credentials.NewTLS(certs.ServerTLS(false)))),
		"mtlsHost": serve(t, "127.0.0.1", grpc.Creds(credentials.NewTLS(certs.ServerTLS(true)))),
		"certDir":  certs.Dir,
		"httpHost": strings.TrimPrefix(web.URL, "http://"),
	}
	var transcript bytes.Buffer
	code, out := run(t, runner.Options{Env: "local", Vars: vars, Verbose: true, Transcript: &transcript}, "../../testdata/collection")
	if code != runner.ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "14 passed, 0 failed") {
		t.Errorf("want 14 passes:\n%s", out)
	}
	// Requests run in seq order, so captures feed later requests.
	if strings.Index(out, "get token over HTTP") > strings.Index(out, "gRPC call with the token") ||
		strings.Index(out, "unary via reflection") > strings.Index(out, "chained") {
		t.Errorf("requests ran out of seq order:\n%s", out)
	}
	if strings.Contains(out, testserver.Token) || strings.Contains(transcript.String(), testserver.Token) {
		t.Errorf("captured token leaked:\n%s\n%s", out, transcript.String())
	}
	lines := strings.Split(strings.TrimSpace(transcript.String()), "\n")
	var first core.TranscriptLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Request == "" || first.Kind == "" {
		t.Errorf("transcript line = %s (%v)", lines[0], err)
	}
	if !strings.Contains(transcript.String(), `"request":"WebSocket echo"`) {
		t.Error("transcript lacks the WebSocket request")
	}
}

func collection(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	files["opencollection.yml"] = "opencollection: 1.0.0\n"
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const unary = `info:
  name: NAME
  type: grpc
grpc:
  url: grpc://{{host}}
  method: streamx.test.Echo/Unary
  metadata:
    - name: x-suffix
      value: "{{token}}"
  message: '{"text": "TEXT"}'
`

func req(name, text, extra string) string {
	return strings.NewReplacer("NAME", name, "TEXT", text).Replace(unary) + extra
}

func TestExitCodesAndRedaction(t *testing.T) {
	host := serve(t, "localhost")
	env := "name: ci\ndotEnvFilePath: .env\nvariables:\n  - name: token\n    secret: true\n"
	const secret = "tok-SUPER-SECRET-123"

	t.Run("failure is exit 1 and secrets are masked", func(t *testing.T) {
		dir := collection(t, map[string]string{
			"environments/ci.yml": env,
			".env":                "token=" + secret + "\n",
			"a.yml":               req("ok", "hi", ""),
			"b.yml": req("bad", "hi", `runtime:
  assertions:
    - expression: res.body.text
      operator: eq
      value: nope
`),
			"c.yml": req("failing status", "fail", ""),
		})
		code, out := run(t, runner.Options{Env: "ci", Vars: map[string]string{"host": host}, Verbose: true}, dir)
		if code != runner.ExitFail {
			t.Errorf("exit %d, want 1\n%s", code, out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("secret leaked into output:\n%s", out)
		}
		for _, want := range []string{"PASS ok", "FAIL bad", "got \"hi****\"", "FAIL failing status", "InvalidArgument", "1 passed, 2 failed"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("secret from OS env var", func(t *testing.T) {
		t.Setenv("token", secret)
		dir := collection(t, map[string]string{"environments/ci.yml": env, "a.yml": req("ok", "hi", "")})
		code, out := run(t, runner.Options{Env: "ci", Vars: map[string]string{"host": host}, Verbose: true}, dir)
		if code != runner.ExitOK || strings.Contains(out, secret) {
			t.Errorf("exit %d (leak=%v)\n%s", code, strings.Contains(out, secret), out)
		}
	})

	t.Run("config errors are exit 2", func(t *testing.T) {
		for name, files := range map[string]map[string]string{
			"bad operator": {"a.yml": req("x", "hi", "runtime:\n  assertions:\n    - expression: res.status\n      operator: matches\n      value: x\n")},
			"undefined":    {"a.yml": req("x", "hi", "")}, // {{token}} never defined
			"parse error":  {"a.yml": "info: [unclosed\n"},
			"bad type":     {"a.yml": "info:\n  type: mqtt\n"},
		} {
			code, out := run(t, runner.Options{Vars: map[string]string{"host": host}}, collection(t, files))
			if code != runner.ExitConfig {
				t.Errorf("%s: exit %d, want 2\n%s", name, code, out)
			}
		}
		if code, _ := run(t, runner.Options{}, filepath.Join(t.TempDir(), "*.yml")); code != runner.ExitConfig {
			t.Errorf("no matching files: exit %d, want 2", code)
		}
	})

	t.Run("dependent of a failed capture is exit 1, not 2", func(t *testing.T) {
		dir := collection(t, map[string]string{
			"a.yml": `info: {name: source, type: grpc, seq: 1}
grpc: {url: "grpc://{{host}}", method: streamx.test.Echo/Unary, message: '{"text": "fail"}'}
runtime:
  actions:
    - type: set-variable
      selector: {expression: res.body.text}
      variable: {name: got}
`,
			"b.yml": `info: {name: dependent, type: grpc, seq: 2}
grpc: {url: "grpc://{{host}}", method: streamx.test.Echo/Unary, message: '{"text": "{{got}}"}'}
`,
		})
		code, out := run(t, runner.Options{Vars: map[string]string{"host": host}}, dir)
		if code != runner.ExitFail || !strings.Contains(out, `skipped: needs "got", which "source" failed to capture`) {
			t.Errorf("exit %d, want 1 with skip reason\n%s", code, out)
		}
	})

	t.Run("unreachable server is exit 1", func(t *testing.T) {
		dir := collection(t, map[string]string{"a.yml": req("x", "hi", "")})
		code, out := run(t, runner.Options{Vars: map[string]string{"host": "127.0.0.1:1", "token": "t"}}, dir)
		if code != runner.ExitFail {
			t.Errorf("exit %d, want 1\n%s", code, out)
		}
	})
}
