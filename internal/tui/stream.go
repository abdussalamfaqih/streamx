package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

// maxEvents bounds memory for firehose streams; older events are dropped.
const maxEvents = 10000

// note is a TUI-only event kind for results and hints shown in the log.
const note core.EventKind = "note"

type item struct {
	seq int
	ev  core.Event // already redacted
}

// stream is the live message log for the current call.
type stream struct {
	items    []item
	seq      int
	dropped  int
	paused   bool
	pauseSeq int // last seq shown while paused
	filter   string
	compact  bool // one line per message instead of pretty-printed JSON
	offset   int  // lines scrolled up from the bottom; 0 follows new output
}

func (s *stream) reset() {
	*s = stream{filter: s.filter, compact: s.compact}
}

func (s *stream) add(ev core.Event) {
	s.seq++
	s.items = append(s.items, item{s.seq, ev})
	if len(s.items) > maxEvents {
		s.dropped += len(s.items) - maxEvents
		s.items = s.items[len(s.items)-maxEvents:]
	}
}

func (s *stream) togglePause() {
	s.paused = !s.paused
	s.pauseSeq = s.seq
}

// pending is how many events arrived since the pause.
func (s *stream) pending() int {
	if !s.paused {
		return 0
	}
	return s.seq - s.pauseSeq
}

var (
	dim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sentSty  = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	recvSty  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errSty   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	noteSty  = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	metaSty  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	boldSty  = lipgloss.NewStyle().Bold(true)
	indent13 = strings.Repeat(" ", 13)
)

// text is the searchable plain text of an event.
func text(ev core.Event) string {
	var b strings.Builder
	b.WriteString(string(ev.Kind))
	b.Write(ev.Data)
	for k, v := range ev.Meta {
		b.WriteString(" " + k + "=" + v)
	}
	b.WriteString(" " + ev.Text + " " + ev.Err)
	return strings.ToLower(b.String())
}

// render turns one event into display lines (unstyled width ≤ width).
func (s *stream) render(ev core.Event, width int) []string {
	ts := dim.Render(ev.Time.Format("15:04:05.000"))
	head, body := s.parts(ev)
	lines := make([]string, len(body))
	for i, b := range body {
		prefix := "  " // continuation lines: json.Indent added 13 spaces, plus "  " lines up under the body
		if i == 0 {
			prefix = ts + " " + head + " "
		}
		lines[i] = ansi.Truncate(prefix+b, width, "…")
	}
	return lines
}

// parts returns an event's marker and its body lines.
func (s *stream) parts(ev core.Event) (string, []string) {
	switch ev.Kind {
	case core.Sent:
		return sentSty.Render("→"), s.message(ev.Data)
	case core.Received:
		return recvSty.Render("←"), s.message(ev.Data)
	case core.Headers, core.Trailers:
		return metaSty.Render("#"), []string{string(ev.Kind) + " " + core.MetaText(ev.Meta)}
	case core.Status:
		return statusParts(ev)
	case core.Error:
		return errSty.Render("!"), []string{errSty.Render(ev.Err)}
	}
	return noteSty.Render("·"), []string{noteSty.Render(ev.Text)}
}

// message pretty-prints JSON unless the view is compact.
func (s *stream) message(data []byte) []string {
	var out bytes.Buffer
	if s.compact || json.Indent(&out, data, indent13, "  ") != nil || !bytes.ContainsAny(out.Bytes(), "\n") {
		return []string{string(data)}
	}
	body := strings.Split(out.String(), "\n")
	body[0] = strings.TrimPrefix(body[0], indent13)
	return body
}

func statusParts(ev core.Event) (string, []string) {
	sty := recvSty
	if ev.Code != 0 {
		sty = errSty
	}
	msg := fmt.Sprintf("status %s (%d)", ev.Text, ev.Code)
	if ev.Err != "" {
		msg += ": " + ev.Err
	}
	body := []string{sty.Render(msg)}
	if len(ev.Data) > 0 { // error details, e.g. google.rpc.BadRequest
		body = append(body, "  "+indent13+"details: "+string(ev.Data))
	}
	return sty.Render("■"), body
}

// view renders the bottom of the log (or offset lines above it) into height lines.
func (s *stream) view(width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := s.tail(width, height+s.offset)
	if s.offset > max(0, len(lines)-height) {
		s.offset = max(0, len(lines)-height) // can't scroll past the top
	}
	window := lines[s.offset:min(len(lines), s.offset+height)]
	out := make([]string, 0, height)
	for i := len(window) - 1; i >= 0; i-- {
		out = append(out, window[i])
	}
	if len(out) == 0 {
		out = append(out, dim.Render("no messages yet: r sends the request"))
	}
	for len(out) < height {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// tail returns at least need visible lines from the end of the log, newest
// first. It walks backwards and stops once it has them, so a 10k buffer stays cheap.
func (s *stream) tail(width, need int) []string {
	q := strings.ToLower(s.filter)
	var lines []string
	for i := len(s.items) - 1; i >= 0 && len(lines) < need; i-- {
		if !s.visible(s.items[i], q) {
			continue
		}
		r := s.render(s.items[i].ev, width)
		for j := len(r) - 1; j >= 0; j-- {
			lines = append(lines, r[j])
		}
	}
	return lines
}

// visible applies pause and filter: paused hides what arrived since the pause.
func (s *stream) visible(it item, q string) bool {
	if s.paused && it.seq > s.pauseSeq {
		return false
	}
	return q == "" || strings.Contains(text(it.ev), q)
}
