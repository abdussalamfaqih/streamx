// Package runner runs request files headless for CI: expand, call, assert,
// capture, and turn the results into an exit code.
package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abdussalamfaqih/streamx/internal/assert"
	"github.com/abdussalamfaqih/streamx/internal/collection"
	"github.com/abdussalamfaqih/streamx/internal/core"
	"github.com/abdussalamfaqih/streamx/internal/env"
)

// Exit codes.
const (
	ExitOK     = 0
	ExitFail   = 1 // a request or assertion failed
	ExitConfig = 2 // usage, config or parse error
)

const DefaultTimeout = 30 * time.Second

type Options struct {
	Env     string
	Vars    map[string]string // --var overrides, not secret
	Verbose bool              // print every event
	Out     io.Writer
	// Transcript, when set, receives every event of every request as redacted JSON lines.
	Transcript io.Writer
}

// Run runs every request matched by args (files, globs or directories) in
// order: directory, then info.seq, then file name.
func Run(ctx context.Context, args []string, o Options) int {
	files, ok := loadAll(args, o.Out)
	if !ok {
		return ExitConfig // parse errors stop the run before any request is sent
	}
	s := &session{o: o, envs: map[string]*env.Env{}, lost: map[string]string{}}
	code := ExitOK
	for _, f := range files {
		e, err := s.env(f.Root)
		if err != nil {
			fmt.Fprintln(o.Out, "ERROR", err)
			return ExitConfig
		}
		code = max(code, s.report(f, e, s.run(ctx, f, e)))
	}
	fmt.Fprintf(o.Out, "\n%d passed, %d failed\n", s.passed, s.failed)
	return code
}

// loadAll expands args and parses every file, printing each error.
func loadAll(args []string, w io.Writer) ([]*collection.File, bool) {
	paths, err := Expand(args)
	if err != nil {
		fmt.Fprintln(w, err)
		return nil, false
	}
	var files []*collection.File
	ok := true
	for _, p := range paths {
		f, err := collection.Load(p)
		if err != nil {
			fmt.Fprintln(w, "ERROR", err)
			ok = false
			continue
		}
		files = append(files, f)
	}
	Sort(files)
	return files, ok
}

// session is the state of one Run across its requests.
type session struct {
	o              Options
	envs           map[string]*env.Env // per collection root; captures live here
	lost           map[string]string   // capture name -> request that failed to set it
	passed, failed int
}

func (s *session) env(root string) (*env.Env, error) {
	if e, ok := s.envs[root]; ok {
		return e, nil
	}
	e, err := env.Load(root, s.o.Env)
	if err != nil {
		return nil, err
	}
	for k, v := range s.o.Vars {
		e.Set(k, v, false)
	}
	s.envs[root] = e
	return e, nil
}

// run runs f and tracks the captures it failed to set.
func (s *session) run(ctx context.Context, f *collection.File, e *env.Env) result {
	r := runOne(ctx, f, e, s.o)
	var undef *env.UndefinedError
	if errors.As(r.configErr, &undef) && s.lost[undef.Name] != "" {
		// Not a config problem: an earlier request that captures this variable failed.
		r = result{failures: []string{fmt.Sprintf("skipped: needs %q, which %q failed to capture", undef.Name, s.lost[undef.Name])}}
	}
	for _, c := range f.Req.Captures {
		if r.failed() {
			s.lost[c.Name] = f.Req.Name
		} else {
			delete(s.lost, c.Name)
		}
	}
	return r
}

// report prints the outcome of one request and returns its exit code.
func (s *session) report(f *collection.File, e *env.Env, r result) int {
	out := func(format string, a ...any) { fmt.Fprint(s.o.Out, e.Redact(fmt.Sprintf(format, a...))) }
	for _, w := range f.Warnings {
		out("WARN %s: %s\n", f.Path, w)
	}
	took := r.took.Round(time.Millisecond)
	switch {
	case r.configErr != nil:
		out("ERROR %s: %v\n", f.Req.Name, r.configErr)
		s.failed++
		return ExitConfig
	case len(r.failures) > 0:
		out("FAIL %s (%s)\n", f.Req.Name, took)
		for _, msg := range r.failures {
			out("    %s\n", msg)
		}
		s.failed++
		return ExitFail
	}
	out("PASS %s (%s)\n", f.Req.Name, took)
	s.passed++
	return ExitOK
}

