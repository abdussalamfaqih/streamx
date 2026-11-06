// Package tui is the interactive terminal UI: a sidebar (collection files and
// a services tree), a request summary, and a live stream view.
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/sahilm/fuzzy"

	"github.com/abdussalamfaqih/streamx/internal/assert"
	"github.com/abdussalamfaqih/streamx/internal/collection"
	"github.com/abdussalamfaqih/streamx/internal/core"
	grpcengine "github.com/abdussalamfaqih/streamx/internal/engine/grpc"
	"github.com/abdussalamfaqih/streamx/internal/env"
	"github.com/abdussalamfaqih/streamx/internal/runner"
)

type Options struct {
	Dir string // collection directory
	Env string
}

// Run starts the TUI and blocks until the user quits.
func Run(o Options) error {
	m, err := newModel(o)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(m).Run()
	return err
}

type focus int

const (
	focusSidebar focus = iota
	focusStream
	focusInput  // message box of an interactive stream
	focusCmd    // ":" command line
	focusPick   // fuzzy picker over the sidebar list
	focusFilter // stream filter
)

type tab int

const (
	tabCollection tab = iota
	tabServices
)

type entry struct {
	label  string
	file   *collection.File
	method *grpcengine.Method
	err    error
}

type model struct {
	root    string
	envName string
	env     *env.Env
	width   int
	height  int

	tab     tab
	lists   [2][]entry
	cursor  [2]int
	svcURL  string // raw url for requests created from the services tree
	svcSrc  *collection.File
	svcMsg  string // what the services tab shows: source or loading/error text
	connect string // :connect target overriding the selected request

	cur   *collection.File
	dirty bool

	focus   focus
	back    focus // focus to return to after cmd/pick/filter
	line    textinput.Model
	input   textinput.Model
	picks   []int
	pickSel int

	stream  stream
	call    *call
	callSeq int

	status  string
	isErr   bool
	pending string // action waiting for confirmation (discard unsaved changes)
}

type call struct {
	id          int
	req         core.Request
	cancel      context.CancelFunc
	send        chan []byte // nil once half-closed
	events      <-chan core.Event
	interactive bool
	done        bool
	resp        assert.Response // raw values, for assertions and captures only
	start       time.Time
}

// Messages from commands.
type (
	startedMsg struct {
		id  int
		c   *call
		err error
	}
	eventsMsg struct {
		id   int
		evs  []core.Event
		done bool
	}
	methodsMsg struct {
		methods []grpcengine.Method
		err     error
	}
	editorMsg struct {
		path  string
		whole bool // edited the whole request file, not just the body
		err   error
	}
)

func newModel(o Options) (*model, error) {
	if o.Dir == "" {
		o.Dir = "."
	}
	root := collection.FindRoot(o.Dir)
	e, err := env.Load(root, o.Env)
	if err != nil {
		return nil, err
	}
	m := &model{root: root, envName: o.Env, env: e, line: textinput.New(), input: textinput.New()}
	m.line.Prompt = ""
	m.input.Prompt = "> "
	m.input.Placeholder = "JSON message, enter sends, ctrl+d ends the stream"
	if err := m.loadFiles(); err != nil {
		return nil, err
	}
	if len(m.lists[tabCollection]) > 0 {
		m.selectEntry(0)
	}
	m.setStatus(false, "? for keys")
	return m, nil
}

func (m *model) loadFiles() error {
	paths, err := runner.Expand([]string{m.root})
	if err != nil && !strings.Contains(err.Error(), "no request files") {
		return err
	}
	var files []*collection.File
	var broken []entry // listed last so they can be opened with E and fixed
	for _, p := range paths {
		f, err := collection.Load(p)
		if err != nil {
			broken = append(broken, entry{label: m.rel(p), err: err, file: &collection.File{Path: p}})
			continue
		}
		files = append(files, f)
	}
	runner.Sort(files)
	es := make([]entry, 0, len(files)+len(broken))
	for _, f := range files {
		es = append(es, entry{label: f.Req.Name, file: f})
	}
	m.lists[tabCollection] = append(es, broken...)
	m.cursor[tabCollection] = min(m.cursor[tabCollection], max(0, len(m.lists[tabCollection])-1))
	return nil
}

