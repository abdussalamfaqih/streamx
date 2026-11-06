package tui

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdussalamfaqih/streamx/internal/core"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/grpc"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/http"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/ws"
	"github.com/abdussalamfaqih/streamx/internal/testserver"
)

// driver is a minimal Bubble Tea loop: commands run concurrently and their
// messages are fed back into Update, like the real program does.
type driver struct {
	t    *testing.T
	m    *model
	msgs chan tea.Msg
	quit bool
}

func newDriver(t *testing.T, dir, envName string) *driver {
	t.Helper()
	m, err := newModel(Options{Dir: dir, Env: envName})
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{t: t, m: m, msgs: make(chan tea.Msg, 1024)}
	d.send(tea.WindowSizeMsg{Width: 140, Height: 45})
	return d
}

func (d *driver) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { d.msgs <- cmd() }()
}

func (d *driver) send(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			d.exec(c)
		}
	case tea.QuitMsg:
		d.quit = true
	default:
		_, cmd := d.m.Update(msg)
		d.exec(cmd)
	}
}

func (d *driver) keys(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg.Code = tea.KeyEnter
		case "esc":
			msg.Code = tea.KeyEscape
		case "tab":
			msg.Code = tea.KeyTab
		case "ctrl+d":
			msg.Code, msg.Mod = 'd', tea.ModCtrl
		default:
			msg.Code, msg.Text = []rune(k)[0], k
		}
		d.send(msg)
	}
}

// typeText sends each rune as a key press.
func (d *driver) typeText(s string) {
	for _, r := range s {
		d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// pick selects a collection request with the fuzzy finder.
func (d *driver) pick(query, wantName string) {
	d.t.Helper()
	d.keys("/")
	d.typeText(query)
	d.keys("enter")
	if d.m.cur == nil || d.m.cur.Req.Name != wantName {
		d.t.Fatalf("picker %q selected %v, want %q", query, d.m.cur, wantName)
	}
}

func (d *driver) view() string { return ansi.Strip(d.m.View().Content) }

func (d *driver) waitFor(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case msg := <-d.msgs:
			d.send(msg)
		case <-deadline:
			d.t.Fatalf("timed out waiting for %s\nstatus: %s\n%s", what, d.m.status, d.view())
		}
	}
}

func (d *driver) waitDone() {
	d.t.Helper()
	d.waitFor("call to finish", func() bool { return strings.HasPrefix(d.m.status, "done in") })
}

