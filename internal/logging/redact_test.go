package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/logging"
)

// Obvious fakes. Nothing here is or resembles a live credential.
const (
	fakeOpenAIKey = "sk-TESTONLYtestonlyTESTONLYtestonly"
	fakeXAIKey    = "xai-TESTONLYtestonlyTESTONLY"
	fakeGitHubPAT = "ghp_TESTONLYtestonly0123456789"
	fakeGitHubOAu = "gho_TESTONLYtestonly0123456789"
	fakeGitHubFin = "github_pat_TESTONLYtestonly0123456789"
	fakeAWSKey    = "AKIAIOSFODNN7EXAMPLE"
	fakeGoogleKey = "AIzaTESTONLYtestonlyTESTONLYtestonly"
	fakeJWT       = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkJvb3AifQ." +
		"TESTONLYtestonlySIGNATUREvalue0123456789"
	fakeGenericTriple = "TESTONLYtestonlyTESTONLY.testonlyTESTONLYtestonly.ONLYtestonlyTESTONLYtest"
	fakePEM           = "-----BEGIN RSA PRIVATE KEY-----\n" +
		"TESTONLYtestonlyTESTONLYtestonlyTESTONLY\n" +
		"testonlyTESTONLYtestonly\n" +
		"-----END RSA PRIVATE KEY-----"
	fakeLiteralSecret = "hunter2-TESTONLY-gateway-key"
)

func TestRedactStringCredentialShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "openai style sk key",
			in:   "provider rejected " + fakeOpenAIKey,
			want: "provider rejected " + logging.Placeholder,
		},
		{
			name: "anthropic style sk-ant key",
			in:   "key=sk-ant-TESTONLYtestonly0123",
			want: "key=" + logging.Placeholder,
		},
		{
			name: "openai project key",
			in:   "sk-proj-TESTONLYtestonly0123",
			want: logging.Placeholder,
		},
		{
			name: "xai key",
			in:   "using " + fakeXAIKey + " for grok",
			want: "using " + logging.Placeholder + " for grok",
		},
		{
			name: "github personal access token",
			in:   fakeGitHubPAT,
			want: logging.Placeholder,
		},
		{
			name: "github oauth token",
			in:   "Authorization header carried " + fakeGitHubOAu,
			want: "Authorization header carried " + logging.Placeholder,
		},
		{
			name: "github fine grained pat",
			in:   fakeGitHubFin,
			want: logging.Placeholder,
		},
		{
			name: "aws access key id",
			in:   "aws creds " + fakeAWSKey + " leaked",
			want: "aws creds " + logging.Placeholder + " leaked",
		},
		{
			name: "aws temporary access key id",
			in:   "ASIAIOSFODNN7EXAMPLE",
			want: logging.Placeholder,
		},
		{
			name: "google api key",
			in:   fakeGoogleKey,
			want: logging.Placeholder,
		},
		{
			name: "bearer header keeps the scheme",
			in:   "Authorization: Bearer TESTONLYtestonly0123456789",
			want: "Authorization: Bearer " + logging.Placeholder,
		},
		{
			name: "lowercase bearer scheme",
			in:   "authorization: bearer TESTONLY.testonly-0123_456",
			want: "authorization: bearer " + logging.Placeholder,
		},
		{
			name: "bearer carrying a jwt",
			in:   "Bearer " + fakeJWT,
			want: "Bearer " + logging.Placeholder,
		},
		{
			name: "bare jwt",
			in:   "token " + fakeJWT + " expired",
			want: "token " + logging.Placeholder + " expired",
		},
		{
			name: "jwt with empty signature",
			in:   "eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0In0.",
			want: logging.Placeholder,
		},
		{
			name: "generic three segment base64url token",
			in:   fakeGenericTriple,
			want: logging.Placeholder,
		},
		{
			name: "pem private key block",
			in:   "dumping config:\n" + fakePEM,
			want: "dumping config:\n" + logging.Placeholder,
		},
		{
			name: "pem block for an ec key",
			in:   "-----BEGIN EC PRIVATE KEY-----\nTESTONLY\n-----END EC PRIVATE KEY-----",
			want: logging.Placeholder,
		},
		{
			name: "unterminated pem block is redacted to the end",
			in:   "oops -----BEGIN OPENSSH PRIVATE KEY-----\nTESTONLYtestonly",
			want: "oops " + logging.Placeholder,
		},
		{
			name: "several secrets in one line",
			in:   fakeOpenAIKey + " and " + fakeXAIKey,
			want: logging.Placeholder + " and " + logging.Placeholder,
		},
		{
			name: "secret inside a url query",
			in:   "GET /v1/models?api_key=" + fakeOpenAIKey,
			want: "GET /v1/models?api_key=" + logging.Placeholder,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := logging.RedactString(tc.in)
			if got != tc.want {
				t.Errorf("RedactString(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactStringLeavesOrdinaryTextIntact guards the other half of the
// contract: a redactor that mangles normal log lines is its own bug.
func TestRedactStringLeavesOrdinaryTextIntact(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "plain sentence", in: "starting boop v0.1.0-dev on linux/amd64"},
		{name: "listen address", in: "WebUI listening on 127.0.0.1:8585"},
		{name: "token counts", in: "prompt_tokens=1024 completion_tokens=256 total_tokens=1280"},
		{name: "words containing sk", in: "the task-based, risk-averse desk-bound workflow"},
		{name: "bearer used as a word", in: "bearer authentication is supported"},
		{name: "bearer with a short word after it", in: "bearer tokens are configured"},
		{name: "import path", in: "github.com/kawaiipantsu/boop/internal/logging"},
		{name: "dotted identifier", in: "provider.openai.chat.completions"},
		{name: "semver", in: "upgrading from v1.2.3 to v1.3.0"},
		{name: "git sha", in: "commit 9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"},
		{name: "uuid", in: "session 3f2504e0-4f89-11d3-9a0c-0305e82c3301 resumed"},
		{name: "windows path", in: `C:\Users\example\AppData\Local\boop\logs\boop.log`},
		{name: "unix path", in: "/home/example/.local/state/boop/logs/boop.log"},
		{name: "certificate is not a private key", in: "-----BEGIN CERTIFICATE-----\nMIIBTESTONLY\n-----END CERTIFICATE-----"},
		{name: "sk prefix too short to be a key", in: "sk-12"},
		{name: "shell command", in: `run: go test -race ./internal/... -run TestRedact`},
		{name: "json fragment", in: `{"model":"llama3.1:8b","stream":true}`},
		{name: "config reference to an env var", in: "api_key_env: OPENAI_API_KEY"},
		{name: "error text", in: "dial tcp 127.0.0.1:11434: connect: connection refused"},
		{name: "uppercase words", in: "AKIA is a prefix, ASIA is a continent"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := logging.RedactString(tc.in); got != tc.in {
				t.Errorf("RedactString mangled ordinary text\n  in: %q\n out: %q", tc.in, got)
			}
		})
	}
}

func TestRegisterSecret(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)

	if got := logging.RedactString("gateway key " + fakeLiteralSecret); !strings.Contains(got, fakeLiteralSecret) {
		t.Fatalf("unregistered literal should not be redacted by shape, got %q", got)
	}

	logging.RegisterSecret(fakeLiteralSecret)
	got := logging.RedactString("gateway key " + fakeLiteralSecret + " accepted")
	want := "gateway key " + logging.Placeholder + " accepted"
	if got != want {
		t.Errorf("registered literal not redacted\n got: %q\nwant: %q", got, want)
	}

	// Registering twice must not change behaviour or duplicate entries.
	logging.RegisterSecret(fakeLiteralSecret)
	if n := logging.RegisteredSecretCount(); n != 1 {
		t.Errorf("RegisteredSecretCount() = %d, want 1 after a duplicate registration", n)
	}

	logging.ClearSecrets()
	if n := logging.RegisteredSecretCount(); n != 0 {
		t.Errorf("RegisteredSecretCount() = %d after ClearSecrets, want 0", n)
	}
	if got := logging.RedactString(fakeLiteralSecret); got != fakeLiteralSecret {
		t.Errorf("ClearSecrets did not forget the literal, got %q", got)
	}
}