func (m *model) rel(p string) string {
	if r, err := filepath.Rel(m.root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

func (m *model) setStatus(isErr bool, format string, a ...any) {
	m.status, m.isErr = m.env.Redact(fmt.Sprintf(format, a...)), isErr
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case startedMsg:
		return m, m.started(msg)
	case eventsMsg:
		return m, m.events(msg)
	case methodsMsg:
		m.methodsLoaded(msg)
		return m, nil
	case editorMsg:
		m.edited(msg)
		return m, nil
	case tea.KeyPressMsg:
		return m, m.key(msg)
	}
	// Anything else (cursor blink, paste) goes to the focused text input.
	return m, m.updateInputs(msg)
}

func (m *model) updateInputs(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m.focus {
	case focusInput:
		m.input, cmd = m.input.Update(msg)
	case focusCmd, focusPick, focusFilter:
		m.line, cmd = m.line.Update(msg)
	}
	return cmd
}

// --- keys ----------------------------------------------------------------------

func (m *model) key(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	switch {
	case key == "ctrl+c":
		return m.quit(false)
	case m.lineOpen():
		return m.lineKey(key, k)
	case m.focus == focusInput:
		return m.inputKey(key, k)
	}
	if key != "enter" && key != "q" {
		m.pending = "" // any other key cancels a pending confirmation
	}
	if fn := globalKeys[key]; fn != nil {
		return fn(m)
	}
	if m.focus == focusStream {
		return m.streamKey(key)
	}
	return m.sidebarKey(key)
}

// globalKeys work in both panes.
var globalKeys = map[string]func(*model) tea.Cmd{
	"q":      func(m *model) tea.Cmd { return m.quit(false) },
	"tab":    (*model).switchTab,
	":":      func(m *model) tea.Cmd { return m.openLine(focusCmd, "") },
	"r":      (*model).send,
	"ctrl+r": (*model).send,
	"x":      (*model).cancelCall,
	"i":      (*model).typeMessage,
	"e":      (*model).editBody,
	"E":      (*model).editFile,
	"w":      func(m *model) tea.Cmd { return m.save("") },
	"s":      (*model).promptTranscript,
	"?":      (*model).help,
}

func (m *model) switchTab() tea.Cmd {
	m.tab, m.focus = 1-m.tab, focusSidebar
	if m.tab == tabServices {
		return m.loadMethods()
	}
	return nil
}

func (m *model) cancelCall() tea.Cmd {
	if m.call != nil {
		m.call.cancel()
		m.setStatus(false, "cancelled")
	}
	return nil
}

// typeMessage goes back into the message box of a running stream.
func (m *model) typeMessage() tea.Cmd {
	if !m.inputVisible() {
		return nil
	}
	m.focus = focusInput
	return m.input.Focus()
}

// promptTranscript opens the command line prefilled with a transcript file name.
func (m *model) promptTranscript() tea.Cmd {
	if len(m.stream.items) == 0 {
		m.setStatus(true, "nothing to save yet")
		return nil
	}
	name := "stream"
	if m.cur != nil {
		name = or(kebab(m.cur.Req.Name), name)
	}
	return m.openLine(focusCmd, "transcript "+name+"-"+time.Now().Format("20060102-150405")+".jsonl")
}

func (m *model) help() tea.Cmd {
	m.setStatus(false, "j/k move · enter open · tab collection/services · / find · r send · x cancel · i type message · e edit body · E edit file · w save · s save transcript · : command (set url, header, env, connect, w, q) · h/l panes · in stream: p pause, c compact, G follow")
	return nil
}

func (m *model) sidebarKey(key string) tea.Cmd {
	list := m.lists[m.tab]
	c := &m.cursor[m.tab]
	switch key {
	case "j", "down":
		*c = min(*c+1, max(0, len(list)-1))
	case "k", "up":
		*c = max(*c-1, 0)
	case "g", "home":
		*c = 0
	case "G", "end":
		*c = max(0, len(list)-1)
	case "l", "right":
		m.focus = focusStream
	case "/":
		m.picks, m.pickSel = nil, 0
		cmd := m.openLine(focusPick, "")
		m.updatePicks()
		return cmd
	case "enter":
		return m.selectEntry(*c)
	}
	return nil
}

func (m *model) streamKey(key string) tea.Cmd {
	s := &m.stream
	switch key {
	case "p", "space":
		s.togglePause()
	case "c":
		s.compact = !s.compact
	case "/":
		return m.openLine(focusFilter, s.filter)
	case "h", "left":
		m.focus = focusSidebar
	default:
		m.scroll(key)
	}
	s.view(m.streamWidth(), m.streamHeight()) // clamps offset
	return nil
}

func (m *model) scroll(key string) {
	s := &m.stream
	page := max(1, m.streamHeight()-1)
	switch key {
	case "j", "down":
		s.offset = max(0, s.offset-1)
	case "k", "up":
		s.offset++
	case "ctrl+d", "pgdown":
		s.offset = max(0, s.offset-page)
	case "ctrl+u", "pgup":
		s.offset += page
	case "g", "home":
		s.offset = maxEvents * 50 // clamped by view
	case "G", "end":
		s.offset = 0
	}
}

func (m *model) inputKey(key string, k tea.KeyPressMsg) tea.Cmd {
	switch key {
	case "esc":
		m.input.Blur()
		m.focus = focusStream
	case "ctrl+d":
		m.halfClose()
	case "enter":
		m.sendInput()
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	}
	return nil
}

// sendInput sends the message box's text on the running stream.
func (m *model) sendInput() {
	v := strings.TrimSpace(m.input.Value())
	if v == "" {
		return
	}
	if !m.inputVisible() {
		m.setStatus(true, "the stream is closed")
		return
	}
	b, err := m.message(v)
	if err != nil {
		m.setStatus(true, "message not sent: %v", err)
		return
	}
	select {
	case m.call.send <- b:
		m.input.SetValue("")
	default:
		m.setStatus(true, "stream is busy, try again")
	}
}

// message expands placeholders in v; gRPC needs JSON, WebSocket frames may be any text.
func (m *model) message(v string) ([]byte, error) {
	expanded, err := m.env.Expand(v)
	if err != nil {
		return nil, err
	}
	if m.call.req.Type == "grpc" && !json.Valid([]byte(expanded)) {
		return nil, errors.New("not valid JSON")
	}
	return []byte(expanded), nil
}

func (m *model) lineKey(key string, k tea.KeyPressMsg) tea.Cmd {
	switch key {
	case "esc":
		if m.focus == focusFilter {
			m.stream.filter = ""
		}
		m.closeLine()
		return nil
	case "enter":
		return m.submitLine()
	case "up", "ctrl+p":
		m.pickSel = max(0, m.pickSel-1)
		return nil
	case "down", "ctrl+n":
		m.pickSel = min(max(0, len(m.picks)-1), m.pickSel+1)
		return nil
	}
	var cmd tea.Cmd
	m.line, cmd = m.line.Update(k)
	switch m.focus {
	case focusPick:
		m.updatePicks()
	case focusFilter:
		m.stream.filter = m.line.Value() // filter as you type
	}
	return cmd
}

func (m *model) openLine(f focus, value string) tea.Cmd {
	if !m.lineOpen() {
		m.back = m.focus
	}
	m.focus = f
	m.line.SetValue(value)
	m.line.CursorEnd()
	return m.line.Focus()
}

// lineOpen reports whether the bottom line (command, find or filter) has focus.
func (m *model) lineOpen() bool {
	return m.focus == focusCmd || m.focus == focusPick || m.focus == focusFilter
}

func (m *model) closeLine() {
	m.line.Blur()
	m.focus = m.back
	m.picks = nil
}

// submitLine acts on the bottom line's text when enter is pressed.
func (m *model) submitLine() tea.Cmd {
	v, mode, picks, sel := m.line.Value(), m.focus, m.picks, m.pickSel
	m.closeLine()
	switch mode {
	case focusCmd:
		return m.command(v)
	case focusFilter:
		m.stream.filter = v
	case focusPick:
		if len(picks) > 0 {
			m.cursor[m.tab] = picks[sel]
			return m.selectEntry(m.cursor[m.tab])
		}
	}
	return nil
}

func (m *model) updatePicks() {
	list := m.lists[m.tab]
	q := m.line.Value()
	m.picks = m.picks[:0]
	if q == "" {
		for i := range list {
			m.picks = append(m.picks, i)
		}
	} else {
		labels := make([]string, len(list))
		for i, e := range list {
			labels[i] = e.label + " " + m.rel(pathOf(e))
		}
		for _, match := range fuzzy.Find(q, labels) {
			m.picks = append(m.picks, match.Index)
		}
	}
	m.pickSel = min(m.pickSel, max(0, len(m.picks)-1))
}

func pathOf(e entry) string {
	if e.file != nil {
		return e.file.Path
	}
	return ""
}

// guard asks before an action that would drop unsaved changes. It returns
// true when the action may go ahead.
func (m *model) guard(action string) bool {
	if !m.dirty || m.pending == action {
		m.pending = ""
		return true
	}
	m.pending = action
	m.setStatus(true, "unsaved changes: w saves, %s again discards them", map[string]string{"quit": "q", "switch": "enter"}[action])
	return false
}

func (m *model) quit(force bool) tea.Cmd {
	if !force && !m.guard("quit") {
		return nil
	}
	if m.call != nil {
		m.call.cancel()
	}
	return tea.Quit
}

// --- selecting -----------------------------------------------------------------

func (m *model) selectEntry(i int) tea.Cmd {
	list := m.lists[m.tab]
	if i < 0 || i >= len(list) {
		return nil
	}
	e := list[i]
	if e.err != nil {
		m.setStatus(true, "%v (E opens the file)", e.err)
		m.cur, m.dirty = e.file, false
		return nil
	}
	if e.file == m.cur && e.method == nil {
		return nil
	}
	if !m.guard("switch") {
		return nil
	}
	if e.method != nil {
		return m.newFromMethod(*e.method)
	}
	m.cur, m.dirty = e.file, false
	m.setStatus(false, "%s", m.rel(e.file.Path))
	for _, w := range e.file.Warnings {
		m.setStatus(false, "warning: %s", w)
	}
	return nil
}

// loadMethods lists services at the :connect target or the selected request's server.
func (m *model) loadMethods() tea.Cmd {
	var req core.Request
	switch {
	case m.connect != "":
		req = core.Request{Type: "grpc", URL: m.connect}
		m.svcURL, m.svcSrc = m.connect, nil
	case m.cur != nil && m.cur.Req.Type == "grpc" && m.cur.Root != "":
		r, err := runner.Prepare(m.cur, m.env)
		if err != nil {
			m.svcMsg = m.env.Redact(err.Error())
			m.lists[tabServices] = nil
			return nil
		}
		req, m.svcURL, m.svcSrc = r, m.cur.Req.URL, m.cur
	default:
		m.svcMsg = "select a gRPC request, or :connect host:port"
		m.lists[tabServices] = nil
		return nil
	}
	m.svcMsg = "loading services from " + m.env.Redact(req.URL) + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ms, err := grpcengine.Methods(ctx, req)
		return methodsMsg{ms, err}
	}
}

