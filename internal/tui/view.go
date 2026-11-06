package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const summaryLines = 6

var (
	selSty    = lipgloss.NewStyle().Reverse(true)
	accentSty = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	focusClr  = lipgloss.Color("12")
	blurClr   = lipgloss.Color("8")
)

func (m *model) sideWidth() int   { return min(44, max(24, m.width/3)) }
func (m *model) streamWidth() int { return max(10, m.width-m.sideWidth()-2) }

func (m *model) inputVisible() bool { return m.call != nil && m.call.send != nil }

// streamHeight is the number of log lines in the stream box.
func (m *model) streamHeight() int {
	h := m.height - 1 - (summaryLines + 2) - 2 - 1 // status line, summary box, stream borders, stream title
	if m.inputVisible() {
		h--
	}
	return max(1, h)
}

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	if m.width == 0 {
		v.Content = "loading…"
		return v
	}
	side := box(m.sidebar(m.height-3), m.sideWidth(), m.height-1, m.focus == focusSidebar || m.focus == focusPick)
	right := lipgloss.JoinVertical(lipgloss.Left,
		box(m.summary(), m.width-m.sideWidth(), summaryLines+2, false),
		box(m.streamPane(), m.width-m.sideWidth(), m.height-1-(summaryLines+2), m.focus == focusStream || m.focus == focusInput || m.focus == focusFilter),
	)
	v.Content = lipgloss.JoinVertical(lipgloss.Left, lipgloss.JoinHorizontal(lipgloss.Top, side, right), m.bottom())
	return v
}

// box draws a rounded border around lines, fitted to exactly w×h cells (border included).
func box(lines []string, w, h int, focused bool) string {
	iw, ih := max(1, w-2), max(1, h-2)
	out := make([]string, ih)
	for i := range out {
		l := ""
		if i < len(lines) {
			l = ansi.Truncate(lines[i], iw, "…")
		}
		out[i] = l + strings.Repeat(" ", max(0, iw-lipgloss.Width(l)))
	}
	clr := blurClr
	if focused {
		clr = focusClr
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clr).Render(strings.Join(out, "\n"))
}

// row is one sidebar line: an entry, or a service header (idx -1).
type row struct {
	text string
	idx  int
}

func (m *model) sidebar(h int) []string {
	lines := m.sidebarHeader()
	rows, sel := m.rows()
	selRow := 0
	for r, rw := range rows {
		if rw.idx == sel {
			selRow = r
		}
	}
	avail := max(1, h-len(lines))
	start := max(0, selRow-avail+1)
	for _, rw := range rows[start:min(len(rows), start+avail)] {
		t := rw.text
		if rw.idx == sel && rw.idx >= 0 {
			t = selSty.Render(ansi.Strip(t))
		}
		lines = append(lines, t)
	}
	return lines
}

// sidebarHeader is the tab bar and the line under it.
func (m *model) sidebarHeader() []string {
	tabs := []string{"Collection", "Services"}
	for i := range tabs {
		if tab(i) == m.tab {
			tabs[i] = accentSty.Render(tabs[i])
		} else {
			tabs[i] = dim.Render(tabs[i])
		}
	}
	sub := m.svcMsg
	if m.tab == tabCollection {
		sub = "env: " + or(m.envName, "none")
	}
	return []string{strings.Join(tabs, dim.Render(" │ ")), dim.Render(sub)}
}

// rows lists the sidebar rows and the selected entry index (-1 for none).
func (m *model) rows() ([]row, int) {
	if m.focus == focusPick {
		return m.pickRows()
	}
	var rows []row
	prevSvc := ""
	for i, e := range m.lists[m.tab] {
		if e.method != nil && e.method.Service != prevSvc {
			prevSvc = e.method.Service
			rows = append(rows, row{boldSty.Render(prevSvc), -1})
		}
		rows = append(rows, row{m.label(i, true), i})
	}
	if len(rows) == 0 && m.tab == tabCollection {
		rows = append(rows, row{dim.Render("no request files: tab → Services to make one"), -1})
	}
	return rows, m.cursor[m.tab]
}