// TestRegisterSecretIgnoresShortValues protects ordinary text from a secret
// that is really an unset environment variable.
func TestRegisterSecretIgnoresShortValues(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)

	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace only", value: "   "},
		{name: "single character", value: "a"},
		{name: "one below the minimum", value: strings.Repeat("x", logging.MinRegisteredSecretLength-1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logging.RegisterSecret(tc.value)
			if n := logging.RegisteredSecretCount(); n != 0 {
				t.Fatalf("RegisterSecret(%q) registered a too-short value", tc.value)
			}
		})
	}

	const ordinary = "a normal message about a task"
	if got := logging.RedactString(ordinary); got != ordinary {
		t.Errorf("short registrations damaged ordinary text: %q", got)
	}

	// Exactly at the minimum length it is honoured.
	atMin := strings.Repeat("x", logging.MinRegisteredSecretLength)
	logging.RegisterSecret(atMin)
	if got := logging.RedactString("value " + atMin); got != "value "+logging.Placeholder {
		t.Errorf("value at MinRegisteredSecretLength not redacted, got %q", got)
	}
}

// TestRegisterSecretConcurrent exercises the registry under -race while the
// redactor reads it, which is exactly how startup and the agent pool overlap.
func TestRegisterSecretConcurrent(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			logging.RegisterSecret(fmt.Sprintf("TESTONLY-secret-%d-padding", i))
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			logging.RedactString("checking TESTONLY-secret-3-padding in flight")
		}()
	}
	wg.Wait()

	if n := logging.RegisteredSecretCount(); n != 8 {
		t.Fatalf("RegisteredSecretCount() = %d, want 8", n)
	}
	got := logging.RedactString("saw TESTONLY-secret-3-padding")
	if strings.Contains(got, "TESTONLY-secret-3-padding") {
		t.Errorf("concurrently registered secret survived redaction: %q", got)
	}
}

func TestSensitiveKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "api_key", key: "api_key", want: true},
		{name: "apikey", key: "apikey", want: true},
		{name: "API-KEY", key: "API-KEY", want: true},
		{name: "x-api-key", key: "x-api-key", want: true},
		{name: "X_API_KEY", key: "X_API_KEY", want: true},
		{name: "token", key: "token", want: true},
		{name: "Token", key: "Token", want: true},
		{name: "access_token", key: "access_token", want: true},
		{name: "refresh-token", key: "refresh_token", want: true},
		{name: "secret", key: "secret", want: true},
		{name: "client_secret", key: "client.secret", want: true},
		{name: "password", key: "password", want: true},
		{name: "Passwd", key: "Passwd", want: true},
		{name: "authorization", key: "Authorization", want: true},
		{name: "cookie", key: "Cookie", want: true},
		{name: "set-cookie", key: "Set-Cookie", want: true},
		{name: "bearer", key: "bearer", want: true},
		{name: "credential", key: "credential", want: true},
		{name: "credentials", key: "credentials", want: true},
		{name: "private_key", key: "private_key", want: true},
		{name: "privateKey", key: "privateKey", want: true},
		{name: "namespaced provider key", key: "openai_api_key", want: true},
		{name: "dotted namespaced key", key: "provider.openai.api-key", want: true},

		{name: "empty", key: "", want: false},
		{name: "plural token count", key: "tokens", want: false},
		{name: "prompt_tokens", key: "prompt_tokens", want: false},
		{name: "max_tokens", key: "max_tokens", want: false},
		{name: "total_tokens", key: "total_tokens", want: false},
		{name: "token_count", key: "token_count", want: false},
		{name: "bare key", key: "key", want: false},
		{name: "map key", key: "keys", want: false},
		{name: "auth mode is not a credential", key: "auth", want: false},
		{name: "session_id", key: "session_id", want: false},
		{name: "model", key: "model", want: false},
		{name: "keyboard", key: "keyboard", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := logging.SensitiveKey(tc.key); got != tc.want {
				t.Errorf("SensitiveKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// stringerSecret hides its secret behind a tidy String() while exposing it
// through MarshalJSON — the worst case for a redactor that only inspects one
// rendering.
type stringerSecret struct{ key string }

func (s stringerSecret) String() string { return "provider(openai)" }

// MarshalJSON deliberately exposes what String() hides.
func (s stringerSecret) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"key": s.key})
}

