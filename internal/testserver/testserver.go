// Package testserver serves testdata/protos/echo.proto dynamically (no generated
// code) with server reflection, for tests and manual runs.
package testserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	grpcengine "github.com/abdussalamfaqih/streamx/internal/engine/grpc"
)

// New returns a gRPC server with the Echo and Types services and reflection.
func New(protoDir string, opts ...grpc.ServerOption) (*grpc.Server, error) {
	return newServer(protoDir, true, opts)
}

// NewWithoutReflection is New with server reflection turned off.
func NewWithoutReflection(protoDir string, opts ...grpc.ServerOption) (*grpc.Server, error) {
	return newServer(protoDir, false, opts)
}

func newServer(protoDir string, withReflection bool, opts []grpc.ServerOption) (*grpc.Server, error) {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{protoDir}})}
	files, err := c.Compile(context.Background(), "echo.proto", "types.proto")
	if err != nil {
		return nil, err
	}
	reg, err := grpcengine.Registry(files[0], files[1])
	if err != nil {
		return nil, err
	}
	echo := files[0].Services().ByName("Echo")
	types := files[1].Services().ByName("Types")
	m := func(sd protoreflect.ServiceDescriptor, n string) protoreflect.MethodDescriptor {
		return sd.Methods().ByName(protoreflect.Name(n))
	}
	s := grpc.NewServer(opts...)
	for sd, handlers := range map[protoreflect.ServiceDescriptor]map[string]grpc.StreamHandler{
		echo: {
			"Unary":        unary(m(echo, "Unary")),
			"ServerStream": serverStream(m(echo, "ServerStream")),
			"ClientStream": clientStream(m(echo, "ClientStream")),
			"Bidi":         bidi(m(echo, "Bidi")),
		},
		types: {"Echo": typesEcho(m(types, "Echo"))},
	} {
		desc := grpc.ServiceDesc{ServiceName: string(sd.FullName()), HandlerType: (*any)(nil)}
		for name, h := range handlers {
			md := m(sd, name)
			desc.Streams = append(desc.Streams, grpc.StreamDesc{
				StreamName: name, Handler: h,
				ServerStreams: md.IsStreamingServer(), ClientStreams: md.IsStreamingClient(),
			})
		}
		s.RegisterService(&desc, struct{}{})
	}
	if withReflection {
		reflectionpb.RegisterServerReflectionServer(s, reflection.NewServerV1(reflection.ServerOptions{Services: s, DescriptorResolver: reg}))
	}
	return s, nil
}

func typesEcho(md protoreflect.MethodDescriptor) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		in, err := recv(ss, md.Input())
		if err != nil {
			return err
		}
		if get(in, "name").String() == "details" {
			st, _ := status.New(codes.InvalidArgument, "bad request").WithDetails(&errdetails.BadRequest{
				FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "name", Description: "must not be details"}},
			})
			return st.Err()
		}
		return ss.SendMsg(in)
	}
}

func get(m protoreflect.Message, f string) protoreflect.Value {
	return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(f)))
}

func set(m protoreflect.Message, f string, v protoreflect.Value) {
	m.Set(m.Descriptor().Fields().ByName(protoreflect.Name(f)), v)
}

func recv(ss grpc.ServerStream, md protoreflect.MessageDescriptor) (*dynamicpb.Message, error) {
	msg := dynamicpb.NewMessage(md)
	return msg, ss.RecvMsg(msg)
}

func unary(md protoreflect.MethodDescriptor) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		in, err := recv(ss, md.Input())
		if err != nil {
			return err
		}
		text := get(in, "text").String()
		if text == "fail" {
			return status.Error(codes.InvalidArgument, "fail requested")
		}
		if inMD, ok := metadata.FromIncomingContext(ss.Context()); ok && len(inMD["x-suffix"]) > 0 {
			text += inMD["x-suffix"][0]
		}
		ss.SetHeader(metadata.Pairs("x-served-by", "testserver"))
		out := dynamicpb.NewMessage(md.Output())
		set(out, "text", protoreflect.ValueOfString(text))
		set(out, "n", get(in, "n"))
		return ss.SendMsg(out)
	}
}

func serverStream(md protoreflect.MethodDescriptor) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		in, err := recv(ss, md.Input())
		if err != nil {
			return err
		}
		for i := range get(in, "count").Int() {
			out := dynamicpb.NewMessage(md.Output())
			set(out, "text", get(in, "text"))
			set(out, "n", protoreflect.ValueOfInt32(int32(i)))
			if err := ss.SendMsg(out); err != nil {
				return err
			}
		}
		return nil
	}
}

func clientStream(md protoreflect.MethodDescriptor) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		out := dynamicpb.NewMessage(md.Output())
		texts := out.Mutable(md.Output().Fields().ByName("texts")).List()
		n := int32(0)
		for {
			in, err := recv(ss, md.Input())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			n++
			texts.Append(get(in, "text"))
		}
		set(out, "count", protoreflect.ValueOfInt32(n))
		atMsg := dynamicpb.NewMessage(md.Output().Fields().ByName("at").Message())
		set(atMsg, "seconds", protoreflect.ValueOfInt64(time.Now().Unix()))
		set(out, "at", protoreflect.ValueOfMessage(atMsg))
		return ss.SendMsg(out)
	}
}

func bidi(md protoreflect.MethodDescriptor) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		for {
			in, err := recv(ss, md.Input())
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			set(in, "n", protoreflect.ValueOfInt32(int32(get(in, "n").Int())+1))
			if err := ss.SendMsg(in); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}
}
