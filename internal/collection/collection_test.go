package collection

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRoundTripIsByteIdentical(t *testing.T) {
	src, _ := os.ReadFile("testdata/bidi.yml")
	f, err := Load("testdata/bidi.yml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(src) {
		t.Errorf("load+save changed the file:\n%s", got)
	}
}

func TestEditKeepsCommentsAndOrder(t *testing.T) {
	f, err := Load("testdata/bidi.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		v    string
		keys []string
	}{
		{"streamx.test.Echo/Unary", []string{"grpc", "method"}},
		{"{\n  \"text\": \"edited\"\n}", []string{"grpc", "message"}},
		{"added", []string{"docs", "note"}},
	} {
		if err := f.Set(e.v, e.keys...); err != nil {
			t.Fatal(err)
		}
	}
	if f.Req.Method != "streamx.test.Echo/Unary" {
		t.Errorf("Req not re-read after Set: %q", f.Req.Method)
	}
	got, _ := f.Bytes()
	golden := "testdata/bidi.edited.golden"
	if *update {
		os.WriteFile(golden, got, 0o644)
	}
	want, _ := os.ReadFile(golden)
	if string(got) != string(want) {
		t.Errorf("edited file differs from %s:\n%s", golden, got)
	}
}

func TestDecode(t *testing.T) {
	f, err := Load("testdata/bidi.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := core.Request{
		Name: "chat", Type: "grpc", URL: "grpcs://{{host}}", Method: "streamx.test.Echo/Bidi",
		MethodType: "bidi-streaming", ProtoFile: "protos/echo.proto",
		Headers:     []core.KV{{Name: "authorization", Value: "Bearer {{token}}"}},
		Body:        `{"text": "hello"}`,
		Messages:    []string{`{"text":"a"}`, `{"text":"b"}`},
		TLS:         core.TLS{CAFile: "certs/ca.pem"},
		MaxMessages: 10, Timeout: 5 * time.Second, StreamStop: true,
		Assertions: []core.Assertion{{Expression: "res.messages.length", Operator: "eq", Value: "2"}},
		Captures:   []core.Capture{{Name: "last", Expression: "res.body.text"}},
	}
	if !reflect.DeepEqual(f.Req, want) {
		t.Errorf("got  %+v\nwant %+v", f.Req, want)
	}
	if f.Seq != 3 || len(f.Warnings) != 1 {
		t.Errorf("seq=%d warnings=%v", f.Seq, f.Warnings)
	}
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRootAndVariants(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "opencollection.yml", `opencollection: 1.0.0
config:
  protobuf:
    importPaths:
      - path: protos
      - path: off
        disabled: true
  clientCertificates:
    - domain: "*.local"
      type: pem
      certificateFilePath: c.pem
      privateKeyFilePath: k.pem
`)
	p := write(t, dir, "sub/ws.yml", `info:
  type: ws
ws:
  url: ws://x
  message:
    - title: one
      message: {type: json, data: "1"}
    - title: two
      selected: true
      message: {type: json, data: "2"}
settings:
  timeout: 1500
`)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Req.Type != "websocket" || f.Req.Body != "2" || f.Req.Timeout != 1500*time.Millisecond || !f.Req.StreamStop {
		t.Errorf("ws request = %+v", f.Req)
	}
	if !reflect.DeepEqual(f.Req.ImportPaths, []string{"protos"}) || f.Req.Name != "ws.yml" {
		t.Errorf("root not applied: %+v", f.Req)
	}
	root, _ := LoadRoot(f.Root)
	req, err := ApplyCert(core.Request{URL: "grpcs://api.local:443"}, root.Certs)
	if err != nil || req.TLS.CertFile != "c.pem" {
		t.Errorf("cert not matched: %+v %v", req.TLS, err)
	}
	req = ResolvePaths(req, dir)
	if req.TLS.CertFile != filepath.Join(dir, "c.pem") {
		t.Errorf("path not resolved: %s", req.TLS.CertFile)
	}
	if req, _ := ApplyCert(core.Request{URL: "grpcs://other:443"}, root.Certs); req.TLS.CertFile != "" {
		t.Error("cert matched the wrong host")
	}
	if req, _ := ApplyCert(core.Request{URL: "grpc://api.local:80"}, root.Certs); req.TLS.CertFile != "" {
		t.Error("cert applied to a plaintext request")
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"type":   "info:\n  type: mqtt\n",
		"form":   "info:\n  type: http\nhttp:\n  body:\n    type: form-urlencoded\n",
		"action": "info:\n  type: http\nhttp: {}\nruntime:\n  actions:\n    - type: run-script\n",
		"nogrpc": "info:\n  type: grpc\n",
	} {
		if _, err := Load(write(t, dir, name+".yml", body)); err == nil {
			t.Errorf("%s: want error", name)
		} else if !strings.Contains(err.Error(), name+".yml") {
			t.Errorf("%s: error lacks file name: %v", name, err)
		}
	}
}