// textMarshalerSecret exposes its secret only through MarshalText, which is
// what slog's text handler prefers over %+v.
type textMarshalerSecret struct{ key string }

func (s textMarshalerSecret) MarshalText() ([]byte, error) {
	return []byte("key=" + s.key), nil
}

type nestedStruct struct {
	Provider string
	APIKey   string
	Nested   struct{ Token string }
}

func TestHandlerRedaction(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)
	logging.RegisterSecret(fakeLiteralSecret)

	tests := []struct {
		name string
		// log emits one record on the supplied logger.
		log func(*slog.Logger)
		// wantAbsent must not appear anywhere in the rendered line.
		wantAbsent []string
		// wantPresent must appear (usually the placeholder plus context that
		// should have survived).
		wantPresent []string
	}{
		{
			name:        "sensitive key with string value",
			log:         func(l *slog.Logger) { l.Info("configured", "api_key", "anything-at-all") },
			wantAbsent:  []string{"anything-at-all"},
			wantPresent: []string{logging.Placeholder, "configured"},
		},
		{
			name:        "sensitive key spelled with a hyphen",
			log:         func(l *slog.Logger) { l.Info("header", "X-Api-Key", "anything-at-all") },
			wantAbsent:  []string{"anything-at-all"},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name:        "authorization header value",
			log:         func(l *slog.Logger) { l.Info("request", "Authorization", "Basic dGVzdG9ubHk6dGVzdA==") },
			wantAbsent:  []string{"dGVzdG9ubHk6dGVzdA"},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name:        "set-cookie",
			log:         func(l *slog.Logger) { l.Info("response", "set-cookie", "boop_session=abc; HttpOnly") },
			wantAbsent:  []string{"boop_session=abc"},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name:        "credential shape in the message",
			log:         func(l *slog.Logger) { l.Error("auth failed for " + fakeOpenAIKey) },
			wantAbsent:  []string{fakeOpenAIKey, "sk-TESTONLY"},
			wantPresent: []string{logging.Placeholder, "auth failed for"},
		},
		{
			name:        "credential shape in an innocuous string attribute",
			log:         func(l *slog.Logger) { l.Info("provider call", "url", "https://api.x.ai/v1?k="+fakeXAIKey) },
			wantAbsent:  []string{fakeXAIKey},
			wantPresent: []string{logging.Placeholder, "https://api.x.ai/v1"},
		},
		{
			name:        "jwt in an attribute",
			log:         func(l *slog.Logger) { l.Info("session", "cursor", fakeJWT) },
			wantAbsent:  []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name:        "pem block in an attribute",
			log:         func(l *slog.Logger) { l.Warn("loaded identity", "material", fakePEM) },
			wantAbsent:  []string{"BEGIN RSA PRIVATE KEY"},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name:        "registered literal secret anywhere",
			log:         func(l *slog.Logger) { l.Info("gateway accepted " + fakeLiteralSecret) },
			wantAbsent:  []string{fakeLiteralSecret},
			wantPresent: []string{logging.Placeholder, "gateway accepted"},
		},
		{
			name:        "registered literal under an innocuous key",
			log:         func(l *slog.Logger) { l.Info("call", "endpoint", "https://gw/"+fakeLiteralSecret) },
			wantAbsent:  []string{fakeLiteralSecret},
			wantPresent: []string{logging.Placeholder, "https://gw/"},
		},
		{
			name: "secret nested in a group",
			log: func(l *slog.Logger) {
				l.Info("provider", slog.Group("openai",
					slog.String("model", "gpt-4o-mini"),
					slog.String("api_key", "anything-at-all"),
				))
			},
			wantAbsent:  []string{"anything-at-all"},
			wantPresent: []string{logging.Placeholder, "gpt-4o-mini"},
		},
		{
			name: "secret nested two groups deep",
			log: func(l *slog.Logger) {
				l.Info("config", slog.Group("providers",
					slog.Group("anthropic",
						slog.String("token", "anything-at-all"),
						slog.Int("timeout_ms", 30000),
					),
				))
			},
			wantAbsent:  []string{"anything-at-all"},
			wantPresent: []string{logging.Placeholder, "30000"},
		},
		{
			name: "group under a sensitive name is dropped whole",
			log: func(l *slog.Logger) {
				l.Info("auth", slog.Group("credentials",
					slog.String("user", "example"),
					slog.String("pass", "anything-at-all"),
				))
			},
			wantAbsent:  []string{"anything-at-all", "example"},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name: "secret bound with With survives into later records",
			log: func(l *slog.Logger) {
				l.With("api_key", "anything-at-all").Info("bound")
			},
			wantAbsent:  []string{"anything-at-all"},
			wantPresent: []string{logging.Placeholder, "bound"},
		},
		{
			name: "secret inside an open group",
			log: func(l *slog.Logger) {
				l.WithGroup("http").With("authorization", "Bearer TESTONLYtestonly0123").Info("outbound")
			},
			wantAbsent:  []string{"TESTONLYtestonly0123"},
			wantPresent: []string{logging.Placeholder, "outbound"},
		},
		{
			name: "struct passed to slog.Any",
			log: func(l *slog.Logger) {
				v := nestedStruct{Provider: "openai", APIKey: fakeOpenAIKey}
				v.Nested.Token = "plain"
				l.Info("snapshot", slog.Any("cfg", v))
			},
			wantAbsent:  []string{fakeOpenAIKey},
			wantPresent: []string{logging.Placeholder, "openai"},
		},
		{
			name: "pointer to struct passed to slog.Any",
			log: func(l *slog.Logger) {
				l.Info("snapshot", slog.Any("cfg", &nestedStruct{Provider: "xai", APIKey: fakeXAIKey}))
			},
			wantAbsent:  []string{fakeXAIKey},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name: "map attribute keyed by a sensitive name",
			log: func(l *slog.Logger) {
				l.Info("headers", slog.Any("h", map[string]string{
					"Authorization": "Basic dGVzdG9ubHk=",
					"User-Agent":    "boop/0.1.0",
				}))
			},
			wantAbsent:  []string{"dGVzdG9ubHk"},
			wantPresent: []string{logging.Placeholder, "boop/0.1.0"},
		},
		{
			name: "map[string]any nested value",
			log: func(l *slog.Logger) {
				l.Info("payload", slog.Any("body", map[string]any{
					"model": "llama3.1:8b",
					"auth":  map[string]any{"token": "anything-at-all"},
				}))
			},
			wantAbsent:  []string{"anything-at-all"},
			wantPresent: []string{logging.Placeholder, "llama3.1:8b"},
		},
		{
			name: "slice of strings",
			log: func(l *slog.Logger) {
				l.Info("argv", slog.Any("args", []string{"curl", "-H", "Authorization: Bearer TESTONLYtestonly01"}))
			},
			wantAbsent:  []string{"TESTONLYtestonly01"},
			wantPresent: []string{logging.Placeholder, "curl"},
		},
		{
			name: "error value",
			log: func(l *slog.Logger) {
				l.Error("request failed", slog.Any("err", errors.New("401 for key "+fakeOpenAIKey)))
			},
			wantAbsent:  []string{fakeOpenAIKey},
			wantPresent: []string{logging.Placeholder, "401 for key"},
		},
		{
			name: "LogValuer is resolved before inspection",
			log: func(l *slog.Logger) {
				l.Info("lazy", slog.Any("v", lazySecret{}))
			},
			wantAbsent:  []string{fakeGitHubPAT},
			wantPresent: []string{logging.Placeholder},
		},
		{
			name:        "token count under a plural key is untouched",
			log:         func(l *slog.Logger) { l.Info("usage", "prompt_tokens", 1024, "total_tokens", 1280) },
			wantAbsent:  []string{logging.Placeholder},
			wantPresent: []string{"1024", "1280"},
		},
		{
			name:        "numeric value under a sensitive key is a count, not a secret",
			log:         func(l *slog.Logger) { l.Info("usage", "token", 42) },
			wantAbsent:  []string{logging.Placeholder},
			wantPresent: []string{"42"},
		},
		{
			name:        "ordinary record is untouched",
			log:         func(l *slog.Logger) { l.Info("model loaded", "model", "llama3.1:8b", "duration_ms", 312) },
			wantAbsent:  []string{logging.Placeholder},
			wantPresent: []string{"model loaded", "llama3.1:8b", "312"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{
				Level:  logging.LevelTrace,
				Format: logging.FormatJSON,
				Writer: &buf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tc.log(lg.Logger)

			line := buf.String()
			if !json.Valid(bytes.TrimSpace([]byte(line))) {
				t.Fatalf("handler produced invalid JSON: %q", line)
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(line, s) {
					t.Errorf("output must not contain %q\ngot: %s", s, line)
				}
			}
			for _, s := range tc.wantPresent {
				if !strings.Contains(line, s) {
					t.Errorf("output must contain %q\ngot: %s", s, line)
				}
			}
		})
	}
}

