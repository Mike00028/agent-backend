package sse

import (
	"net/http"
	"strings"
)

// Writer wraps an http.ResponseWriter to write SSE frames.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// New creates an SSE Writer and sets the required headers.
func New(w http.ResponseWriter) (*Writer, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &Writer{w: w, flusher: f}, true
}

// Send writes a single SSE data frame and flushes immediately.
func (s *Writer) Send(data string) {
	s.sendLines("", data)
}

// SendEvent writes a named SSE event frame and flushes.
// Multi-line data is split into multiple data: lines per the SSE spec.
func (s *Writer) SendEvent(event, data string) {
	s.sendLines(event, data)
}

func (s *Writer) sendLines(event, data string) {
	var sb strings.Builder
	if event != "" {
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteByte('\n')
	}
	for i, line := range strings.Split(data, "\n") {
		_ = i
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	s.w.Write([]byte(sb.String())) //nolint:errcheck
	s.flusher.Flush()
}
