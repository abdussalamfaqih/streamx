// Package wsengine runs WebSocket connections: messages from the send channel
// go out as text frames, every received frame becomes a Received event.
// Closing the send channel does not close the socket; the call ends when the
// server closes it or the context is cancelled.
package wsengine

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

const maxMessage = 32 << 20

type Engine struct{}

func init() { core.Register("websocket", Engine{}) }

func (Engine) Run(ctx context.Context, req core.Request, send <-chan []byte) (<-chan core.Event, error) {
	c, resp, err := dial(ctx, req)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithCancel(ctx)
	out := make(chan core.Event, 64)
	emit := core.Emitter(ctx, out)
	emit(core.Event{Kind: core.Headers, Meta: core.FlattenHeader(resp.Header)})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sendAll(callCtx, c, send, emit)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		receive(callCtx, c, emit)
	}()
	go func() {
		wg.Wait()
		c.CloseNow() // shortcut: no close handshake when we stop first; servers see an abrupt close. Add Close(1000) with a short timeout if one complains.
		close(out)
	}()
	return out, nil
}

func dial(ctx context.Context, req core.Request) (*websocket.Conn, *http.Response, error) {
	client, err := req.TLS.HTTPClient()
	if err != nil {
		return nil, nil, err
	}
	h := http.Header{}
	for _, kv := range req.Headers {
		h.Add(kv.Name, kv.Value)
	}
	c, resp, err := websocket.Dial(ctx, req.URL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: h})
	if err != nil && resp != nil {
		return nil, nil, fmt.Errorf("websocket handshake got %s: %w", resp.Status, err)
	}
	if err != nil {
		return nil, nil, err
	}
	c.SetReadLimit(maxMessage)
	return c, resp, nil
}

func sendAll(ctx context.Context, c *websocket.Conn, send <-chan []byte, emit func(core.Event) bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case b, open := <-send:
			if !open {
				send = nil // nothing more to send; keep the socket open for replies
				continue
			}
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				return // the receiver reports why the connection ended
			}
			emit(core.Event{Kind: core.Sent, Data: core.JSON(b)})
		}
	}
}

func receive(ctx context.Context, c *websocket.Conn, emit func(core.Event) bool) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			emit(endEvent(ctx, err))
			return
		}
		ev := core.Event{Kind: core.Received, Data: core.JSON(data)}
		if typ == websocket.MessageBinary {
			ev.Data = core.JSON([]byte(`"` + base64.StdEncoding.EncodeToString(data) + `"`))
			ev.Meta = map[string]string{"type": "binary (base64)"}
		}
		emit(ev)
	}
}

// endEvent turns the error that ended reading into the final event.
func endEvent(ctx context.Context, err error) core.Event {
	var ce websocket.CloseError
	switch {
	case errors.As(err, &ce):
		ev := core.Event{Kind: core.Status, Code: int(ce.Code), Text: ce.Code.String()}
		if ce.Code != websocket.StatusNormalClosure && ce.Code != websocket.StatusGoingAway {
			ev.Err = cmp.Or(ce.Reason, "closed by server")
		}
		return ev
	case ctx.Err() != nil: // also done when the caller's context is
		return core.Event{Kind: core.Status, Text: "closed by client"}
	}
	return core.Event{Kind: core.Error, Err: err.Error()}
}