// lazySecret leaks through slog.LogValuer, which the redactor resolves first.
type lazySecret struct{}

func (lazySecret) LogValue() slog.Value { return slog.StringValue(fakeGitHubPAT) }

// TestRedactionAppliesToBothFormats checks the middleware sits outside the
// formatter, so text output is protected exactly like JSON output.
func TestRedactionAppliesToBothFormats(t *testing.T) {
	for _, format := range []logging.Format{logging.FormatText, logging.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{Format: format, Writer: &buf})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			lg.Info("call", "api_key", "anything-at-all", "note", fakeOpenAIKey)
			out := buf.String()
			if strings.Contains(out, "anything-at-all") || strings.Contains(out, fakeOpenAIKey) {
				t.Errorf("%s output leaked a secret: %s", format, out)
			}
		})
	}
}

// TestDisableRedactionIsOptIn proves the tests above are testing the redactor
// and not some accident of formatting.
func TestDisableRedactionIsOptIn(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{
		Format:           logging.FormatJSON,
		Writer:           &buf,
		DisableRedaction: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("raw", "api_key", "anything-at-all")
	if !strings.Contains(buf.String(), "anything-at-all") {
		t.Errorf("DisableRedaction did not disable redaction: %s", buf.String())
	}
}

// TestRedactionReachesCustomMarshalers covers values whose secret is only
// visible through the rendering the sink happens to prefer.
func TestRedactionReachesCustomMarshalers(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)

	tests := []struct {
		name   string
		format logging.Format
		value  any
		secret string
	}{
		{
			name:   "json marshaler hidden behind a tidy String",
			format: logging.FormatJSON,
			value:  stringerSecret{key: fakeOpenAIKey},
			secret: fakeOpenAIKey,
		},
		{
			name:   "text marshaler",
			format: logging.FormatText,
			value:  textMarshalerSecret{key: fakeXAIKey},
			secret: fakeXAIKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{Format: tc.format, Writer: &buf})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			lg.Info("opaque", slog.Any("provider", tc.value))
			if strings.Contains(buf.String(), tc.secret) {
				t.Errorf("secret survived via a custom marshaler: %s", buf.String())
			}
			if !strings.Contains(buf.String(), logging.Placeholder) {
				t.Errorf("expected a placeholder in %s", buf.String())
			}
		})
	}
}

// TestRedactionKnownLimitations pins the documented blind spots so that the
// doc comment on Redact and the behaviour cannot drift apart. Each case also
// shows the remedy: register the literal.
func TestRedactionKnownLimitations(t *testing.T) {
	const shapeless = "correct-horse-battery-staple"
	// base64("sk-TESTONLYtestonly") — encoded before logging, so no shape.
	const encoded = "c2stVEVTVE9OTFl0ZXN0b25seQ=="

	tests := []struct {
		name  string
		value string
	}{
		{name: "credential with no recognisable shape", value: shapeless},
		{name: "credential encoded before logging", value: encoded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logging.ClearSecrets()
			t.Cleanup(logging.ClearSecrets)

			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			lg.Info("gateway", "note", tc.value)
			if !strings.Contains(buf.String(), tc.value) {
				t.Fatalf("redaction now catches %q; update the Redact doc comment", tc.value)
			}

			// Documented remedy.
			logging.RegisterSecret(tc.value)
			buf.Reset()
			lg.Info("gateway", "note", tc.value)
			if strings.Contains(buf.String(), tc.value) {
				t.Errorf("registering the literal must close the gap, got: %s", buf.String())
			}
		})
	}
}