func TestIsRequestFile(t *testing.T) {
	for p, want := range map[string]bool{
		"a/get.yml": true, "a/get.yaml": true, "opencollection.yml": false, "a/folder.yml": false,
		"environments/local.yml": false, "a/notes.md": false,
	} {
		if IsRequestFile(p) != want {
			t.Errorf("IsRequestFile(%s) = %v", p, !want)
		}
	}
}

func TestNewSetHeaderSaveAs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "opencollection.yml", "opencollection: 1.0.0\n")
	f, err := New(dir, core.Request{Name: "Unary", Type: "grpc", URL: "grpc://{{host}}", Method: "pkg.Svc/Unary", MethodType: "unary", Body: `{"text":""}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetHeader("Authorization", "Bearer {{token}}"); err != nil {
		t.Fatal(err)
	}
	f.SetHeader("x-a", "1")
	f.SetHeader("authorization", "Bearer {{other}}") // case-insensitive update
	f.SetHeader("x-a", "")                           // remove
	p := filepath.Join(dir, "svc", "unary.yml")
	if err := f.SaveAs(p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := `info:
  name: Unary
  type: grpc
grpc:
  url: grpc://{{host}}
  method: pkg.Svc/Unary
  methodType: unary
  message: '{"text":""}'
  metadata:
    - name: Authorization
      value: Bearer {{other}}
`
	if string(got) != want {
		t.Errorf("saved:\n%s\nwant:\n%s", got, want)
	}
	if err := f.SaveAs(p); err == nil {
		t.Error("SaveAs overwrote an existing file")
	}
	g, err := Load(p)
	if err != nil || g.Req.Headers[0].Value != "Bearer {{other}}" || g.Req.Body != `{"text":""}` {
		t.Errorf("reload: %+v %v", g.Req, err)
	}
}

func TestSetFailureLeavesDocUntouched(t *testing.T) {
	f, err := Load("testdata/bidi.yml")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.Bytes()
	if err := f.Set("x", "info", "name", "deeper"); err == nil {
		t.Fatal("want error: info.name is a scalar")
	}
	if err := f.Set("x", "grpc", "metadata"); err == nil {
		t.Fatal("want error: grpc.metadata is a list")
	}
	if err := f.Set("x", "brand", "new", "info", "name", "x"); err == nil {
		// creates brand.new.info.name.x — fine, but check the failing ones above changed nothing
		after, _ := f.Bytes()
		if !strings.HasPrefix(string(after), string(before)) {
			t.Errorf("failed Set calls changed the document:\n%s", after)
		}
	}
	ws, err := New(t.TempDir(), core.Request{Type: "websocket", URL: "ws://x", Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Req.Body != "hi" || ws.Req.URL != "ws://x" {
		t.Errorf("websocket New: %+v", ws.Req)
	}
}