// Sort orders files by directory, then info.seq, then path.
func Sort(files []*collection.File) {
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if da, db := filepath.Dir(a.Path), filepath.Dir(b.Path); da != db {
			return da < db
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.Path < b.Path
	})
}

// Prepare turns a file's request into what gets sent: placeholders expanded,
// client cert applied, paths made absolute.
func Prepare(f *collection.File, e *env.Env) (core.Request, error) {
	req, err := e.ExpandRequest(f.Req)
	if err != nil {
		return req, err
	}
	root, err := collection.LoadRoot(f.Root)
	if err != nil {
		return req, err
	}
	if req, err = collection.ApplyCert(req, append(e.Certs, root.Certs...)); err != nil {
		return req, err
	}
	for _, p := range []*string{&req.TLS.CertFile, &req.TLS.KeyFile} { // cert paths come from config, after expansion
		if *p, err = e.Expand(*p); err != nil {
			return req, err
		}
	}
	return collection.ResolvePaths(req, f.Root), nil
}

type result struct {
	took      time.Duration
	failures  []string
	configErr error
}

func (r result) failed() bool { return r.configErr != nil || len(r.failures) > 0 }

func (o Options) logging() bool { return o.Verbose || o.Transcript != nil }

func runOne(ctx context.Context, f *collection.File, e *env.Env, o Options) (r result) {
	start := time.Now()
	defer func() { r.took = time.Since(start) }()

	req, err := Prepare(f, e)
	if err != nil {
		return result{configErr: err}
	}
	eng, err := core.EngineFor(req.Type)
	if err != nil {
		return result{configErr: err}
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(req.Timeout, DefaultTimeout))
	defer cancel()
	events, err := eng.Run(ctx, req, queued(req.Outgoing()))
	if err != nil {
		return result{failures: []string{err.Error()}}
	}
	c := collect(ctx, cancel, req, events, o.logging())
	// Events are written only after captures ran: a captured token must be
	// masked in the very response that carried it.
	defer func() {
		if err := o.writeLog(e, f.Req.Name, c.log); err != nil && r.configErr == nil {
			r = result{configErr: fmt.Errorf("transcript: %w", err)}
		}
	}()
	if err := c.judge(req); err != nil {
		return result{configErr: err}
	}
	if err := c.capture(req, e); err != nil {
		return result{configErr: err}
	}
	return result{failures: c.failures}
}

// queued returns a closed channel holding msgs: the runner sends everything, then half-closes.
func queued(msgs []string) <-chan []byte {
	send := make(chan []byte, len(msgs))
	for _, m := range msgs {
		send <- []byte(m)
	}
	close(send)
	return send
}

// outcome is what one call produced, before assertions and captures.
type outcome struct {
	resp      assert.Response
	gotStatus bool
	statusErr string // a failing status; assertions, when present, decide instead
	failures  []string
	log       []core.Event
}

func collect(ctx context.Context, cancel context.CancelFunc, req core.Request, events <-chan core.Event, keepLog bool) *outcome {
	c := &outcome{}
	stopped := false // the runner ended the stream on purpose (maxMessages or stream timeout)
	for ev := range events {
		if keepLog {
			c.log = append(c.log, ev)
		}
		if stopped && ev.Kind == core.Received {
			continue
		}
		if c.record(ev, req.MaxMessages) {
			stopped = true
			cancel()
		}
	}
	c.end(stopped || req.StreamStop && errors.Is(ctx.Err(), context.DeadlineExceeded))
	return c
}

// record adds one event and reports whether maxMessages is reached.
func (c *outcome) record(ev core.Event, maxMessages int) bool {
	c.resp.Add(ev)
	switch ev.Kind {
	case core.Received:
		return maxMessages > 0 && len(c.resp.Messages) >= maxMessages
	case core.Status:
		c.gotStatus = true
		c.statusErr = statusError(ev)
	case core.Error:
		c.failures = append(c.failures, ev.Err)
	}
	return false
}

func statusError(ev core.Event) string {
	if ev.Err == "" {
		return ""
	}
	s := fmt.Sprintf("status %s: %s", ev.Text, ev.Err)
	if len(ev.Data) > 0 {
		s += " " + string(ev.Data) // error details
	}
	return s
}

