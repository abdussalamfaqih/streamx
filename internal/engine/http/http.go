// Package httpengine runs plain HTTP requests. A response with
// Content-Type text/event-stream is read as Server-Sent Events: one Received
// event per SSE message, as it arrives.
package httpengine

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

// maxBody caps a non-streaming response body held in memory.
const maxBody = 32 << 20

type Engine struct{}

func init() { core.Register("http", Engine{}) }

// Run sends req. The send channel is not used: an HTTP body is req.Body.
func (Engine) Run(ctx context.Context, req core.Request, _ <-chan []byte) (<-chan core.Event, error) {
	client, err := req.TLS.HTTPClient()
	if err != nil {
		return nil, err
	}
	hreq, err := newRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan core.Event, 64)
	emit := core.Emitter(ctx, out)
	go func() {
		defer close(out)
		if req.Body != "" {
			emit(core.Event{Kind: core.Sent, Data: core.JSON([]byte(req.Body))})
		}
		resp, err := client.Do(hreq)
		if err != nil {
			emit(core.Event{Kind: core.Error, Err: err.Error()})
			return
		}
		defer resp.Body.Close()
		emit(core.Event{Kind: core.Headers, Meta: core.FlattenHeader(resp.Header)})
		if err := readBody(resp, emit); err != nil && ctx.Err() == nil {
			emit(core.Event{Kind: core.Error, Err: "reading response: " + err.Error()})
		}
		emit(core.Event{Kind: core.Status, Code: resp.StatusCode, Text: http.StatusText(resp.StatusCode)})
	}()
	return out, nil
}

// newRequest builds the request; no scheme means http://, no method means GET.
func newRequest(ctx context.Context, req core.Request) (*http.Request, error) {
	u := req.URL
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, cmp.Or(strings.ToUpper(req.Method), http.MethodGet), u, body)
	if err != nil {
		return nil, err
	}
	for _, h := range req.Headers {
		hreq.Header.Add(h.Name, h.Value)
	}
	return hreq, nil
}

// readBody emits an event stream message by message, any other body whole.
func readBody(resp *http.Response, emit func(core.Event) bool) error {
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/event-stream" {
		return readSSE(resp.Body, emit)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if len(b) > maxBody {
		return fmt.Errorf("response body is larger than %d MB", maxBody>>20)
	}
	if len(b) > 0 {
		emit(core.Event{Kind: core.Received, Data: core.JSON(b)})
	}
	return err
}

// readSSE parses an event stream (WHATWG HTML §9.2) and emits each message.
// Meta carries the event name and id when the server sets them.
func readSSE(r io.Reader, emit func(core.Event) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var m sseMessage
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line != "" {
			m.add(line)
			continue
		}
		if len(m.data) > 0 && !emit(m.event()) { // a blank line dispatches the message
			return nil
		}
		m = sseMessage{}
	}
	return sc.Err() // a message without its closing blank line is dropped, per spec
}

type sseMessage struct {
	data     []string
	name, id string
}

func (m *sseMessage) add(line string) {
	if strings.HasPrefix(line, ":") {
		return // comment / keep-alive
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "data":
		m.data = append(m.data, value)
	case "event":
		m.name = value
	case "id":
		m.id = value
	}
}

func (m *sseMessage) event() core.Event {
	meta := map[string]string{}
	if m.name != "" {
		meta["event"] = m.name
	}
	if m.id != "" {
		meta["id"] = m.id
	}
	return core.Event{Kind: core.Received, Data: core.JSON([]byte(strings.Join(m.data, "\n"))), Meta: meta}
}
