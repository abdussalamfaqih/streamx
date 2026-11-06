package grpcengine_test

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/abdussalamfaqih/streamx/internal/core"
	grpcengine "github.com/abdussalamfaqih/streamx/internal/engine/grpc"
	"github.com/abdussalamfaqih/streamx/internal/testserver"
)

var protoDir, _ = filepath.Abs("../../../testdata/protos")

func serve(t *testing.T, opts ...grpc.ServerOption) string {
	t.Helper()
	s, err := testserver.New(protoDir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

// call runs req, sending msgs then half-closing, and returns all events.
func call(t *testing.T, req core.Request) []core.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	send := make(chan []byte, len(req.Outgoing()))
	for _, m := range req.Outgoing() {
		send <- []byte(m)
	}
	close(send)
	ch, err := grpcengine.Engine{}.Run(ctx, req, send)
	if err != nil {
		t.Fatal(err)
	}
	var evs []core.Event
	for e := range ch {
		evs = append(evs, e)
	}
	return evs
}

func summary(evs []core.Event) (received []string, st core.Event) {
	for _, e := range evs {
		switch e.Kind {
		case core.Received:
			received = append(received, string(e.Data))
		case core.Status:
			st = e
		}
	}
	return
}

func TestCallTypes(t *testing.T) {
	addr := serve(t)
	tests := []struct {
		name, method, mtype string
		msgs                []string
		proto               bool
		want                []string
		wantCode            int
	}{
		{"unary reflection", "streamx.test.Echo/Unary", "unary", []string{`{"text":"hi","n":3}`}, false, []string{`{"text":"hi!","n":3}`}, 0},
		{"unary proto file", "/streamx.test.Echo/Unary", "", []string{`{"text":"hi"}`}, true, []string{`{"text":"hi!"}`}, 0},
		{"unary no body", "streamx.test.Echo.Unary", "", nil, false, []string{`{"text":"!"}`}, 0},
		{"unary error status", "streamx.test.Echo/Unary", "", []string{`{"text":"fail"}`}, false, nil, 3},
		{"server streaming", "streamx.test.Echo/ServerStream", "server-streaming", []string{`{"text":"s","count":3}`}, false,
			[]string{`{"text":"s"}`, `{"text":"s","n":1}`, `{"text":"s","n":2}`}, 0},
		{"client streaming", "streamx.test.Echo/ClientStream", "client-streaming", []string{`{"text":"a"}`, `{"text":"b"}`}, true,
			[]string{`"count":2,"texts":["a","b"]`}, 0},
		{"bidi", "streamx.test.Echo/Bidi", "bidi-streaming", []string{`{"text":"a"}`, `{"text":"b","n":5}`}, false,
			[]string{`{"text":"a","n":1}`, `{"text":"b","n":6}`}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := core.Request{URL: "grpc://" + addr, Method: tt.method, MethodType: tt.mtype, Messages: tt.msgs,
				Headers: []core.KV{{Name: "X-Suffix", Value: "!"}}}
			if tt.proto {
				req.ProtoFile = filepath.Join(protoDir, "echo.proto")
				req.ImportPaths = []string{protoDir}
			}
			got, st := summary(call(t, req))
			if st.Code != tt.wantCode {
				t.Fatalf("status = %d %s %q, want %d", st.Code, st.Text, st.Err, tt.wantCode)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("received %v, want %v", got, tt.want)
			}
			for i := range got {
				if !strings.Contains(strings.ReplaceAll(got[i], " ", ""), tt.want[i]) {
					t.Errorf("message %d = %s, want it to contain %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHeadersAndMethodTypeMismatch(t *testing.T) {
	addr := serve(t)
	evs := call(t, core.Request{URL: addr, Method: "streamx.test.Echo/Unary", Body: `{}`})
	if evs[0].Kind != core.Sent {
		t.Errorf("first event = %s, want sent", evs[0].Kind)
	}
	found := false
	for _, e := range evs {
		found = found || (e.Kind == core.Headers && e.Meta["x-served-by"] == "testserver")
	}
	if !found {
		t.Errorf("no x-served-by header in %+v", evs)
	}
	_, err := grpcengine.Engine{}.Run(context.Background(),
		core.Request{URL: addr, Method: "streamx.test.Echo/Unary", MethodType: "bidi-streaming"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unary") {
		t.Errorf("mismatch err = %v", err)
	}
}

func TestBadMessage(t *testing.T) {
	addr := serve(t)
	evs := call(t, core.Request{URL: addr, Method: "streamx.test.Echo/Unary", Body: `{"nope":1}`})
	if evs[0].Kind != core.Error || !strings.Contains(evs[0].Err, "streamx.test.Msg") {
		t.Errorf("first event = %+v, want a readable error", evs[0])
	}
}

func TestTLS(t *testing.T) {
	certs, err := testserver.NewCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tlsAddr := serve(t, grpc.Creds(credentials.NewTLS(certs.ServerTLS(false))))
	mtlsAddr := serve(t, grpc.Creds(credentials.NewTLS(certs.ServerTLS(true))))
	port := func(a string) string { _, p, _ := net.SplitHostPort(a); return p }
	req := func(addr string, tls core.TLS) core.Request {
		return core.Request{URL: "grpcs://localhost:" + port(addr), Method: "streamx.test.Echo/Unary", Body: `{"text":"x"}`, TLS: tls}
	}
	ok := func(evs []core.Event) bool { _, st := summary(evs); return st.Kind == core.Status && st.Code == 0 }

	if !ok(call(t, req(tlsAddr, core.TLS{CAFile: certs.CAFile}))) {
		t.Error("TLS with CA failed")
	}
	if !ok(call(t, req(tlsAddr, core.TLS{Insecure: true}))) {
		t.Error("TLS insecure failed")
	}
	if !ok(call(t, req(mtlsAddr, core.TLS{CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}))) {
		t.Error("mTLS failed")
	}
	// Without a client cert the mTLS server must refuse: reflection fails before any call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (grpcengine.Engine{}).Run(ctx, req(mtlsAddr, core.TLS{CAFile: certs.CAFile}), nil); err == nil {
		t.Error("mTLS without client cert succeeded")
	}
}

func TestMethods(t *testing.T) {
	addr := serve(t)
	echo := "streamx.test.Echo/Bidi:bidi-streaming streamx.test.Echo/ClientStream:client-streaming " +
		"streamx.test.Echo/ServerStream:server-streaming streamx.test.Echo/Unary:unary"
	for name, req := range map[string]core.Request{
		"reflection": {URL: addr},
		"proto file": {URL: addr, ProtoFile: filepath.Join(protoDir, "echo.proto"), ImportPaths: []string{protoDir}},
	} {
		ms, err := grpcengine.Methods(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, m := range ms {
			got = append(got, m.FullName()+":"+m.Type)
		}
		want := echo // a proto file lists its own services; reflection lists the server's
		if req.ProtoFile == "" {
			want += " streamx.test.Types/Echo:unary"
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s: got %v", name, got)
		}
		if ms[3].Template != `{"text":"","n":0}` {
			t.Errorf("%s: Unary template = %s", name, ms[3].Template)
		}
	}
}

// --- grpcurl edge cases ---------------------------------------------------------

func TestReflectionDisabled(t *testing.T) {
	s, err := testserver.NewWithoutReflection(protoDir)
	if err != nil {
		t.Fatal(err)
	}
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	req := core.Request{URL: lis.Addr().String(), Method: "streamx.test.Echo/Unary", Body: `{"text":"x"}`}
	_, err = grpcengine.Engine{}.Run(context.Background(), req, nil)
	if err == nil || !strings.Contains(err.Error(), "set protoFilePath") {
		t.Fatalf("err = %v, want a hint to use protoFilePath", err)
	}
	req.ProtoFile, req.ImportPaths = filepath.Join(protoDir, "echo.proto"), []string{protoDir}
	if _, st := summary(call(t, req)); st.Code != 0 {
		t.Errorf("proto file fallback failed: %+v", st)
	}
}

func TestReflectionBehindAuth(t *testing.T) {
	auth := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		if v := md.Get("authorization"); len(v) == 0 || v[0] != "Bearer s3cret" {
			return status.Error(codes.Unauthenticated, "no token")
		}
		return h(srv, ss)
	}
	addr := serve(t, grpc.StreamInterceptor(auth))
	req := core.Request{URL: addr, Method: "streamx.test.Echo/Unary", Body: `{"text":"x"}`}
	if _, err := (grpcengine.Engine{}).Run(context.Background(), req, nil); err == nil || !strings.Contains(err.Error(), "Unauthenticated") {
		t.Errorf("without token: err = %v", err)
	}
	req.Headers = []core.KV{{Name: "Authorization", Value: "Bearer s3cret"}}
	if _, st := summary(call(t, req)); st.Code != 0 {
		t.Errorf("with token (also on reflection calls): %+v", st)
	}
}

func TestJSONMappings(t *testing.T) {
	addr := serve(t)
	in := `{
	  "any": {"@type": "type.googleapis.com/streamx.test.Msg", "text": "inside", "n": 2},
	  "at": "2026-11-06T10:00:00Z",
	  "took": "1.500s",
	  "meta": {"k": [1, "two", null, true]},
	  "maybe": "yes",
	  "color": "GREEN",
	  "raw": "aGVsbG8=",
	  "counts": {"a": "9007199254740993"},
	  "name": "oneof",
	  "big": "-9007199254740993",
	  "items": [{"text": "x"}]
	}`
	got, st := summary(call(t, core.Request{URL: addr, Method: "streamx.test.Types/Echo", Body: in}))
	if st.Code != 0 || len(got) != 1 {
		t.Fatalf("status %+v got %v", st, got)
	}
	var a, b any
	json.Unmarshal([]byte(in), &a)
	json.Unmarshal([]byte(got[0]), &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("round trip changed the message:\n in  %s\n out %s", in, got[0])
	}
}

func TestReadableErrors(t *testing.T) {
	addr := serve(t)
	evs := call(t, core.Request{URL: addr, Method: "streamx.test.Echo/Unary", Body: `{"txt": "typo"}`})
	if evs[0].Kind != core.Error || !strings.Contains(evs[0].Err, `unknown field "txt"`) || !strings.Contains(evs[0].Err, "text, n") {
		t.Errorf("error = %q, want the unknown field and the valid ones", evs[0].Err)
	}
	evs = call(t, core.Request{URL: addr, Method: "streamx.test.Types/Echo", Body: `{"color": "PURPLE"}`})
	if evs[0].Kind != core.Error || !strings.Contains(evs[0].Err, "PURPLE") {
		t.Errorf("enum error = %q", evs[0].Err)
	}
	if strings.Contains(evs[0].Err, "proto: ") {
		t.Errorf("error keeps the library prefix: %q", evs[0].Err)
	}
}

func TestStatusDetails(t *testing.T) {
	addr := serve(t)
	_, st := summary(call(t, core.Request{URL: addr, Method: "streamx.test.Types/Echo", Body: `{"name": "details"}`}))
	if st.Code != 3 || !strings.Contains(string(st.Data), `"field":"name"`) || !strings.Contains(string(st.Data), "google.rpc.BadRequest") {
		t.Errorf("status = %+v data %s", st, st.Data)
	}
}

func TestLargeMessages(t *testing.T) {
	addr := serve(t, grpc.MaxRecvMsgSize(64<<20), grpc.MaxSendMsgSize(64<<20))
	big := strings.Repeat("x", 5<<20) // over grpc-go's 4 MB default receive limit
	got, st := summary(call(t, core.Request{URL: addr, Method: "streamx.test.Echo/Unary", Body: `{"text":"` + big + `"}`}))
	if st.Code != 0 || len(got) != 1 || len(got[0]) < 5<<20 {
		t.Errorf("status %+v, got %d bytes", st, len(strings.Join(got, "")))
	}
}