func (m *model) methodsLoaded(msg methodsMsg) {
	if msg.err != nil {
		m.svcMsg = m.env.Redact(msg.err.Error())
		m.lists[tabServices] = nil
		return
	}
	var es []entry
	for i := range msg.methods {
		mt := msg.methods[i]
		es = append(es, entry{label: mt.Service + "/" + mt.Name, method: &mt})
	}
	m.lists[tabServices] = es
	m.cursor[tabServices] = min(m.cursor[tabServices], max(0, len(es)-1))
	m.svcMsg = m.env.Redact(m.svcURL)
}

// newFromMethod makes an unsaved request for a method in the services tree.
func (m *model) newFromMethod(mt grpcengine.Method) tea.Cmd {
	body := mt.Template
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(body), "", "  ") == nil {
		body = pretty.String()
	}
	req := core.Request{Name: mt.Name, Type: "grpc", URL: m.svcURL, Method: mt.FullName(), MethodType: mt.Type, Body: body}
	src := m.svcSrc
	if src != nil {
		req.ProtoFile = src.Req.ProtoFile
	}
	f, err := collection.New(m.root, req)
	if err != nil {
		m.setStatus(true, "%v", err)
		return nil
	}
	if src != nil { // keep the source's auth metadata and TLS settings
		for _, h := range src.Req.Headers {
			f.SetHeader(h.Name, h.Value)
		}
		// shortcut: copies caFile/insecure only; client certs come from config anyway.
		if src.Req.TLS.CAFile != "" {
			f.Set(src.Req.TLS.CAFile, "extensions", "streamx", "tls", "caFile")
		}
		if src.Req.TLS.Insecure {
			f.Set("true", "extensions", "streamx", "tls", "insecure")
		}
	}
	m.cur, m.dirty = f, true
	m.stream.reset()
	m.setStatus(false, "new %s request for %s: e edits the body, r sends, w saves", mt.Type, mt.FullName())
	return nil
}

