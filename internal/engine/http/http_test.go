package httpengine

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abdussalamfaqih/streamx/internal/core"
	"github.com/abdussalamfaqih/streamx/internal/testserver"
)

func run(t *testing.T, req core.Request) (recv []string, st core.Event, all []core.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := Engine{}.Run(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	for e := range ch {
		all = append(all, e)
		switch e.Kind {
		case core.Received:
			recv = append(recv, string(e.Data))
		case core.Status:
			st = e
		}
	}
	return
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(testserver.HTTP())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	recv, st, all := run(t, core.Request{Method: "post", URL: srv.URL + "/echo", Body: `{"a": 1}`,
		Headers: []core.KV{{Name: "Authorization", Value: "Bearer x"}}})
	if st.Code != 200 || st.Text != "OK" || len(recv) != 1 {
		t.Fatalf("status %+v recv %v", st, recv)
	}
	for _, want := range []string{`"method":"POST"`, `"authorization":"Bearer x"`, `"body":"{\"a\": 1}"`} {
		if !strings.Contains(recv[0], want) {
			t.Errorf("echo lacks %s: %s", want, recv[0])
		}
	}
	if all[0].Kind != core.Sent || string(all[0].Data) != `{"a":1}` || all[1].Kind != core.Headers {
		t.Errorf("event order: %+v", all[:2])
	}

	recv, st, _ = run(t, core.Request{URL: host + "/text"}) // no scheme defaults to http, no method to GET
	if st.Code != 200 || recv[0] != `"hello"` {
		t.Errorf("text body should become a JSON string: %v", recv)
	}
	if _, st, _ = run(t, core.Request{URL: srv.URL + "/status/404"}); st.Code != 404 || st.Text != "Not Found" {
		t.Errorf("status = %+v", st)
	}
	if _, _, all = run(t, core.Request{URL: "http://127.0.0.1:1/"}); all[len(all)-1].Kind != core.Error {
		t.Errorf("unreachable server should end with an error event: %+v", all)
	}
}

func TestSSE(t *testing.T) {
	srv := httptest.NewServer(testserver.HTTP())
	defer srv.Close()
	recv, st, all := run(t, core.Request{URL: srv.URL + "/sse?count=3"})
	if st.Code != 200 || strings.Join(recv, " ") != `{"n":0} {"n":1} {"n":2}` {
		t.Fatalf("status %+v recv %v", st, recv)
	}
	for _, e := range all {
		if e.Kind == core.Received && (e.Meta["event"] != "tick" || e.Meta["id"] == "") {
			t.Errorf("meta = %v", e.Meta)
		}
	}
}

func TestReadSSE(t *testing.T) {
	in := "data: line1\r\ndata: line2\r\n\r\n: comment\n\nevent: x\ndata:{\"k\":true}\n\nid: 7\ndata: unterminated"
	var got []core.Event
	readSSE(strings.NewReader(in), func(e core.Event) bool { got = append(got, e); return true })
	if len(got) != 2 || string(got[0].Data) != `"line1\nline2"` || string(got[1].Data) != `{"k":true}` || got[1].Meta["event"] != "x" {
		t.Errorf("got %+v", got)
	}
}
