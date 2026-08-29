package execution

import (
	"fmt"
	"strings"
	"sync"
)

// DefaultMaxOutputBytes is the per-stream capture cap applied when neither the
// request nor the executor specifies one.
//
// 256 KiB is generous for human inspection while staying far below the context
// window of any model the output is likely to be fed to.
const DefaultMaxOutputBytes = 256 << 10

// elisionFormat is the marker inserted between the retained head and tail of a
// truncated stream. It is deliberately verbose and machine-readable enough that
// a model reading the output cannot mistake it for program output.
const elisionFormat = "\n\n... [boop: %d bytes elided] ...\n\n"

// boundedCapture accumulates a process output stream under a byte cap while
// preserving both ends of the stream.
//
// Naive truncation keeps only the head, which is exactly the wrong half: the
// head says what the command started doing, but the tail holds the failing
// assertion, the compiler error, or the stack trace. Keeping only the tail
// loses the invocation banner and the first error, which is usually the causal
// one. boundedCapture therefore retains the first limit/2 bytes verbatim and
// the last limit-limit/2 bytes in a ring buffer, joining them with an explicit
// elision marker. The marker itself is not counted against the cap, so the
// rendered string may exceed limit by the marker's length.
//
// It is safe for concurrent use: the pipe reader writes while the executor may
// read the result after the process exits.
type boundedCapture struct {
	mu sync.Mutex

	headCap int
	head    []byte

	tailCap   int
	tail      []byte
	tailStart int
	tailLen   int

	total int64
}

// newBoundedCapture returns a capture that retains at most limit bytes of
// stream content. A non-positive limit selects DefaultMaxOutputBytes.
func newBoundedCapture(limit int) *boundedCapture {
	if limit <= 0 {
		limit = DefaultMaxOutputBytes
	}
	head := limit / 2
	tail := limit - head
	return &boundedCapture{
		headCap: head,
		head:    make([]byte, 0, head),
		tailCap: tail,
		tail:    make([]byte, tail),
	}
}

// Write implements io.Writer. It never returns an error and never reports a
// short write: exceeding the cap is recorded, not failed, so a chatty command
// cannot break its own capture.
func (c *boundedCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := len(p)
	c.total += int64(n)

	if room := c.headCap - len(c.head); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.head = append(c.head, p[:room]...)
		p = p[room:]
	}
	c.pushTail(p)
	return n, nil
}

// pushTail appends p to the ring buffer holding the tail of the stream.
func (c *boundedCapture) pushTail(p []byte) {
	if c.tailCap == 0 || len(p) == 0 {
		return
	}
	if len(p) >= c.tailCap {
		copy(c.tail, p[len(p)-c.tailCap:])
		c.tailStart = 0
		c.tailLen = c.tailCap
		return
	}
	for _, b := range p {
		c.tail[(c.tailStart+c.tailLen)%c.tailCap] = b
		if c.tailLen < c.tailCap {
			c.tailLen++
			continue
		}
		c.tailStart = (c.tailStart + 1) % c.tailCap
	}
}

// tailBytes returns the ring buffer contents in stream order.
func (c *boundedCapture) tailBytes() []byte {
	if c.tailLen == 0 {
		return nil
	}
	out := make([]byte, 0, c.tailLen)
	end := c.tailStart + c.tailLen
	if end <= c.tailCap {
		return append(out, c.tail[c.tailStart:end]...)
	}
	out = append(out, c.tail[c.tailStart:]...)
	return append(out, c.tail[:end-c.tailCap]...)
}

// Total reports how many bytes the stream produced, including elided ones.
func (c *boundedCapture) Total() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Result renders the captured stream and reports whether anything was elided.
func (c *boundedCapture) Result() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tail := c.tailBytes()
	kept := int64(len(c.head)) + int64(len(tail))
	if c.total <= kept {
		var b strings.Builder
		b.Grow(int(kept))
		b.Write(c.head)
		b.Write(tail)
		return b.String(), false
	}

	elided := c.total - kept
	marker := fmt.Sprintf(elisionFormat, elided)

	var b strings.Builder
	b.Grow(int(kept) + len(marker))
	b.Write(c.head)
	b.WriteString(marker)
	b.Write(tail)
	return b.String(), true
}
