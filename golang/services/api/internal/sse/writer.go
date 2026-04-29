package sse

import (
	"fmt"
	"net/http"
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
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.flusher.Flush()
}

// SendEvent writes a named SSE event frame and flushes.
func (s *Writer) SendEvent(event, data string) {
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	s.flusher.Flush()
}