// end closes the outcome; a deliberate stop counts as a clean end of stream.
func (c *outcome) end(stopped bool) {
	if stopped {
		c.resp.Status, c.resp.StatusText, c.gotStatus, c.statusErr = 0, "OK", true, ""
	}
	if !c.gotStatus && len(c.failures) == 0 {
		c.failures = append(c.failures, "call ended without a status")
	}
}

// judge runs the assertions, or without any, checks the status.
// With assertions, they decide about the status (you may expect NotFound).
func (c *outcome) judge(req core.Request) error {
	if len(req.Assertions) == 0 {
		if msg := c.statusFailure(req.Type); msg != "" {
			c.failures = append(c.failures, msg)
		}
		return nil
	}
	for _, a := range req.Assertions {
		msg, err := assert.Check(a, &c.resp)
		if err != nil {
			return err
		}
		if msg != "" {
			c.failures = append(c.failures, msg)
		}
	}
	return nil
}

func (c *outcome) statusFailure(typ string) string {
	switch {
	case c.statusErr != "":
		return c.statusErr
	case c.gotStatus && !defaultOK(typ, c.resp.Status):
		return fmt.Sprintf("status %d %s", c.resp.Status, c.resp.StatusText)
	}
	return ""
}

func (c *outcome) capture(req core.Request, e *env.Env) error {
	for _, cp := range req.Captures {
		v, ok, err := assert.Value(cp.Expression, &c.resp)
		if err != nil {
			return fmt.Errorf("capture %s: %w", cp.Name, err)
		}
		if !ok {
			c.failures = append(c.failures, fmt.Sprintf("capture %s: no value at %s", cp.Name, cp.Expression))
			continue
		}
		e.Capture(cp.Name, v)
	}
	return nil
}

// writeLog prints (-v) and records (--transcript) the redacted events.
func (o Options) writeLog(e *env.Env, name string, log []core.Event) error {
	for _, ev := range log {
		red := e.RedactEvent(ev)
		if o.Verbose {
			printEvent(o.Out, e, red)
		}
		if o.Transcript == nil {
			continue
		}
		if err := core.WriteTranscript(o.Transcript, name, red); err != nil {
			return err
		}
	}
	return nil
}

func defaultOK(typ string, status int) bool {
	switch typ {
	case "grpc":
		return status == 0
	case "http":
		return status < 400
	case "websocket": // 0: we closed it; 1000/1001: normal server close
		return status == 0 || status == 1000 || status == 1001
	}
	return true
}

func printEvent(w io.Writer, e *env.Env, ev core.Event) {
	line := fmt.Sprintf("    %s %-8s", ev.Time.Format("15:04:05.000"), ev.Kind)
	switch {
	case len(ev.Data) > 0:
		line += " " + string(ev.Data)
	case len(ev.Meta) > 0:
		line += " " + core.MetaText(ev.Meta)
	case ev.Kind == core.Status:
		line += fmt.Sprintf(" %d %s %s", ev.Code, ev.Text, ev.Err)
	default:
		line += " " + ev.Err
	}
	fmt.Fprintln(w, e.Redact(line)) // RedactEvent already ran; this catches the formatted line too
}

// Expand turns args into request file paths. Directories are walked; globs
// are expanded; duplicates are dropped.
func Expand(args []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, a := range args {
		if err := expandArg(a, add); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no request files in %s", strings.Join(args, " "))
	}
	return out, nil
}

func expandArg(a string, add func(string)) error {
	matches, err := filepath.Glob(a)
	if err != nil {
		return fmt.Errorf("bad pattern %q: %w", a, err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no files match %q", a)
	}
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			return err
		}
		if st.IsDir() {
			if err := walkRequests(m, add); err != nil {
				return err
			}
		} else if collection.IsRequestFile(m) || len(matches) == 1 { // a glob skips root/env files; a named file is taken as asked
			add(m)
		}
	}
	return nil
}

// walkRequests adds every request file under dir, skipping hidden directories.
func walkRequests(dir string, add func(string)) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") && p != dir {
			return filepath.SkipDir
		}
		if !d.IsDir() && collection.IsRequestFile(p) {
			add(p)
		}
		return nil
	})
}
