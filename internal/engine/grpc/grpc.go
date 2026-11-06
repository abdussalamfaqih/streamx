// Package grpcengine runs gRPC calls without generated code. Descriptors come
// from server reflection or from .proto files compiled at runtime.
package grpcengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bufbuild/protocompile"
	"github.com/jhump/protoreflect/v2/grpcreflect"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails" // registers google.rpc error detail types for status details
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

// maxMessage lifts grpc-go's 4 MB receive default; debugging tools meet big payloads.
const maxMessage = 64 << 20

type Engine struct{}

func init() { core.Register("grpc", Engine{}) }

// resolver finds types in the call's descriptors first, then in the types
// linked into this binary: well-known types and google.rpc error details
// inside Any values work even when the service's protos don't import them.
type resolver struct{ local *dynamicpb.Types }

func (r resolver) FindMessageByName(n protoreflect.FullName) (protoreflect.MessageType, error) {
	if t, err := r.local.FindMessageByName(n); err == nil {
		return t, nil
	}
	return protoregistry.GlobalTypes.FindMessageByName(n)
}

func (r resolver) FindMessageByURL(u string) (protoreflect.MessageType, error) {
	if t, err := r.local.FindMessageByURL(u); err == nil {
		return t, nil
	}
	return protoregistry.GlobalTypes.FindMessageByURL(u)
}

func (r resolver) FindExtensionByName(n protoreflect.FullName) (protoreflect.ExtensionType, error) {
	if t, err := r.local.FindExtensionByName(n); err == nil {
		return t, nil
	}
	return protoregistry.GlobalTypes.FindExtensionByName(n)
}

func (r resolver) FindExtensionByNumber(m protoreflect.FullName, n protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	if t, err := r.local.FindExtensionByNumber(m, n); err == nil {
		return t, nil
	}
	return protoregistry.GlobalTypes.FindExtensionByNumber(m, n)
}

// details renders a status's error details (google.rpc.BadRequest etc.) as a
// JSON array, like grpcurl prints them. Unknown detail types keep their type URL.
func details(st *status.Status, marshal protojson.MarshalOptions) json.RawMessage {
	ds := st.Proto().GetDetails()
	if len(ds) == 0 {
		return nil
	}
	parts := make([]string, len(ds))
	for i, d := range ds {
		b, err := marshal.Marshal(d)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"@type": d.GetTypeUrl(), "error": "unknown detail type"})
		}
		parts[i] = string(b)
	}
	return core.JSON([]byte("[" + strings.Join(parts, ",") + "]"))
}

// jsonError makes protojson errors readable: no library prefix, and for an
// unknown field the fields the message does have.
func jsonError(err error, md protoreflect.MessageDescriptor) string {
	msg := strings.TrimLeft(strings.TrimPrefix(err.Error(), "proto:"), " \u00a0")
	if strings.Contains(msg, "unknown field") {
		fs := md.Fields()
		names := make([]string, fs.Len())
		for i := range fs.Len() {
			names[i] = fs.Get(i).JSONName()
		}
		msg += fmt.Sprintf(" (fields of %s: %s)", md.FullName(), strings.Join(names, ", "))
	}
	return msg
}

func (Engine) Run(ctx context.Context, req core.Request, send <-chan []byte) (<-chan core.Event, error) {
	// shortcut: one connection per call, fine for CLI and manual use; pool per target if calls get chatty.
	conn, ctx, err := dial(ctx, req)
	if err != nil {
		return nil, err
	}
	c, err := start(ctx, conn, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	out := make(chan core.Event, 64)
	c.emit = core.Emitter(ctx, out)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.sendAll(send)
	}()
	go func() {
		defer wg.Done()
		defer c.cancel() // stops the sender once the call is over
		c.receive()
	}()
	go func() {
		wg.Wait()
		conn.Close()
		close(out)
	}()
	return out, nil
}

