package core

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// JSON returns b compacted if it is valid JSON, otherwise b as a JSON string,
// so every event body is valid JSON for assertions and transcripts.
func JSON(b []byte) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, b) == nil && buf.Len() > 0 {
		return buf.Bytes()
	}
	s, _ := json.Marshal(string(b))
	return s
}

// FlattenHeader turns HTTP headers into lower-case event metadata.
func FlattenHeader(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return out
}

// TranscriptLine is one JSONL transcript record.
type TranscriptLine struct {
	Request string `json:"request"`
	Event
}

// WriteTranscript appends events as JSON lines. Callers must pass redacted events.
func WriteTranscript(w io.Writer, request string, evs ...Event) error {
	enc := json.NewEncoder(w)
	for _, ev := range evs {
		if err := enc.Encode(TranscriptLine{request, ev}); err != nil {
			return err
		}
	}
	return nil
}

// CreateTranscript creates path for writing, private to the user since it
// holds response data. It never overwrites unless overwrite is set.
func CreateTranscript(path string, overwrite bool) (*os.File, error) {
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	return os.OpenFile(path, flag, 0o600)
}
