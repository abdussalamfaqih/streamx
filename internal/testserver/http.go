package testserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Token is what /token hands out.
const Token = "tok-2f9c81e4a7d0"

// HTTP serves the endpoints the HTTP, SSE and WebSocket examples use:
//
//	/echo            JSON {method, headers, body} of the request
//	/token           JSON {"token": Token}
//	/status/{code}   that status, JSON {"code": n}
//	/text            plain text "hello"
//	/sse?count=N     N SSE messages "tick" with data {"n": i}, then closes
//	/ws              WebSocket echo; JSON {"n": x} comes back with n+1; text "bye" closes normally
func HTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hs := map[string]string{}
		for k, v := range r.Header {
			hs[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		writeJSON(w, 200, map[string]any{"method": r.Method, "headers": hs, "body": string(b)})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"token": Token, "expires_in": 3600})
	})
	mux.HandleFunc("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.PathValue("code"))
		if err != nil || code < 100 || code > 599 {
			code = 400
		}
		writeJSON(w, code, map[string]any{"code": code})
	})
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	})
	mux.HandleFunc("/sse", sse)
	mux.HandleFunc("/ws", wsEcho)
	return mux
}

func sse(w http.ResponseWriter, r *http.Request) {
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	io.WriteString(w, ": keep-alive comment\n\n")
	for i := range count {
		fmt.Fprintf(w, "event: tick\nid: %d\ndata: {\"n\": %d}\n\n", i, i)
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func wsEcho(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	for {
		typ, b, err := c.Read(r.Context())
		if err != nil {
			return
		}
		if string(b) == "bye" {
			c.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		if err := c.Write(r.Context(), typ, bump(b)); err != nil {
			return
		}
	}
}

// bump adds 1 to "n" in a JSON object; anything else comes back as is.
func bump(b []byte) []byte {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if n, ok := m["n"].(float64); ok {
		m["n"] = n + 1
	}
	b, _ = json.Marshal(m)
	return b
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