// call is one running RPC.
type call struct {
	ctx    context.Context
	cancel context.CancelFunc
	stream grpc.ClientStream
	method protoreflect.MethodDescriptor
	types  resolver
	emit   func(core.Event) bool
}

// start resolves req's method and opens the stream.
func start(ctx context.Context, conn *grpc.ClientConn, req core.Request) (*call, error) {
	svc, name, err := splitMethod(req.Method)
	if err != nil {
		return nil, err
	}
	files, err := loadFiles(ctx, conn, req, svc)
	if err != nil {
		return nil, err
	}
	method, err := findMethod(files, svc, name)
	if err != nil {
		return nil, err
	}
	if req.MethodType != "" && req.MethodType != MethodType(method) {
		return nil, fmt.Errorf("%s: methodType is %q but the service defines it as %q", req.Method, req.MethodType, MethodType(method))
	}
	ctx, cancel := context.WithCancel(ctx)
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    string(method.Name()),
		ServerStreams: method.IsStreamingServer(),
		ClientStreams: method.IsStreamingClient(),
	}, "/"+svc+"/"+name)
	if err != nil {
		cancel()
		return nil, err
	}
	return &call{ctx: ctx, cancel: cancel, stream: stream, method: method, types: resolver{dynamicpb.NewTypes(files)}}, nil
}

// next waits for the next message to send. It returns false when the call is
// over or the sending side is finished, which it half-closes.
func (c *call) next(send <-chan []byte, sent int) ([]byte, bool) {
	select {
	case <-c.ctx.Done():
		return nil, false
	case m, open := <-send:
		if !open && (sent > 0 || c.method.IsStreamingClient()) {
			c.stream.CloseSend()
			return nil, false
		}
		if len(m) == 0 {
			m = []byte("{}") // unary/server-streaming with no body sends an empty message, like grpcurl
		}
		return m, true
	}
}

func (c *call) sendAll(send <-chan []byte) {
	streaming := c.method.IsStreamingClient()
	in := c.method.Input()
	for sent := 0; ; {
		b, ok := c.next(send, sent)
		if !ok {
			return
		}
		msg := dynamicpb.NewMessage(in)
		if err := (protojson.UnmarshalOptions{Resolver: c.types}).Unmarshal(b, msg); err != nil {
			c.emit(core.Event{Kind: core.Error, Err: fmt.Sprintf("message %d is not a valid %s: %s", sent+1, in.FullName(), jsonError(err, in))})
			if streaming {
				continue // skip it; an interactive stream survives a typo
			}
			c.cancel()
			return
		}
		if err := c.stream.SendMsg(msg); err != nil {
			return // server closed the stream; the receiver reports the real status
		}
		c.emit(core.Event{Kind: core.Sent, Data: core.JSON(b)})
		sent++
		if !streaming {
			c.stream.CloseSend()
			return
		}
	}
}

func (c *call) receive() {
	marshal := protojson.MarshalOptions{Resolver: c.types}
	for first := true; ; first = false {
		msg := dynamicpb.NewMessage(c.method.Output())
		err := c.stream.RecvMsg(msg)
		if first {
			c.headers()
		}
		if err != nil {
			c.end(err, marshal)
			return
		}
		data, err := marshal.Marshal(msg)
		if err != nil {
			c.emit(core.Event{Kind: core.Error, Err: err.Error()})
			continue
		}
		c.emit(core.Event{Kind: core.Received, Data: core.JSON(data)}) // compacts: protojson randomizes spacing
	}
}

func (c *call) headers() {
	if h, err := c.stream.Header(); err == nil && len(h) > 0 {
		c.emit(core.Event{Kind: core.Headers, Meta: core.FlattenHeader(http.Header(h))})
	}
}