// setup copies the example collection and protos to a temp dir and starts the Echo server.
func setup(t *testing.T) (dir string) {
	t.Helper()
	tmp := t.TempDir()
	for _, src := range []string{"../../testdata/collection", "../../testdata/protos"} {
		if err := os.CopyFS(filepath.Join(tmp, filepath.Base(src)), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	protoDir := filepath.Join(tmp, "protos")
	s, err := testserver.New(protoDir)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	web := httptest.NewServer(testserver.HTTP())
	t.Cleanup(web.Close)
	// Point the local environment at these servers.
	envFile := filepath.Join(tmp, "collection", "environments", "local.yml")
	b, _ := os.ReadFile(envFile)
	env := strings.NewReplacer("localhost:50051", lis.Addr().String(), "localhost:8080", strings.TrimPrefix(web.URL, "http://")).Replace(string(b))
	os.WriteFile(envFile, []byte(env), 0o644)
	return filepath.Join(tmp, "collection")
}

func TestUnaryCallAndCapture(t *testing.T) {
	d := newDriver(t, setup(t), "local")
	files := d.m.lists[tabCollection]
	if len(files) != 14 || files[0].label != "get token over HTTP" {
		t.Fatalf("collection = %d entries, first %q", len(files), files[0].label)
	}
	d.pick("unary via refl", "unary via reflection")
	if !strings.Contains(d.view(), "grpc://{{host}}") {
		t.Errorf("summary should show raw placeholders:\n%s", d.view())
	}
	d.keys("r")
	d.waitDone()
	v := d.view()
	for _, want := range []string{`"text": "hi!"`, "← {", "status OK (0)", "pass res.body.text eq hi!", "captured echoed", "# headers"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if got, _ := d.m.env.Expand("{{echoed}}"); got != "hi!" {
		t.Errorf("capture not stored: %q", got)
	}
	// compact mode puts the message on one line
	d.keys("l", "c")
	if !strings.Contains(d.view(), `← {"text":"hi!","n":1}`) {
		t.Errorf("compact view:\n%s", d.view())
	}
}

func TestBidiInteractive(t *testing.T) {
	d := newDriver(t, setup(t), "local")
	d.pick("bidi", "bidi streaming")
	d.keys("r")
	d.waitFor("stream to start", func() bool { return d.m.focus == focusInput })
	d.waitFor("file messages echoed", func() bool { return len(d.m.call.resp.Messages) == 2 })

	d.typeText(`{"text":"typed","n":41}`)
	d.keys("enter")
	d.waitFor("typed message echoed", func() bool { return len(d.m.call.resp.Messages) == 3 })
	if !strings.Contains(d.view(), `"n": 42`) {
		t.Errorf("typed echo missing:\n%s", d.view())
	}

	d.typeText("not json")
	d.keys("enter")
	if !d.m.isErr || !strings.Contains(d.m.status, "not valid JSON") || d.m.call.send == nil {
		t.Errorf("bad JSON should be refused locally and keep the stream: %q", d.m.status)
	}
	d.keys("ctrl+d") // end our side of the stream
	d.waitDone()
	if !strings.Contains(d.view(), "FAIL res.messages.length eq 2: got 3") {
		t.Errorf("assertion result missing:\n%s", d.view())
	}
}

func TestServicesNewEditSave(t *testing.T) {
	dir := setup(t)
	d := newDriver(t, dir, "local")
	d.pick("unary via refl", "unary via reflection") // the services tab browses this request's server
	d.keys("tab")
	d.waitFor("services", func() bool { return len(d.m.lists[tabServices]) == 5 })
	if v := d.view(); !strings.Contains(v, "streamx.test.Echo") || !strings.Contains(v, "ServerStream server") {
		t.Errorf("services tree:\n%s", v)
	}
	d.keys("/")
	d.typeText("serverstr")
	d.keys("enter")
	if d.m.cur.Path != "" || !d.m.dirty || d.m.cur.Req.MethodType != "server-streaming" {
		t.Fatalf("want a new unsaved request, got %+v dirty=%v", d.m.cur.Req, d.m.dirty)
	}

	editorCmd = func(path string, whole bool) tea.Cmd {
		os.WriteFile(path, []byte(`{"text": "z", "count": 2}`+"\n"), 0o600)
		return func() tea.Msg { return editorMsg{path: path, whole: whole} }
	}
	orig := editorCmd
	t.Cleanup(func() { editorCmd = orig })
	d.keys("e")
	d.waitFor("body edit", func() bool { return d.m.cur.Req.Body == `{"text": "z", "count": 2}` })

	d.keys("r")
	d.waitDone()
	if n := len(d.m.call.resp.Messages); n != 2 {
		t.Errorf("received %d, want 2", n)
	}

	d.keys("w")
	if d.m.focus != focusCmd || d.m.line.Value() != "w echo/server-stream.yml" {
		t.Fatalf("save prompt = %q", d.m.line.Value())
	}
	d.keys("enter")
	saved := filepath.Join(dir, "echo", "server-stream.yml")
	b, err := os.ReadFile(saved)
	if err != nil || d.m.dirty {
		t.Fatalf("not saved: %v (dirty=%v) status=%q", err, d.m.dirty, d.m.status)
	}
	for _, want := range []string{"url: grpc://{{host}}", "method: streamx.test.Echo/ServerStream", "methodType: server-streaming", `"count": 2`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("saved file lacks %q:\n%s", want, b)
		}
	}
	if len(d.m.lists[tabCollection]) != 15 {
		t.Errorf("collection not reloaded: %d entries", len(d.m.lists[tabCollection]))
	}
}

func TestCommandsAndQuitGuard(t *testing.T) {
	dir := setup(t)
	d := newDriver(t, dir, "local")
	d.pick("unary via refl", "unary via reflection")
	d.keys(":")
	d.typeText("header authorization Bearer {{token}}")
	d.keys("enter", ":")
	d.typeText("set url grpc://other:1")
	d.keys("enter")
	if !d.m.dirty || d.m.cur.Req.URL != "grpc://other:1" || d.m.cur.Req.Headers[1].Value != "Bearer {{token}}" {
		t.Fatalf("edits not applied: %+v", d.m.cur.Req)
	}
	d.keys("q")
	if d.quit || !strings.Contains(d.m.status, "unsaved changes") {
		t.Fatalf("q with unsaved changes should ask first (status %q)", d.m.status)
	}
	d.keys("j") // any other key cancels the pending quit
	d.keys("q")
	if d.quit {
		t.Fatal("quit after the confirmation was cancelled")
	}
	d.keys("w")
	b, _ := os.ReadFile(filepath.Join(dir, "grpc", "unary.yml"))
	if !strings.Contains(string(b), "url: grpc://other:1") || !strings.Contains(string(b), "value: Bearer {{token}}") {
		t.Errorf("saved file:\n%s", b)
	}
	if !strings.HasPrefix(string(b), "info:\n  name: unary via reflection\n") {
		t.Errorf("save reordered keys:\n%s", b)
	}
	d.keys("q")
	d.waitFor("quit", func() bool { return d.quit })
}

func TestSecretsNeverRendered(t *testing.T) {
	d := newDriver(t, setup(t), "local")
	const secret = "sekret-token-value"
	d.m.env.Set("token", secret, true)
	d.pick("unary via refl", "unary via reflection")
	d.keys(":")
	d.typeText("header x-suffix {{token}}") // the server echoes x-suffix into the body
	d.keys("enter", "r")
	d.waitDone()
	if v := d.view(); strings.Contains(v, secret) || !strings.Contains(v, "hi****") {
		t.Errorf("secret visible or not masked:\n%s", v)
	}
	if got := string(d.m.call.resp.Messages[0]); !strings.Contains(got, secret) {
		t.Errorf("assertions must see raw values, got %s", got)
	}
}

func TestStreamBuffer(t *testing.T) {
	var s stream
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for i := range maxEvents + 5 {
		s.add(core.Event{Time: at, Kind: core.Received, Data: []byte(`{"i":` + strconv.Itoa(i) + `}`)})
	}
	if len(s.items) != maxEvents || s.dropped != 5 {
		t.Fatalf("items=%d dropped=%d", len(s.items), s.dropped)
	}
	s.compact = true
	if v := s.view(80, 2); !strings.HasSuffix(ansi.Strip(v), `{"i":10004}`) {
		t.Errorf("tail = %q", ansi.Strip(v))
	}
	s.togglePause()
	s.add(core.Event{Time: at, Kind: core.Received, Data: []byte(`{"i":"new"}`)})
	if strings.Contains(s.view(80, 3), "new") || s.pending() != 1 {
		t.Error("paused view shows new events")
	}
	s.togglePause()
	s.filter = "1000"
	if v := ansi.Strip(s.view(80, 50)); strings.Count(v, "←") != 6 { // 1000 and 10000..10004
		t.Errorf("filter matched %d lines:\n%s", strings.Count(v, "←"), v)
	}
	s.filter, s.offset = "", 1_000_000
	s.view(80, 10)
	if s.offset != maxEvents-10 {
		t.Errorf("offset not clamped: %d", s.offset)
	}
}

func TestDefaultPath(t *testing.T) {
	for in, want := range map[string]string{
		"streamx.test.Echo/ServerStream": "echo/server-stream.yml",
		"/pkg.UserAPI/GetUser":           "user-api/get-user.yml",
		"Svc/do_thing":                   "svc/do-thing.yml",
		"pkg.HTTPServer/V2Call":          "http-server/v2-call.yml",
	} {
		if got := defaultPath(core.Request{Type: "grpc", Method: in}); got != want {
			t.Errorf("defaultPath(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestWebSocketSSEAndTranscript(t *testing.T) {
	d := newDriver(t, setup(t), "local")
	d.pick("websocket echo", "WebSocket echo")
	d.keys("r")
	d.waitFor("socket open", func() bool { return d.m.focus == focusInput })
	d.waitFor("file messages echoed", func() bool { return len(d.m.call.resp.Messages) == 2 })
	d.typeText("free text, not JSON")
	d.keys("enter")
	d.waitFor("text echoed", func() bool { return len(d.m.call.resp.Messages) == 3 })
	d.typeText("bye") // the server closes normally
	d.keys("enter")
	d.waitDone()
	if v := d.view(); !strings.Contains(v, `← "free text, not JSON"`) || !strings.Contains(v, "StatusNormalClosure") {
		t.Errorf("websocket view:\n%s", v)
	}

	d.keys("h")
	d.pick("sse tick", "SSE ticks")
	d.keys("r")
	d.waitDone()
	if n := len(d.m.call.resp.Messages); n != 5 { // the TUI shows the whole stream; maxMessages is for run
		t.Errorf("SSE messages = %d, want 5", n)
	}

	// Transcript: s prompts with a name in the current directory.
	t.Chdir(t.TempDir())
	d.keys("s")
	if !strings.HasPrefix(d.m.line.Value(), "transcript sse-ticks-") {
		t.Fatalf("transcript prompt = %q", d.m.line.Value())
	}
	d.keys("enter")
	files, _ := filepath.Glob("sse-ticks-*.jsonl")
	if len(files) != 1 {
		t.Fatalf("transcript files: %v (status %q)", files, d.m.status)
	}
	b, _ := os.ReadFile(files[0])
	if n := strings.Count(string(b), `"kind":"received"`); n != 5 || !strings.Contains(string(b), `"request":"SSE ticks"`) {
		t.Errorf("transcript has %d received lines:\n%s", n, b)
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("transcript mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestCapturedTokenMaskedInTUI(t *testing.T) {
	d := newDriver(t, setup(t), "local")
	d.pick("get token", "get token over HTTP")
	d.keys("r")
	d.waitDone()
	if v := d.view(); strings.Contains(v, testserver.Token) || !strings.Contains(v, "captured token") {
		t.Errorf("token visible after capture:\n%s", v)
	}
	d.keys("h")
	d.pick("grpc call with", "gRPC call with the token")
	d.keys("r")
	d.waitDone()
	if !strings.Contains(d.view(), "pass res.body.text eq hi-") {
		t.Errorf("chained call:\n%s", d.view())
	}
}

func TestFirstSaveCreatesCollection(t *testing.T) {
	src := setup(t) // for the servers and env; browse them from an empty folder
	b, _ := os.ReadFile(filepath.Join(src, "environments", "local.yml"))
	host := strings.Fields(strings.SplitN(string(b), "value: ", 2)[1])[0]

	dir := t.TempDir()
	d := newDriver(t, dir, "")
	if len(d.m.lists[tabCollection]) != 0 {
		t.Fatal("empty folder should list nothing")
	}
	d.keys(":")
	d.typeText("connect " + host)
	d.keys("enter")
	d.waitFor("services", func() bool { return len(d.m.lists[tabServices]) == 5 })
	d.keys("enter", "w", "enter") // first method, save at the suggested path
	for _, f := range []string{"opencollection.yml", ".gitignore", "echo/bidi.yml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not created: %v (status %q)", f, err, d.m.status)
		}
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(gi), ".env") {
		t.Errorf(".gitignore = %q", gi)
	}
	if !strings.Contains(d.m.status, "new collection") {
		t.Errorf("status = %q", d.m.status)
	}
}

func TestIdleAfterFailedCall(t *testing.T) {
	d := newDriver(t, setup(t), "local")
	d.pick("http echo", "HTTP echo")
	d.keys(":")
	d.typeText("set url http://127.0.0.1:1/echo") // nothing listens: the call ends with an error, no status
	d.keys("enter", "r")
	d.waitDone()
	if v := d.view(); !strings.Contains(v, "Stream  idle") || strings.Contains(v, "receiving") {
		t.Errorf("stream should be idle after a failed call:\n%s", v)
	}
}
