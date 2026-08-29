// Package webclient gives Boop a deliberately narrow door onto the public web.
//
// It provides three things the tool layer builds on:
//
//   - Fetch: retrieve a URL under strict limits (timeout, size, redirects) and
//     hand back a decoded body.
//   - ExtractText: turn HTML into readable plain text plus metadata, which is
//     what actually makes a page useful to a language model.
//   - Search: run a web search through a pluggable backend (DuckDuckGo today).
//
// Everything here is off unless the user turned it on. Reaching out to a third
// party sends the user's text to a server they did not choose, so
// config.NetworkConfig.Enabled gates every call, and a request guard treats
// model-supplied URLs as hostile input rather than as instructions.
package webclient

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind classifies a webclient failure. It exists so the tool layer can
// turn a failure into a message the model can act on ("that host is blocked")
// instead of leaking transport internals, per PROJECT.md §57.
type ErrorKind string

// Failure categories. These map onto the normalized error categories in §57.
const (
	// KindDisabled means outbound web access is switched off in config.
	KindDisabled ErrorKind = "disabled"
	// KindBlocked means the request guard refused the URL: bad scheme,
	// denied domain, or an address in a range Boop will not connect to.
	KindBlocked ErrorKind = "blocked"
	// KindRobotsDenied means robots.txt disallows the path for our agent.
	KindRobotsDenied ErrorKind = "robots_denied"
	// KindTimeout means the request exceeded the configured timeout.
	KindTimeout ErrorKind = "timeout"
	// KindCancelled means the caller's context was cancelled.
	KindCancelled ErrorKind = "cancelled"
	// KindTooLarge means the response exceeded the byte cap on a request
	// where a partial body would be meaningless (robots.txt, search).
	KindTooLarge ErrorKind = "too_large"
	// KindBadStatus means the server answered with a 4xx or 5xx status.
	// The Response is still returned alongside the error.
	KindBadStatus ErrorKind = "bad_status"
	// KindMalformed means the URL, or a response Boop must parse, was not
	// well formed.
	KindMalformed ErrorKind = "malformed"
	// KindUnsupported means a declared charset, encoding or configuration
	// value is one this package cannot handle.
	KindUnsupported ErrorKind = "unsupported"
	// KindTransport covers DNS, connection and TLS failures.
	KindTransport ErrorKind = "transport"
	// KindTooManyRedirects means the redirect chain exceeded MaxRedirects.
	KindTooManyRedirects ErrorKind = "too_many_redirects"
	// KindSearchBlocked means the search backend served a bot challenge or
	// rate-limit page instead of results. Distinguishing it from "no
	// results" matters: one is worth retrying later, the other is not.
	KindSearchBlocked ErrorKind = "search_blocked"
)

// Sentinel errors for errors.Is. Each corresponds to one ErrorKind, so callers
// can write errors.Is(err, webclient.ErrBlocked) without type-asserting.
var (
	ErrDisabled         = errors.New("outbound web access is disabled")
	ErrBlocked          = errors.New("blocked by the network guard")
	ErrRobotsDenied     = errors.New("disallowed by robots.txt")
	ErrTimeout          = errors.New("request timed out")
	ErrCancelled        = errors.New("request cancelled")
	ErrTooLarge         = errors.New("response too large")
	ErrBadStatus        = errors.New("bad HTTP status")
	ErrMalformed        = errors.New("malformed input")
	ErrUnsupported      = errors.New("unsupported")
	ErrTransport        = errors.New("transport failure")
	ErrTooManyRedirects = errors.New("too many redirects")
	ErrSearchBlocked    = errors.New("search backend blocked the request")
)

// sentinels maps each kind to the sentinel that errors.Is should match.
var sentinels = map[ErrorKind]error{
	KindDisabled:         ErrDisabled,
	KindBlocked:          ErrBlocked,
	KindRobotsDenied:     ErrRobotsDenied,
	KindTimeout:          ErrTimeout,
	KindCancelled:        ErrCancelled,
	KindTooLarge:         ErrTooLarge,
	KindBadStatus:        ErrBadStatus,
	KindMalformed:        ErrMalformed,
	KindUnsupported:      ErrUnsupported,
	KindTransport:        ErrTransport,
	KindTooManyRedirects: ErrTooManyRedirects,
	KindSearchBlocked:    ErrSearchBlocked,
}

// EnableHint is the one-line instruction shown whenever a call is refused
// because outbound access is off. It names the exact config key so the user
// does not have to go looking for it.
const EnableHint = "set network.enabled: true in the Boop config to allow it"

// Error is a webclient failure carrying enough context for the tool layer to
// explain itself without dumping internals into the transcript.
type Error struct {
	// Kind is the normalized category.
	Kind ErrorKind
	// Op is the operation that failed: "fetch", "search" or "robots".
	Op string
	// URL is the target, when one is known.
	URL string
	// Status is the HTTP status for KindBadStatus, otherwise zero.
	Status int
	// Message is the human-readable explanation.
	Message string
	// Err is the underlying cause, if any.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("webclient: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.URL != "" {
		b.WriteString(e.URL)
		b.WriteString(": ")
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	} else if s, ok := sentinels[e.Kind]; ok {
		b.WriteString(s.Error())
	} else {
		b.WriteString(string(e.Kind))
	}
	if e.Err != nil && !strings.Contains(e.Message, e.Err.Error()) {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying cause to errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether this error matches the sentinel for its kind, so
// errors.Is(err, ErrBlocked) works regardless of the wrapped cause.
func (e *Error) Is(target error) bool {
	s, ok := sentinels[e.Kind]
	return ok && target == s
}

// KindOf returns the ErrorKind of err, or the empty string if err did not come
// from this package.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// newError builds an *Error with a formatted message.
func newError(kind ErrorKind, op, url string, format string, args ...any) *Error {
	return &Error{Kind: kind, Op: op, URL: url, Message: fmt.Sprintf(format, args...)}
}

// wrapError builds an *Error that keeps cause reachable through errors.Is/As.
func wrapError(kind ErrorKind, op, url string, cause error, format string, args ...any) *Error {
	return &Error{Kind: kind, Op: op, URL: url, Message: fmt.Sprintf(format, args...), Err: cause}
}

// errDisabled is the refusal returned whenever NetworkConfig.Enabled is false.
func errDisabled(op string) *Error {
	return newError(KindDisabled, op, "", "outbound web access is disabled; %s", EnableHint)
}
