package execution

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestBoundedCaptureResult(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		writes    []string
		want      string
		truncated bool
	}{
		{
			name:   "empty",
			limit:  16,
			writes: nil,
			want:   "",
		},
		{
			name:   "under limit single write",
			limit:  16,
			writes: []string{"hello"},
			want:   "hello",
		},
		{
			name:   "under limit many writes",
			limit:  16,
			writes: []string{"a", "b", "c", "d"},
			want:   "abcd",
		},
		{
			name:   "exactly at limit",
			limit:  4,
			writes: []string{"ab", "cd"},
			want:   "abcd",
		},
		{
			name:      "one byte over limit",
			limit:     4,
			writes:    []string{"abcde"},
			want:      "ab" + fmt.Sprintf(elisionFormat, 1) + "de",
			truncated: true,
		},
		{
			name:      "head and tail preserved across many writes",
			limit:     8,
			writes:    []string{"HEAD", "xxxxxxxxxxxx", "TAIL"},
			want:      "HEAD" + fmt.Sprintf(elisionFormat, 12) + "TAIL",
			truncated: true,
		},
		{
			name:      "single oversized write keeps both ends",
			limit:     8,
			writes:    []string{"HEADmiddlemiddleTAIL"},
			want:      "HEAD" + fmt.Sprintf(elisionFormat, 12) + "TAIL",
			truncated: true,
		},
		{
			name:      "odd limit splits head and tail",
			limit:     5,
			writes:    []string{"0123456789"},
			want:      "01" + fmt.Sprintf(elisionFormat, 5) + "789",
			truncated: true,
		},
		{
			name:      "limit of one keeps last byte",
			limit:     1,
			writes:    []string{"abc"},
			want:      fmt.Sprintf(elisionFormat, 2) + "c",
			truncated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newBoundedCapture(tt.limit)
			var total int
			for _, w := range tt.writes {
				n, err := c.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error: %v", w, err)
				}
				if n != len(w) {
					t.Fatalf("Write(%q) = %d, want %d", w, n, len(w))
				}
				total += len(w)
			}
			got, truncated := c.Result()
			if got != tt.want {
				t.Errorf("Result() = %q, want %q", got, tt.want)
			}
			if truncated != tt.truncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.truncated)
			}
			if c.Total() != int64(total) {
				t.Errorf("Total() = %d, want %d", c.Total(), total)
			}
		})
	}
}

func TestBoundedCaptureDefaultsToDefaultMaxOutputBytes(t *testing.T) {
	for _, limit := range []int{0, -1} {
		c := newBoundedCapture(limit)
		if got := c.headCap + c.tailCap; got != DefaultMaxOutputBytes {
			t.Fatalf("newBoundedCapture(%d) capacity = %d, want %d", limit, got, DefaultMaxOutputBytes)
		}
	}
}

func TestBoundedCaptureRingWrapsRepeatedly(t *testing.T) {
	c := newBoundedCapture(6) // head 3, tail 3
	for i := 0; i < 100; i++ {
		if _, err := c.Write([]byte("ab")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got, truncated := c.Result()
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !strings.HasPrefix(got, "aba") {
		t.Errorf("head = %q, want prefix %q", got, "aba")
	}
	if !strings.HasSuffix(got, "bab") {
		t.Errorf("tail of %q, want suffix %q", got, "bab")
	}
}

func TestBoundedCaptureConcurrentWrites(t *testing.T) {
	c := newBoundedCapture(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = c.Write([]byte("xy"))
			}
		}()
	}
	wg.Wait()
	if got, want := c.Total(), int64(8*100*2); got != want {
		t.Fatalf("Total() = %d, want %d", got, want)
	}
	if _, truncated := c.Result(); !truncated {
		t.Fatal("expected truncation")
	}
}

// collectHandler records streamed chunks for assertions.
type collectHandler struct {
	mu     sync.Mutex
	stdout strings.Builder
	stderr strings.Builder
	delay  func()
}

func (h *collectHandler) OnStdout(chunk string) {
	if h.delay != nil {
		h.delay()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stdout.WriteString(chunk)
}

func (h *collectHandler) OnStderr(chunk string) {
	if h.delay != nil {
		h.delay()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stderr.WriteString(chunk)
}

func (h *collectHandler) snapshot() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stdout.String(), h.stderr.String()
}

func TestStreamPumpDeliversInOrder(t *testing.T) {
	h := &collectHandler{}
	p := newStreamPump(h)
	for i := 0; i < 50; i++ {
		p.push(false, "o")
		p.push(true, "e")
	}
	p.close()
	p.close() // close must be idempotent

	out, errOut := h.snapshot()
	if out != strings.Repeat("o", 50) {
		t.Errorf("stdout = %q", out)
	}
	if errOut != strings.Repeat("e", 50) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestStreamPumpDropsBacklogWithNotice(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	h := &collectHandler{delay: func() { once.Do(func() { <-release }) }}
	p := newStreamPump(h)

	// Wedge the dispatcher on the first chunk, then overflow the queue.
	p.push(false, "first")
	chunk := strings.Repeat("x", 64*1024)
	for i := 0; i < 32; i++ {
		p.push(false, chunk)
	}
	close(release)
	p.close()

	out, _ := h.snapshot()
	if !strings.HasPrefix(out, "first") {
		t.Errorf("stdout should start with the first chunk, got %.20q", out)
	}
	if !strings.Contains(out, "bytes of live output dropped") {
		t.Error("expected a drop notice in the stream")
	}
}

func TestStreamPumpIgnoresPushAfterClose(t *testing.T) {
	h := &collectHandler{}
	p := newStreamPump(h)
	p.close()
	p.push(false, "late")
	if out, _ := h.snapshot(); out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
}
