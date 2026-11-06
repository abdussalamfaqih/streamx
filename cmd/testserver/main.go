// Command testserver serves the Echo gRPC test service (testdata/protos/echo.proto,
// reflection on) and the HTTP/SSE/WebSocket test endpoints, plaintext, for
// trying streamx by hand.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/abdussalamfaqih/streamx/internal/testserver"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "gRPC listen address")
	httpAddr := flag.String("http", "localhost:8080", "HTTP/SSE/WebSocket listen address")
	protos := flag.String("protos", "testdata/protos", "directory holding echo.proto")
	flag.Parse()
	s, err := testserver.New(*protos)
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		log.Printf("HTTP, SSE and WebSocket on %s", *httpAddr)
		log.Fatal(http.ListenAndServe(*httpAddr, testserver.HTTP()))
	}()
	log.Printf("Echo gRPC service on %s (reflection on)", lis.Addr())
	log.Fatal(s.Serve(lis))
}