// end emits the trailers and the final status.
func (c *call) end(err error, marshal protojson.MarshalOptions) {
	if t := c.stream.Trailer(); len(t) > 0 {
		c.emit(core.Event{Kind: core.Trailers, Meta: core.FlattenHeader(http.Header(t))})
	}
	st := status.New(codes.OK, "")
	if !errors.Is(err, io.EOF) {
		st = status.Convert(err)
	}
	c.emit(core.Event{Kind: core.Status, Code: int(st.Code()), Text: st.Code().String(), Err: st.Message(), Data: details(st, marshal)})
}

// dial connects to req.URL and returns a context carrying req.Headers as
// metadata (also sent on reflection calls, for reflection behind auth).
func dial(ctx context.Context, req core.Request) (*grpc.ClientConn, context.Context, error) {
	target, useTLS, err := parseTarget(req.URL, req.TLS.Enabled())
	if err != nil {
		return nil, nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		tc, err := req.TLS.Config()
		if err != nil {
			return nil, nil, err
		}
		creds = credentials.NewTLS(tc)
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage), grpc.MaxCallSendMsgSize(maxMessage)))
	if err != nil {
		return nil, nil, err
	}
	md := metadata.MD{}
	for _, h := range req.Headers {
		md.Append(strings.ToLower(h.Name), h.Value)
	}
	return conn, metadata.NewOutgoingContext(ctx, md), nil
}

// Method describes one RPC for browsing.
type Method struct {
	Service  string // pkg.Service
	Name     string
	Type     string // OpenCollection methodType
	Template string // input message with every top-level field, as JSON
}

func (m Method) FullName() string { return m.Service + "/" + m.Name }

// Methods lists every method at req's server (reflection) or in req.ProtoFile.
func Methods(ctx context.Context, req core.Request) ([]Method, error) {
	conn, ctx, err := dial(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	list := reflectServices
	if req.ProtoFile != "" {
		list = protoServices
	}
	services, err := list(ctx, conn, req)
	if err != nil {
		return nil, err
	}
	var out []Method
	for _, sd := range services {
		for i := range sd.Methods().Len() {
			m := sd.Methods().Get(i)
			tmpl, _ := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(dynamicpb.NewMessage(m.Input()))
			out = append(out, Method{Service: string(sd.FullName()), Name: string(m.Name()), Type: MethodType(m), Template: string(core.JSON(tmpl))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName() < out[j].FullName() })
	return out, nil
}

// protoServices lists the services defined in req.ProtoFile itself, not its imports.
func protoServices(ctx context.Context, conn *grpc.ClientConn, req core.Request) ([]protoreflect.ServiceDescriptor, error) {
	files, err := loadFiles(ctx, conn, req, "")
	if err != nil {
		return nil, err
	}
	var services []protoreflect.ServiceDescriptor
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Path() == protoPath(req) {
			for i := range fd.Services().Len() {
				services = append(services, fd.Services().Get(i))
			}
		}
		return true
	})
	return services, nil
}

// reflectServices lists the server's services, minus reflection itself.
func reflectServices(ctx context.Context, conn *grpc.ClientConn, req core.Request) ([]protoreflect.ServiceDescriptor, error) {
	rc := grpcreflect.NewClientAuto(ctx, conn)
	defer rc.Reset()
	names, err := rc.ListServices()
	if err != nil {
		return nil, reflectionErr(err, req.URL)
	}
	var services []protoreflect.ServiceDescriptor
	for _, n := range names {
		if strings.HasPrefix(string(n), "grpc.reflection.") {
			continue
		}
		fd, err := rc.FileContainingSymbol(n)
		if err != nil {
			return nil, reflectionErr(err, req.URL)
		}
		if sd := fd.Services().ByName(n.Name()); sd != nil {
			services = append(services, sd)
		}
	}
	return services, nil
}

func reflectionErr(err error, url string) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("server reflection is not available on %s; set protoFilePath to use local .proto files", url)
	}
	return fmt.Errorf("reflection: %w", err)
}