// --- commands --------------------------------------------------------------------

func (m *model) command(line string) tea.Cmd {
	f := strings.Fields(line)
	if len(f) == 0 {
		return nil
	}
	run := commands[f[0]]
	if run == nil {
		m.setStatus(true, "unknown command %q", f[0])
		return nil
	}
	return run(m, f, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0])))
}

// commands maps a command name to its handler. f is the line split into
// words; arg is the text after the command name.
var commands = map[string]func(m *model, f []string, arg string) tea.Cmd{
	"q":          func(m *model, _ []string, _ string) tea.Cmd { return m.quit(false) },
	"quit":       func(m *model, _ []string, _ string) tea.Cmd { return m.quit(false) },
	"q!":         func(m *model, _ []string, _ string) tea.Cmd { return m.quit(true) },
	"w":          func(m *model, _ []string, arg string) tea.Cmd { return m.save(arg) },
	"write":      func(m *model, _ []string, arg string) tea.Cmd { return m.save(arg) },
	"wq":         (*model).writeQuit,
	"set":        (*model).setURL,
	"header":     (*model).setHeader,
	"h":          (*model).setHeader,
	"env":        (*model).setEnv,
	"transcript": func(m *model, _ []string, arg string) tea.Cmd { return m.saveTranscript(arg) },
	"connect":    (*model).connectTo,
}

