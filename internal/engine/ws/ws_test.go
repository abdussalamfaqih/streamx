package wsengine

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abdussalamfaqih/streamx/internal/core"
	"github.com/abdussalamfaqih/streamx/internal/testserver"
)

func TestWebSocket(t *testing.T) {
	srv := httptest.NewServer(testserver.HTTP())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	send := make(chan []byte, 4)
	ch, err := Engine{}.Run(ctx, core.Request{URL: url}, send)
	if err != nil {
		t.Fatal(err)
	}
	send <- []byte(`{"n": 1}`)
	send <- []byte(`plain text`)
	close(send) // must not close the socket
	var recv []string
	for e := range ch {
		if e.Kind == core.Received {
			recv = append(recv, string(e.Data))
			if len(recv) == 2 {
				break
			}
		}
	}
	if strings.Join(recv, " ") != `{"n":2} "plain text"` {
		t.Errorf("recv = %v", recv)
	}

	// The server closes normally on "bye": status 1000 with no error.
	send2 := make(chan []byte, 1)
	send2 <- []byte("bye")
	ch, err = Engine{}.Run(ctx, core.Request{URL: url}, send2)
	if err != nil {
		t.Fatal(err)
	}
	var st core.Event
	for e := range ch {
		if e.Kind == core.Status {
			st = e
		}
	}
	if st.Code != 1000 || st.Err != "" {
		t.Errorf("close status = %+v", st)
	}

	// Cancelling ends the call cleanly.
	cctx, ccancel := context.WithCancel(ctx)
	ch, err = Engine{}.Run(cctx, core.Request{URL: url}, make(chan []byte))
	if err != nil {
		t.Fatal(err)
	}
	ccancel()
	for range ch {
	}

	if _, err := (Engine{}).Run(ctx, core.Request{URL: srv.URL + "/text"}, nil); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Errorf("non-websocket endpoint err = %v", err)
	}
}