// MethodType names a method's call type with OpenCollection's methodType values.
func MethodType(m protoreflect.MethodDescriptor) string {
	switch {
	case m.IsStreamingClient() && m.IsStreamingServer():
		return "bidi-streaming"
	case m.IsStreamingClient():
		return "client-streaming"
	case m.IsStreamingServer():
		return "server-streaming"
	}
	return "unary"
}

// parseTarget turns a request URL into a dial target. grpcs:// and https://
// mean TLS; grpc:// and http:// mean plaintext; no scheme means TLS only if a
// TLS option is set.
func parseTarget(raw string, tlsOpts bool) (target string, useTLS bool, err error) {
	host := raw
	useTLS = tlsOpts
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", false, fmt.Errorf("bad url %q: %w", raw, err)
		}
		switch u.Scheme {
		case "grpcs", "https":
			useTLS = true
		case "grpc", "http":
		default:
			return "", false, fmt.Errorf("bad url %q: scheme must be grpc, grpcs, http or https", raw)
		}
		host = u.Host
	}
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		return "", false, fmt.Errorf("missing url")
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "80"
		if useTLS {
			port = "443"
		}
		host = net.JoinHostPort(host, port)
	}
	return host, useTLS, nil
}

// splitMethod accepts "pkg.Svc/Method", "/pkg.Svc/Method" and "pkg.Svc.Method".
func splitMethod(m string) (svc, name string, err error) {
	m = strings.TrimPrefix(strings.TrimSpace(m), "/")
	i := strings.LastIndex(m, "/")
	if i < 0 {
		i = strings.LastIndex(m, ".")
	}
	if i <= 0 || i == len(m)-1 {
		return "", "", fmt.Errorf("bad gRPC method %q: want package.Service/Method", m)
	}
	return m[:i], m[i+1:], nil
}

func loadFiles(ctx context.Context, conn *grpc.ClientConn, req core.Request, svc string) (*protoregistry.Files, error) {
	var fd protoreflect.FileDescriptor
	if req.ProtoFile != "" {
		name, paths := protoName(req.ProtoFile, req.ImportPaths)
		c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: paths})}
		files, err := c.Compile(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", req.ProtoFile, err)
		}
		fd = files[0]
	} else {
		if svc == "" {
			return nil, errors.New("no service to look up")
		}
		rc := grpcreflect.NewClientAuto(ctx, conn)
		defer rc.Reset()
		var err error
		fd, err = rc.FileContainingSymbol(protoreflect.FullName(svc))
		if err != nil {
			return nil, reflectionErr(err, req.URL)
		}
	}
	return Registry(fd)
}

// Registry holds fds and everything they import.
func Registry(fds ...protoreflect.FileDescriptor) (*protoregistry.Files, error) {
	files := new(protoregistry.Files)
	for _, fd := range fds {
		if err := register(files, fd); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func protoPath(req core.Request) string {
	name, _ := protoName(req.ProtoFile, req.ImportPaths)
	return name
}

// protoName makes file relative to one of the import paths, adding its own
// directory as an import path if it is under none of them.
func protoName(file string, importPaths []string) (string, []string) {
	for _, ip := range importPaths {
		if rel, err := filepath.Rel(ip, file); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel), importPaths
		}
	}
	return filepath.Base(file), append(append([]string{}, importPaths...), filepath.Dir(file))
}

func register(files *protoregistry.Files, fd protoreflect.FileDescriptor) error {
	if _, err := files.FindFileByPath(fd.Path()); err == nil {
		return nil
	}
	imps := fd.Imports()
	for i := range imps.Len() {
		if err := register(files, imps.Get(i).FileDescriptor); err != nil {
			return err
		}
	}
	return files.RegisterFile(fd)
}

func findMethod(files *protoregistry.Files, svc, name string) (protoreflect.MethodDescriptor, error) {
	d, err := files.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil, fmt.Errorf("service %s not found", svc)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a service", svc)
	}
	m := sd.Methods().ByName(protoreflect.Name(name))
	if m == nil {
		return nil, fmt.Errorf("method %s not found in service %s", name, svc)
	}
	return m, nil
}