func (m *model) writeQuit(_ []string, arg string) tea.Cmd {
	if cmd := m.save(arg); m.dirty {
		return cmd
	}
	return m.quit(true)
}

func (m *model) setURL(f []string, _ string) tea.Cmd {
	if len(f) < 3 || f[1] != "url" {
		m.setStatus(true, "usage: set url <url>")
		return nil
	}
	return m.edit(func(cur *collection.File) error { return cur.Set(f[2], cur.Section(), "url") })
}

func (m *model) setHeader(f []string, arg string) tea.Cmd {
	if len(f) < 2 {
		m.setStatus(true, "usage: header <name> [value]  (no value removes it)")
		return nil
	}
	return m.edit(func(cur *collection.File) error {
		return cur.SetHeader(f[1], strings.TrimSpace(strings.TrimPrefix(arg, f[1])))
	})
}

func (m *model) setEnv(_ []string, arg string) tea.Cmd {
	e, err := env.Load(m.root, arg)
	if err != nil {
		m.setStatus(true, "%v", err)
		return nil
	}
	m.env, m.envName = e, arg
	m.setStatus(false, "environment: %s", or(arg, "none"))
	return nil
}

// connectTo browses a server; no address goes back to the selected request's.
func (m *model) connectTo(_ []string, arg string) tea.Cmd {
	m.connect = arg
	m.tab, m.focus = tabServices, focusSidebar
	return m.loadMethods()
}

