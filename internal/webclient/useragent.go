package webclient

import (
	"strings"

	"github.com/boop-dev/boop/internal/version"
)

// UserAgentProduct is the product token Boop identifies itself with. It is also
// the token matched against robots.txt User-agent groups.
const UserAgentProduct = "Boop"

// userAgentURL goes in the default User-Agent comment so a site operator
// reading their logs can find out what is calling and how to stop it.
const userAgentURL = "+https://github.com/boop-dev/boop"

// maxUserAgentLen bounds a configured User-Agent. Nothing legitimate is this
// long, and an unbounded header is a way to abuse a remote log pipeline.
const maxUserAgentLen = 512

// DefaultUserAgent returns the User-Agent Boop sends when the user has not
// configured one:
//
//	Boop/<version> (+https://github.com/boop-dev/boop)
//
// Attributable traffic is the point. A site operator who does not want Boop
// crawling them must be able to identify it in a log line and block it.
func DefaultUserAgent() string {
	return UserAgentProduct + "/" + sanitizeToken(version.Get().Version) + " (" + userAgentURL + ")"
}

// ResolveUserAgent picks the User-Agent for a configuration: the configured
// override if there is one, otherwise DefaultUserAgent.
//
// An override is validated rather than trusted. A malformed header value would
// be silently mangled or rejected by servers, and an override that drops "Boop"
// would make the traffic unattributable — which defeats the reason Boop
// identifies itself at all — so both are refused with an explanatory error.
func ResolveUserAgent(override string) (string, error) {
	ua := strings.TrimSpace(override)
	if ua == "" {
		return DefaultUserAgent(), nil
	}
	if err := ValidateUserAgent(ua); err != nil {
		return "", err
	}
	return ua, nil
}

// ValidateUserAgent checks ua against the RFC 9110 §10.1.5 User-Agent grammar
// and additionally requires it to contain "boop" (case-insensitive).
//
//	User-Agent = product *( RWS ( product / comment ) )
//	product    = token [ "/" product-version ]
//	comment    = "(" *( ctext / quoted-pair / comment ) ")"
//
// Control characters, unbalanced parentheses and empty product tokens are all
// rejected; so is anything without "boop" in it.
func ValidateUserAgent(ua string) error {
	if ua == "" {
		return newError(KindMalformed, "user-agent", "", "User-Agent must not be empty")
	}
	if len(ua) > maxUserAgentLen {
		return newError(KindMalformed, "user-agent", "",
			"User-Agent is %d bytes, limit is %d", len(ua), maxUserAgentLen)
	}
	if ua != strings.TrimSpace(ua) {
		return newError(KindMalformed, "user-agent", "", "User-Agent has leading or trailing whitespace")
	}
	if !strings.Contains(strings.ToLower(ua), "boop") {
		return newError(KindMalformed, "user-agent", "",
			"User-Agent %q must contain %q so the traffic stays attributable", ua, "Boop")
	}

	i, err := parseProduct(ua, 0)
	if err != nil {
		return err
	}
	for i < len(ua) {
		ws := skipRWS(ua, i)
		if ws == i {
			return newError(KindMalformed, "user-agent", "",
				"User-Agent %q: expected whitespace before position %d", ua, i)
		}
		i = ws
		if i >= len(ua) {
			return newError(KindMalformed, "user-agent", "", "User-Agent %q ends with whitespace", ua)
		}
		if ua[i] == '(' {
			i, err = parseComment(ua, i)
		} else {
			i, err = parseProduct(ua, i)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// parseProduct consumes `token [ "/" token ]` starting at i.
func parseProduct(s string, i int) (int, error) {
	start := i
	for i < len(s) && isTChar(s[i]) {
		i++
	}
	if i == start {
		return 0, newError(KindMalformed, "user-agent", "",
			"User-Agent %q: expected a product token at position %d", s, start)
	}
	if i < len(s) && s[i] == '/' {
		i++
		vs := i
		for i < len(s) && isTChar(s[i]) {
			i++
		}
		if i == vs {
			return 0, newError(KindMalformed, "user-agent", "",
				"User-Agent %q: %q must be followed by a version token", s, s[start:vs])
		}
	}
	return i, nil
}

// parseComment consumes a parenthesised comment, including nested comments and
// quoted pairs, starting at the "(" at i.
func parseComment(s string, i int) (int, error) {
	depth := 0
	for i < len(s) {
		switch c := s[i]; {
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
			if depth == 0 {
				return i, nil
			}
		case c == '\\':
			// quoted-pair: backslash followed by HTAB / SP / VCHAR / obs-text.
			if i+1 >= len(s) {
				return 0, newError(KindMalformed, "user-agent", "",
					"User-Agent %q: trailing backslash", s)
			}
			n := s[i+1]
			if n != '\t' && n != ' ' && (n < 0x21 || n == 0x7f) {
				return 0, newError(KindMalformed, "user-agent", "",
					"User-Agent %q: invalid escape at position %d", s, i)
			}
			i += 2
		case isCText(c):
			i++
		default:
			return 0, newError(KindMalformed, "user-agent", "",
				"User-Agent %q: invalid byte %#02x in comment at position %d", s, c, i)
		}
	}
	return 0, newError(KindMalformed, "user-agent", "", "User-Agent %q: unbalanced parenthesis", s)
}

// skipRWS consumes one or more spaces or horizontal tabs.
func skipRWS(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// isTChar reports whether c is an RFC 9110 tchar.
func isTChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// isCText reports whether c may appear literally inside a comment:
// HTAB / SP / %x21-27 / %x2A-5B / %x5D-7E / obs-text (%x80-FF).
func isCText(c byte) bool {
	switch {
	case c == '\t', c == ' ':
		return true
	case c >= 0x21 && c <= 0x27:
		return true
	case c >= 0x2a && c <= 0x5b:
		return true
	case c >= 0x5d && c <= 0x7e:
		return true
	case c >= 0x80:
		return true
	}
	return false
}

// sanitizeToken makes s usable as a product-version token by replacing any
// non-tchar byte with '-'. Build metadata comes from -ldflags and is not
// guaranteed to be header-safe.
func sanitizeToken(s string) string {
	if s == "" {
		return "0"
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if isTChar(s[i]) {
			b.WriteByte(s[i])
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// agentToken returns the lowercased product token of a User-Agent, which is
// what robots.txt groups are matched against.
func agentToken(ua string) string {
	if i := strings.IndexAny(ua, "/ \t"); i > 0 {
		return strings.ToLower(ua[:i])
	}
	return strings.ToLower(ua)
}
