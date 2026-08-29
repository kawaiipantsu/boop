package webclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/version"
)

func TestDefaultUserAgent(t *testing.T) {
	ua := DefaultUserAgent()
	want := "Boop/" + sanitizeToken(version.Get().Version) + " (+https://github.com/kawaiipantsu/boop)"
	if ua != want {
		t.Fatalf("DefaultUserAgent() = %q, want %q", ua, want)
	}
	if err := ValidateUserAgent(ua); err != nil {
		t.Fatalf("default User-Agent must satisfy its own validator: %v", err)
	}
	if !strings.Contains(strings.ToLower(ua), "boop") {
		t.Fatalf("default User-Agent %q must be attributable to Boop", ua)
	}
}

func TestValidateUserAgent(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		ok   bool
	}{
		{"default", DefaultUserAgent(), true},
		{"bare product", "Boop", true},
		{"product and version", "Boop/1.2.3", true},
		{"prerelease version", "Boop/0.1.0-dev", true},
		{"comment only after product", "Boop/1.0 (+https://example.com/bot)", true},
		{"two products", "Boop/1.0 boop-fetch/2", true},
		{"nested comment", "Boop/1.0 (a (nested) comment)", true},
		{"quoted paren in comment", `Boop/1.0 (smiley \) here)`, true},
		{"tab separator", "Boop/1.0\t(+https://example.com)", true},
		{"mixed case token", "BoOp/1.0", true},
		{"comment with obs-text", "Boop/1.0 (caf\xc3\xa9)", true},

		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"missing boop", "Mozilla/5.0 (compatible; Googlebot/2.1)", false},
		{"leading space", " Boop/1.0", false},
		{"trailing space", "Boop/1.0 ", false},
		{"unbalanced open paren", "Boop/1.0 (unclosed", false},
		{"unbalanced close paren", "Boop/1.0 unopened)", false},
		{"newline injection", "Boop/1.0\r\nX-Evil: 1", false},
		{"bare newline", "Boop/1.0\n(comment)", false},
		{"control char in product", "Boop\x01/1.0", false},
		{"control char in comment", "Boop/1.0 (bad\x07)", false},
		{"null byte", "Boop/1.0\x00", false},
		{"empty version after slash", "Boop/", false},
		{"slash without product", "/1.0 boop", false},
		{"double space separator ok but empty product", "Boop/1.0  ", false},
		{"invalid token char", "Boop/1.0 boop@home", false},
		{"comment without separator", "Boop/1.0(no-space)", false},
		{"too long", "Boop/1.0 (" + strings.Repeat("x", maxUserAgentLen) + ")", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUserAgent(tc.ua)
			if tc.ok && err != nil {
				t.Fatalf("ValidateUserAgent(%q) = %v, want nil", tc.ua, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("ValidateUserAgent(%q) = nil, want an error", tc.ua)
				}
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("ValidateUserAgent(%q) error kind = %q, want malformed", tc.ua, KindOf(err))
				}
			}
		})
	}
}

func TestResolveUserAgent(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
		wantErr  bool
	}{
		{"empty falls back to default", "", DefaultUserAgent(), false},
		{"blank falls back to default", "   ", DefaultUserAgent(), false},
		{"valid override wins", "Boop-Research/2.0 (+https://example.com)", "Boop-Research/2.0 (+https://example.com)", false},
		{"override without boop rejected", "curl/8.0", "", true},
		{"malformed override rejected", "Boop/1.0 (", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveUserAgent(tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveUserAgent(%q) = %q, want error", tc.override, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveUserAgent(%q): %v", tc.override, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveUserAgent(%q) = %q, want %q", tc.override, got, tc.want)
			}
		})
	}
}

func TestAgentToken(t *testing.T) {
	tests := []struct{ ua, want string }{
		{"Boop/0.1.0 (+https://example.com)", "boop"},
		{"Boop", "boop"},
		{"Boop-Research/2.0", "boop-research"},
		{"boop 1.0", "boop"},
	}
	for _, tc := range tests {
		if got := agentToken(tc.ua); got != tc.want {
			t.Errorf("agentToken(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

func TestSanitizeToken(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "0"},
		{"0.1.0-dev", "0.1.0-dev"},
		{"1.0 (weird)", "1.0--weird-"},
		{"v2\n", "v2-"},
	}
	for _, tc := range tests {
		if got := sanitizeToken(tc.in); got != tc.want {
			t.Errorf("sanitizeToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