// pickRows lists the fuzzy finder's matches.
func (m *model) pickRows() ([]row, int) {
	var rows []row
	for _, i := range m.picks {
		rows = append(rows, row{m.label(i, false), i})
	}
	if len(m.picks) == 0 {
		return rows, -1
	}
	return rows, m.picks[m.pickSel]
}

func (m *model) label(i int, inTree bool) string {
	e := m.lists[m.tab][i]
	switch {
	case e.method != nil:
		name := e.method.Name
		if !inTree {
			name = e.label
		} else {
			name = "  " + name
		}
		return name + " " + dim.Render(strings.TrimSuffix(e.method.Type, "-streaming"))
	case e.err != nil:
		return errSty.Render("✗ " + e.label)
	}
	mark := "  "
	if e.file == m.cur {
		mark = accentSty.Render("● ")
	}
	return mark + e.label + " " + dim.Render(m.rel(e.file.Path))
}

func (m *model) summary() []string {
	f := m.cur
	if f == nil || f.Root == "" {
		return []string{dim.Render("no request selected")}
	}
	r := f.Req
	kind := r.Type
	if r.MethodType != "" {
		kind += " · " + r.MethodType
	}
	title := boldSty.Render(r.Name) + "  " + dim.Render(kind)
	if m.dirty {
		title += "  " + errSty.Render("[modified]")
	}
	lines := []string{title, r.URL + "  " + r.Method}
	if r.Type == "grpc" {
		lines = append(lines, "proto: "+or(r.ProtoFile, "server reflection"))
	}
	var hs []string
	for _, h := range r.Headers {
		hs = append(hs, h.Name+"="+h.Value)
	}
	lines = append(lines, "meta:  "+dim.Render(or(strings.Join(hs, ", "), "none")))
	body := strings.Join(strings.Fields(r.Body), " ")
	if n := len(r.Messages); n > 0 {
		body = fmt.Sprintf("%d messages to send", n)
	}
	lines = append(lines, "body:  "+or(body, dim.Render("empty")))
	path := "(unsaved: w to save)"
	if f.Path != "" {
		path = m.rel(f.Path)
	}
	return append(lines, dim.Render(path))
}

func (m *model) streamPane() []string {
	s := &m.stream
	parts := []string{boldSty.Render("Stream"), m.streamState()}
	if s.filter != "" {
		parts = append(parts, metaSty.Render("filter: "+s.filter))
	}
	if s.compact {
		parts = append(parts, dim.Render("compact"))
	}
	if s.offset > 0 {
		parts = append(parts, dim.Render(fmt.Sprintf("↑%d (G follows)", s.offset)))
	}
	if s.dropped > 0 {
		parts = append(parts, dim.Render(fmt.Sprintf("%d old dropped", s.dropped)))
	}
	lines := append([]string{strings.Join(parts, "  ")}, strings.Split(s.view(m.streamWidth(), m.streamHeight()), "\n")...)
	if m.inputVisible() {
		lines = append(lines, m.input.View())
	}
	return lines
}

func (m *model) streamState() string {
	switch {
	case m.stream.paused:
		return errSty.Render(fmt.Sprintf("❚❚ paused (%d new)", m.stream.pending()))
	case m.call == nil || m.call.done:
		return dim.Render("idle")
	case !m.call.interactive:
		return recvSty.Render("● receiving")
	}
	return recvSty.Render("● live")
}

func (m *model) bottom() string {
	var s string
	switch m.focus {
	case focusCmd:
		s = ":" + m.line.View()
	case focusPick:
		s = "/" + m.line.View() + dim.Render(fmt.Sprintf("  %d matches · ↑↓ choose · enter open · esc cancel", len(m.picks)))
	case focusFilter:
		s = "filter: " + m.line.View() + dim.Render("  enter keeps · esc clears")
	default:
		s = m.status
		if m.isErr {
			s = errSty.Render(s)
		}
	}
	return ansi.Truncate(s, m.width, "…")
}
