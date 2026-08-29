package fixtures

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// SSEFrame is one parsed server-sent event.
//
// Data is the concatenation of the frame's data: lines, newline-joined, with
// the single leading space after the colon removed — i.e. exactly what the
// EventSource spec says a consumer should see.
type SSEFrame struct {
	// Event is the event: field, empty for OpenAI-style unnamed frames.
	Event string
	// Data is the payload, e.g. a JSON chunk or the literal "[DONE]".
	Data string
	// ID is the id: field where present.
	ID string
}

// ParseSSE reads an SSE body to EOF and returns its frames.
//
// It is exported because every adapter test needs to assert on the wire, and
// re-implementing a parser per test package is how subtle disagreements creep
// in. It deliberately does not interpret payloads.
func ParseSSE(r io.Reader) ([]SSEFrame, error) {
	var (
		frames []SSEFrame
		cur    SSEFrame
		data   []string
		open   bool
	)
	flush := func() {
		if !open {
			return
		}
		cur.Data = strings.Join(data, "\n")
		frames = append(frames, cur)
		cur, data, open = SSEFrame{}, nil, false
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			cur.Event, open = value, true
		case "data":
			data, open = append(data, value), true
		case "id":
			cur.ID, open = value, true
		}
	}
	// A truncated stream ends without its blank-line terminator; surface the
	// partial frame rather than silently dropping it.
	flush()
	if err := sc.Err(); err != nil {
		return frames, err
	}
	return frames, nil
}

// ReadSSE parses the body of resp and closes it.
func ReadSSE(resp *http.Response) ([]SSEFrame, error) {
	defer resp.Body.Close()
	return ParseSSE(resp.Body)
}

// sseWriter serializes frames onto a response, applying the per-response
// pacing and truncation/drop injection.
//
// Once stopped, every further frame call is a no-op returning false so
// renderers can simply `if !w.frame(...) { return }`.
type sseWriter struct {
	w          io.Writer
	flusher    http.Flusher
	bw         *bufio.ReadWriter
	conn       net.Conn
	frames     int
	truncate   int
	drop       int
	chunkDelay time.Duration
	stopped    bool
}

// newSSEWriter prepares w for event-stream output, hijacking the connection
// when the script asks for a mid-stream disconnect.
func newSSEWriter(w http.ResponseWriter, resp *Response) (*sseWriter, error) {
	s := &sseWriter{
		truncate:   resp.TruncateAfter,
		drop:       resp.DropAfter,
		chunkDelay: resp.ChunkDelay,
	}
	if resp.DropAfter > 0 {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return nil, errors.New("fixtures: response writer does not support hijacking")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return nil, fmt.Errorf("fixtures: hijack: %w", err)
		}
		s.conn, s.bw = conn, buf
		// The status line and headers are ours to write once hijacked. The
		// body is chunked on purpose: a chunked stream cut short is an
		// unexpected EOF for the client, whereas a close-delimited body would
		// look like a perfectly graceful end and prove nothing.
		fmt.Fprint(buf,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: text/event-stream\r\n"+
				"Cache-Control: no-cache\r\n"+
				"Transfer-Encoding: chunked\r\n\r\n")
		buf.Flush()
		s.w = chunkedWriter{w: buf}
		return s, nil
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	s.w = w
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
		f.Flush()
	}
	return s, nil
}

// frame writes one event and reports whether the stream may continue.
func (s *sseWriter) frame(event, data string) bool {
	if s.stopped {
		return false
	}
	if s.chunkDelay > 0 {
		time.Sleep(s.chunkDelay)
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(s.w, b.String()); err != nil {
		s.stopped = true
		return false
	}
	s.flushOut()
	s.frames++

	if s.drop > 0 && s.frames >= s.drop {
		s.stopped = true
		s.closeConn()
		return false
	}
	if s.truncate > 0 && s.frames >= s.truncate {
		s.stopped = true
		return false
	}
	return true
}

// flushOut pushes buffered bytes to the client so pacing is observable.
func (s *sseWriter) flushOut() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
	if s.bw != nil {
		s.bw.Flush()
	}
}

// chunkedWriter applies HTTP/1.1 chunked transfer encoding to each write, so
// a hijacked stream can be cut off mid-body in a way clients detect.
type chunkedWriter struct{ w io.Writer }

func (c chunkedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if _, err := fmt.Fprintf(c.w, "%x\r\n", len(p)); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	if err != nil {
		return n, err
	}
	_, err = io.WriteString(c.w, "\r\n")
	return n, err
}

// closeConn drops a hijacked connection, simulating a server crash mid-stream.
func (s *sseWriter) closeConn() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// finish releases a hijacked connection after a complete stream.
func (s *sseWriter) finish() {
	s.flushOut()
	s.closeConn()
}