// saveTranscript writes the stream buffer (already redacted) as JSON lines.
// Paths are relative to the directory streamx was started in, so transcripts
// stay out of the collection unless you put them there.
func (m *model) saveTranscript(path string) tea.Cmd {
	if path == "" {
		m.setStatus(true, "usage: transcript <file.jsonl>")
		return nil
	}
	f, err := core.CreateTranscript(path, false)
	if err != nil {
		m.setStatus(true, "transcript not saved: %v", err)
		return nil
	}
	name := ""
	if m.cur != nil {
		name = m.cur.Req.Name
	}
	for _, it := range m.stream.items {
		if err == nil {
			err = core.WriteTranscript(f, name, it.ev)
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		m.setStatus(true, "transcript: %v", err)
		return nil
	}
	msg := fmt.Sprintf("saved %d events to %s", len(m.stream.items), path)
	if m.stream.dropped > 0 {
		msg += fmt.Sprintf(" (the oldest %d were already dropped from the buffer)", m.stream.dropped)
	}
	m.setStatus(false, "%s", msg)
	return nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// haveRequest reports whether a loaded request is open, and says so if not.
// A broken file is listed but has no collection root.
func (m *model) haveRequest() bool {
	if m.cur == nil || m.cur.Root == "" {
		m.setStatus(true, "no request selected")
		return false
	}
	return true
}

func (m *model) edit(fn func(*collection.File) error) tea.Cmd {
	if !m.haveRequest() {
		return nil
	}
	if err := fn(m.cur); err != nil {
		m.setStatus(true, "%v", err)
		return nil
	}
	m.dirty = true
	m.setStatus(false, "changed: w saves")
	return nil
}

// save writes the current request. A new request needs a path: the command
// line opens prefilled with one derived from the method.
func (m *model) save(arg string) tea.Cmd {
	if !m.haveRequest() {
		return nil
	}
	if m.cur.Path == "" {
		return m.saveNew(arg)
	}
	if arg != "" && arg != m.rel(m.cur.Path) {
		m.setStatus(true, "this request is already saved as %s", m.rel(m.cur.Path))
		return nil
	}
	if err := m.cur.Save(); err != nil {
		m.setStatus(true, "not saved: %v", err)
		return nil
	}
	m.dirty = false
	m.setStatus(false, "saved %s", m.rel(m.cur.Path))
	return nil
}

// saveNew writes a request that has no file yet; the first save in a folder
// also makes it a collection.
func (m *model) saveNew(arg string) tea.Cmd {
	if arg == "" {
		return m.openLine(focusCmd, "w "+defaultPath(m.cur.Req))
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.root, p)
	}
	if !collection.IsRequestFile(p) {
		m.setStatus(true, "%s: a request file must end in .yml and not be a root, folder or environment file", arg)
		return nil
	}
	created, err := collection.InitRoot(m.root)
	if err == nil {
		err = m.cur.SaveAs(p)
	}
	if err != nil {
		m.setStatus(true, "not saved: %v", err)
		return nil
	}
	m.dirty = false
	m.reselect(p)
	if created {
		m.setStatus(false, "saved %s (new collection: wrote opencollection.yml and .gitignore)", m.rel(p))
	} else {
		m.setStatus(false, "saved %s", m.rel(p))
	}
	return nil
}

// reselect reloads the collection list and selects the file at path.
func (m *model) reselect(path string) (entry, bool) {
	m.loadFiles()
	for i, e := range m.lists[tabCollection] {
		if e.file != nil && e.file.Path == path {
			m.cursor[tabCollection], m.cur = i, e.file
			return e, true
		}
	}
	return entry{}, false
}

// defaultPath is <service>/<method>.yml in kebab case, e.g. echo/server-stream.yml.
func defaultPath(r core.Request) string {
	svc, method, _ := strings.Cut(strings.TrimPrefix(r.Method, "/"), "/")
	if i := strings.LastIndex(svc, "."); i >= 0 {
		svc = svc[i+1:]
	}
	return kebab(or(svc, r.Type)) + "/" + kebab(or(method, or(r.Name, "request"))) + ".yml"
}

func kebab(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case unicode.IsUpper(r):
			if wordStart(rs, i) {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// wordStart reports whether the capital at rs[i] starts a word: after a
// lower-case letter or digit, or at the last capital of an acronym (HTTPServer).
func wordStart(rs []rune, i int) bool {
	if i == 0 {
		return false
	}
	p := rs[i-1]
	return unicode.IsLower(p) || unicode.IsDigit(p) || unicode.IsUpper(p) && i+1 < len(rs) && unicode.IsLower(rs[i+1])
}

// --- editor ----------------------------------------------------------------------

// editorCmd runs the user's editor on path. Tests replace it.
var editorCmd = func(path string, whole bool) tea.Cmd {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
		if runtime.GOOS == "windows" {
			ed = "notepad"
		}
	}
	args := strings.Fields(ed) // allows EDITOR="code -w"
	c := exec.Command(args[0], append(args[1:], path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return editorMsg{path, whole, err} })
}

func (m *model) editBody() tea.Cmd {
	if !m.haveRequest() {
		return nil
	}
	tmp, err := os.CreateTemp("", "streamx-*.json")
	if err == nil {
		_, err = tmp.WriteString(m.cur.Req.Body)
		tmp.Close()
	}
	if err != nil {
		m.setStatus(true, "%v", err)
		return nil
	}
	return editorCmd(tmp.Name(), false)
}

func (m *model) editFile() tea.Cmd {
	if m.cur == nil || m.cur.Path == "" {
		m.setStatus(true, "save the request first (w)")
		return nil
	}
	if m.dirty {
		m.setStatus(true, "save or discard changes before editing the whole file")
		return nil
	}
	return editorCmd(m.cur.Path, true)
}

func (m *model) edited(msg editorMsg) {
	if msg.err != nil {
		m.setStatus(true, "editor: %v", msg.err)
	}
	if msg.whole {
		m.reloaded(msg)
		return
	}
	defer os.Remove(msg.path)
	if msg.err != nil {
		return
	}
	b, err := os.ReadFile(msg.path)
	if err != nil {
		m.setStatus(true, "%v", err)
		return
	}
	body := strings.TrimRight(string(b), "\n")
	if body == m.cur.Req.Body {
		m.setStatus(false, "body unchanged")
		return
	}
	m.edit(func(cur *collection.File) error { return cur.Set(body, cur.BodyKeys()...) })
}

// reloaded reselects a file edited whole and reports whether it still parses.
func (m *model) reloaded(msg editorMsg) {
	e, ok := m.reselect(msg.path)
	switch {
	case !ok:
	case e.err != nil:
		m.setStatus(true, "%v", e.err)
	case msg.err == nil:
		m.setStatus(false, "reloaded %s", m.rel(msg.path))
	}
}

// --- calls -------------------------------------------------------------------------

func (m *model) send() tea.Cmd {
	if !m.haveRequest() {
		return nil
	}
	req, err := runner.Prepare(m.cur, m.env)
	if err != nil {
		m.setStatus(true, "%v", err)
		return nil
	}
	eng, err := core.EngineFor(req.Type)
	if err != nil {
		m.setStatus(true, "%v", err)
		return nil
	}
	if m.call != nil {
		m.call.cancel()
	}
	m.callSeq++
	id := m.callSeq
	m.call = nil
	m.stream.reset()
	m.setStatus(false, "sending %s…", m.cur.Req.Name)
	return func() tea.Msg { return start(id, req, eng) }
}

// start opens a call. It runs as a command, off the UI goroutine.
func start(id int, req core.Request, eng core.Engine) tea.Msg {
	req, err := withMethodType(req)
	if err != nil {
		return startedMsg{id: id, err: err}
	}
	interactive := isInteractive(req)
	ctx, cancel := context.WithCancel(context.Background())
	out := req.Outgoing()
	send := make(chan []byte, len(out)+16)
	for _, b := range out {
		send <- []byte(b)
	}
	if !interactive {
		close(send)
	}
	evs, err := eng.Run(ctx, req, send)
	if err != nil {
		cancel()
		return startedMsg{id: id, err: err}
	}
	c := &call{id: id, req: req, cancel: cancel, events: evs, interactive: interactive, start: time.Now()}
	if interactive {
		c.send = send
	}
	return startedMsg{id: id, c: c}
}

// withMethodType asks the server or proto for a gRPC method's call type when
// the file doesn't say.
func withMethodType(req core.Request) (core.Request, error) {
	if req.Type != "grpc" || req.MethodType != "" {
		return req, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ms, err := grpcengine.Methods(ctx, req)
	if err != nil {
		return req, err
	}
	for _, mt := range ms {
		if strings.TrimPrefix(req.Method, "/") == mt.FullName() {
			req.MethodType = mt.Type
		}
	}
	return req, nil
}

// isInteractive reports whether the user types messages while the call runs.
func isInteractive(req core.Request) bool {
	switch req.Type {
	case "websocket":
		return true
	case "grpc":
		return req.MethodType == "client-streaming" || req.MethodType == "bidi-streaming"
	}
	return false
}

func (m *model) started(msg startedMsg) tea.Cmd {
	if msg.id != m.callSeq { // superseded by a newer send
		if msg.c != nil {
			msg.c.cancel()
		}
		return nil
	}
	if msg.err != nil {
		m.stream.add(m.env.RedactEvent(core.Event{Time: time.Now(), Kind: core.Error, Err: msg.err.Error()}))
		m.setStatus(true, "%v", msg.err)
		return nil
	}
	m.call = msg.c
	if msg.c.interactive {
		m.focus = focusInput
		m.setStatus(false, "streaming: type messages, enter sends, ctrl+d ends your side, x cancels")
		return tea.Batch(m.input.Focus(), next(msg.c))
	}
	m.setStatus(false, "running…")
	return next(msg.c)
}

// next waits for the call's next events, batching whatever else is ready so a
// fast stream re-renders once per batch, not once per message.
func next(c *call) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-c.events
		if !ok {
			return eventsMsg{id: c.id, done: true}
		}
		evs := []core.Event{ev}
		for len(evs) < 512 {
			select {
			case ev, ok := <-c.events:
				if !ok {
					return eventsMsg{id: c.id, evs: evs, done: true}
				}
				evs = append(evs, ev)
			default:
				return eventsMsg{id: c.id, evs: evs}
			}
		}
		return eventsMsg{id: c.id, evs: evs}
	}
}

func (m *model) events(msg eventsMsg) tea.Cmd {
	c := m.call
	if c == nil || msg.id != c.id {
		return nil
	}
	for _, ev := range msg.evs {
		c.resp.Add(ev)
		m.stream.add(m.env.RedactEvent(ev)) // the only path into the display
	}
	if !msg.done {
		return next(c)
	}
	m.finish(c)
	return nil
}

// finish runs assertions and captures once the call is over.
func (m *model) finish(c *call) {
	c.cancel()
	c.send, c.done = nil, true
	if m.focus == focusInput {
		m.input.Blur()
		m.focus = focusStream
	}
	took := time.Since(c.start).Round(time.Millisecond)
	addNote := func(format string, a ...any) {
		m.stream.add(m.env.RedactEvent(core.Event{Time: time.Now(), Kind: note, Text: fmt.Sprintf(format, a...)}))
	}
	failed := 0
	for _, a := range c.req.Assertions {
		msg, err := assert.Check(a, &c.resp)
		switch {
		case err != nil:
			addNote("assertion error: %v", err)
			failed++
		case msg != "":
			addNote("FAIL %s", msg)
			failed++
		default:
			addNote("pass %s %s %s", a.Expression, a.Operator, a.Value)
		}
	}
	for _, cp := range c.req.Captures {
		if v, ok, _ := assert.Value(cp.Expression, &c.resp); ok {
			m.env.Capture(cp.Name, v)
			addNote("captured %s", cp.Name)
		} else {
			addNote("capture %s: no value at %s", cp.Name, cp.Expression)
		}
	}
	if len(c.req.Captures) > 0 { // mask captured values in the response that carried them
		for i := range m.stream.items {
			m.stream.items[i].ev = m.env.RedactEvent(m.stream.items[i].ev)
		}
	}
	m.setStatus(failed > 0, "done in %s: %d messages, status %s, %d of %d assertions failed",
		took, len(c.resp.Messages), or(c.resp.StatusText, "-"), failed, len(c.req.Assertions))
}

func (m *model) halfClose() {
	if m.call != nil && m.call.send != nil {
		close(m.call.send)
		m.call.send = nil
		m.setStatus(false, "your side of the stream is closed; waiting for the server")
	}
	m.input.Blur()
	m.focus = focusStream
}
