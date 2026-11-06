// Package core holds the protocol-neutral request and event types shared by
// every engine, the headless runner and the TUI.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type KV struct{ Name, Value string }

type TLS struct {
	CAFile   string
	Insecure bool
	CertFile string // client cert for mTLS
	KeyFile  string
}

type Assertion struct{ Expression, Operator, Value string }

// Capture copies a response value into a runtime variable (OC set-variable action).
type Capture struct{ Name, Expression string }

// Request is an interpolation-ready request. Every string field may contain {{var}}.
type Request struct {
	Name        string
	Type        string // grpc | websocket | http
	URL         string
	Method      string // gRPC "pkg.Service/Method"; HTTP verb
	MethodType  string // unary | client-streaming | server-streaming | bidi-streaming ("" = from descriptor)
	ProtoFile   string // "" = server reflection
	ImportPaths []string
	Headers     []KV // gRPC metadata or HTTP/WS headers
	Body        string
	Messages    []string // messages to send in order; overrides Body when set
	TLS         TLS
	MaxMessages int           // runner stops after this many received messages (0 = no limit)
	Timeout     time.Duration // runner stop; 0 = default timeout
	StreamStop  bool          // Timeout came from stream settings: reaching it is a normal stop, not a failure
	Assertions  []Assertion
	Captures    []Capture
}

// Outgoing returns the messages to send, in order.
func (r Request) Outgoing() []string {
	if len(r.Messages) > 0 {
		return r.Messages
	}
	if r.Body != "" {
		return []string{r.Body}
	}
	return nil
}

type EventKind string

const (
	Sent     EventKind = "sent"
	Received EventKind = "received"
	Headers  EventKind = "headers"
	Trailers EventKind = "trailers"
	Status   EventKind = "status" // final status; Code set
	Error    EventKind = "error"  // transport/encoding error; Err set
)

type Event struct {
	Time time.Time         `json:"time"`
	Kind EventKind         `json:"kind"`
	Data json.RawMessage   `json:"data,omitempty"`
	Meta map[string]string `json:"meta,omitempty"`
	Code int               `json:"code,omitempty"` // gRPC code or HTTP status
	Text string            `json:"text,omitempty"` // status text, e.g. "OK" / "NotFound"
	Err  string            `json:"err,omitempty"`
}

// Engine runs one request. Messages arrive on send (closing it half-closes the
// stream). The returned channel is closed when the call is over.
type Engine interface {
	Run(ctx context.Context, req Request, send <-chan []byte) (<-chan Event, error)
}

// Emitter returns the send function engines use: it stamps the time and
// drops the event once ctx is done, so a caller that gave up never blocks the
// engine. It reports whether the event was delivered.
func Emitter(ctx context.Context, out chan<- Event) func(Event) bool {
	return func(e Event) bool {
		e.Time = time.Now()
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

var engines = map[string]Engine{}

func Register(typ string, e Engine) { engines[typ] = e }

func EngineFor(typ string) (Engine, error) {
	if e, ok := engines[typ]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("request type %q is not supported", typ)
}

// MetaText formats metadata as sorted "key=value" pairs.
func MetaText(meta map[string]string) string {
	kv := make([]string, 0, len(meta))
	for k, v := range meta {
		kv = append(kv, k+"="+v)
	}
	sort.Strings(kv)
	return strings.Join(kv, " ")
}
