package fixtures

import (
	"io"
	"net/http"
)

// readBody reads a request body that [Server.ServeHTTP] has already captured
// and replayed onto the request.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// orDefault returns v, or fallback when v is empty.
func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// strptr returns a pointer to s, used where a JSON field must serialize as an
// explicit empty string rather than being omitted.
func strptr(s string) *string { return &s }

// splitN splits s into at most n non-empty pieces of roughly equal rune
// length. Splitting on runes rather than bytes keeps each fragment valid
// UTF-8, so JSON-encoding a fragment never mangles it.
//
// An empty s yields no fragments at all, which is what lets a tool call with
// no arguments stream cleanly.
func splitN(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 1 {
		return []string{s}
	}
	runes := []rune(s)
	if n > len(runes) {
		n = len(runes)
	}
	out := make([]string, 0, n)
	size := len(runes) / n
	rem := len(runes) % n
	pos := 0
	for i := 0; i < n; i++ {
		take := size
		if i < rem {
			take++
		}
		out = append(out, string(runes[pos:pos+take]))
		pos += take
	}
	return out
}

// splitAt cuts s after n runes, returning the head and the remainder.
func splitAt(s string, n int) (head, tail string) {
	runes := []rune(s)
	if n >= len(runes) {
		return s, ""
	}
	return string(runes[:n]), string(runes[n:])
}
