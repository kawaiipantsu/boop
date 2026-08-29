package logging

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Placeholder is substituted for every redacted value. It is deliberately
// constant and obvious so that a redacted log line reads as "something was
// removed here" rather than as a corrupt value.
const Placeholder = "[REDACTED]"

// MinRegisteredSecretLength is the shortest literal RegisterSecret will accept.
//
// Registration performs plain substring replacement, so a one-character or
// empty "secret" would blank out ordinary prose everywhere it appeared. Real
// credentials are long; refusing short ones costs nothing and prevents a
// self-inflicted denial of readability.
const MinRegisteredSecretLength = 6

// pattern is one credential shape and how a match is rewritten.
//
// Most shapes are a straight replacement. A few need a second opinion in Go
// code — see bearerPattern — because RE2 has no lookahead and the distinction
// between a credential and an ordinary word is not always expressible as a
// regular expression.
type pattern struct {
	name string
	re   *regexp.Regexp
	repl string
	// fn, when set, replaces repl and receives the whole subject string.
	fn func(*regexp.Regexp, string) string
}

// apply rewrites every match of p in s.
func (p pattern) apply(s string) string {
	if !p.re.MatchString(s) {
		return s
	}
	if p.fn != nil {
		return p.fn(p.re, s)
	}
	return p.re.ReplaceAllString(s, p.repl)
}

// credentialPatterns matches credential *shapes* wherever they appear, so a
// key pasted into a message ("provider rejected sk-...") is caught even though
// no attribute key looks sensitive.
//
// Order matters: PEM blocks and Bearer headers are matched before the generic
// token shapes so that the whole construct is removed rather than only the
// part inside it. Each pattern is anchored on a distinctive prefix or on
// segment lengths that ordinary prose does not reach, because a redactor that
// mangles normal log lines is its own bug.
var credentialPatterns = []pattern{
	// Full PEM private key blocks, including newlines.
	{
		name: "pem_private_key",
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		repl: Placeholder,
	},
	// A truncated PEM block (BEGIN with no END) is still a leaked key, so
	// everything from the header onwards goes.
	{
		name: "pem_private_key_unterminated",
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*`),
		repl: Placeholder,
	},
	// Authorization: Bearer <token>. The scheme is kept because knowing that
	// an Authorization header was present is useful and not sensitive.
	{
		name: "bearer",
		re:   regexp.MustCompile(`(?i)\b(bearer)\s+([A-Za-z0-9\-._~+/]{8,}=*)`),
		fn:   redactBearer,
	},
	// JWTs: header.payload.signature. Real tokens start with the base64url of
	// `{"`, i.e. eyJ; the signature may be empty for alg=none.
	{
		name: "jwt",
		re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`),
		repl: Placeholder,
	},
	// Generic three-segment base64url token. The 20-character minimum per
	// segment keeps dotted identifiers, package paths and version strings out.
	{
		name: "jwt_generic",
		re:   regexp.MustCompile(`\b[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\b`),
		repl: Placeholder,
	},
	// OpenAI / Anthropic style: sk-, sk-proj-, sk-ant-...
	{
		name: "sk_key",
		re:   regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
		repl: Placeholder,
	},
	// xAI.
	{
		name: "xai_key",
		re:   regexp.MustCompile(`\bxai-[A-Za-z0-9_-]{8,}`),
		repl: Placeholder,
	},
	// GitHub personal access / OAuth / server / refresh tokens.
	{
		name: "github_token",
		re:   regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`),
		repl: Placeholder,
	},
	{
		name: "github_pat",
		re:   regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		repl: Placeholder,
	},
	// AWS access key IDs (long-term AKIA, temporary ASIA).
	{
		name: "aws_access_key_id",
		re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		repl: Placeholder,
	},
	// Google API keys.
	{
		name: "google_api_key",
		re:   regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{20,}`),
		repl: Placeholder,
	},
}

// secretRegistry holds literal secrets the application knows at runtime.
//
// Shape matching cannot cover a self-hosted gateway key that looks like an
// ordinary word, but the app resolves every API key from the environment at
// startup and can simply say "never log this exact string".
type secretRegistry struct {
	mu       sync.RWMutex
	values   map[string]struct{}
	replacer *strings.Replacer
}

var secrets = &secretRegistry{values: make(map[string]struct{})}

// RegisterSecret records a literal value that must never appear in a log line.
//
// It is safe for concurrent use and idempotent. Values shorter than
// MinRegisteredSecretLength (after trimming whitespace) are ignored, so
// registering an unset environment variable cannot turn every log line into
// placeholders. Registration is global because the redactor sits behind a
// package-level handler that any goroutine may reach.
func RegisterSecret(value string) {
	RegisterSecrets(value)
}

// RegisterSecrets registers several literals in one lock acquisition, which is
// what startup does once every provider's key has been resolved.
func RegisterSecrets(values ...string) {
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	changed := false
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < MinRegisteredSecretLength {
			continue
		}
		if _, ok := secrets.values[v]; ok {
			continue
		}
		secrets.values[v] = struct{}{}
		changed = true
	}
	if changed {
		secrets.rebuildLocked()
	}
}

// ClearSecrets forgets every registered literal. It exists for tests and for
// configuration reloads that replace the whole provider set.
func ClearSecrets() {
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	secrets.values = make(map[string]struct{})
	secrets.replacer = nil
}

// RegisteredSecretCount reports how many literals are registered. It never
// exposes the values themselves, so it is safe to surface in status output
// (§54) as evidence that redaction is armed.
func RegisteredSecretCount() int {
	secrets.mu.RLock()
	defer secrets.mu.RUnlock()
	return len(secrets.values)
}

// rebuildLocked recreates the replacer. Longest values come first so that a
// key which contains a shorter registered value is replaced as a whole.
func (r *secretRegistry) rebuildLocked() {
	list := make([]string, 0, len(r.values))
	for v := range r.values {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	pairs := make([]string, 0, len(list)*2)
	for _, v := range list {
		pairs = append(pairs, v, Placeholder)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// replace substitutes every registered literal found in s.
func (r *secretRegistry) replace(s string) string {
	r.mu.RLock()
	rep := r.replacer
	r.mu.RUnlock()
	if rep == nil {
		return s
	}
	return rep.Replace(s)
}

// RedactString removes registered literals and known credential shapes from s.
//
// It is exported because §45 requires redaction from logs, the WebUI, crash
// reports and transcripts, and all four should use the same rules rather than
// growing their own near-miss copies.
func RedactString(s string) string {
	if s == "" {
		return s
	}
	s = secrets.replace(s)
	for _, p := range credentialPatterns {
		s = p.apply(s)
	}
	return s
}

// redactBearer removes the token from an Authorization: Bearer header while
// leaving the scheme in place.
//
// The regexp alone would also match prose such as "bearer authentication is
// supported", so the captured token is checked by tokenLike first. Mangling
// ordinary sentences is a real bug, not a harmless excess of caution.
func redactBearer(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		sub := re.FindStringSubmatch(match)
		if len(sub) != 3 || !tokenLike(sub[2]) {
			return match
		}
		return sub[1] + " " + Placeholder
	})
}

// tokenLike reports whether s looks like an opaque credential rather than an
// English word.
//
// Any digit or token punctuation settles it. A run of pure letters only counts
// when it mixes case and is long, which no ordinary word following "bearer"
// is. The deliberate blind spot is a bearer token of at least eight but fewer
// than sixteen lowercase letters; such a token would be far too weak to be
// real, and the alternative is redacting "bearer authentication".
func tokenLike(s string) bool {
	var hasUpper, hasLower bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			return true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		default:
			// -, _, ., ~, +, / and = never occur inside a word.
			return true
		}
	}
	return hasUpper && hasLower && len(s) >= 16
}

// sensitiveKeys are attribute names whose value is a credential by definition.
// Keys are compared after normalisation (lowercased, separators stripped), so
// "API-Key", "api_key" and "apikey" all land on the same entry.
var sensitiveKeys = map[string]struct{}{
	"apikey":        {},
	"apitoken":      {},
	"accesstoken":   {},
	"authorization": {},
	"bearer":        {},
	"clientsecret":  {},
	"cookie":        {},
	"credential":    {},
	"credentials":   {},
	"idtoken":       {},
	"password":      {},
	"passwd":        {},
	"privatekey":    {},
	"refreshtoken":  {},
	"secret":        {},
	"sessiontoken":  {},
	"setcookie":     {},
	"token":         {},
	"xapikey":       {},
}

// sensitiveSuffixes catch namespaced variants such as openai_api_key,
// provider.access_token or gh_credential without resorting to a substring
// match, which would also fire on innocent keys.
var sensitiveSuffixes = []string{
	"apikey",
	"apitoken",
	"accesstoken",
	"authorization",
	"clientsecret",
	"credential",
	"password",
	"privatekey",
	"refreshtoken",
	"secret",
}

// keySeparators are stripped before comparison.
var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")

// normalizeKey lowercases a key and removes separators.
func normalizeKey(key string) string {
	return keySeparators.Replace(strings.ToLower(key))
}

// SensitiveKey reports whether an attribute name denotes a credential.
//
// Note what is intentionally absent: bare "key" (map keys), "auth" (often a
// bool or a mode name) and anything ending in "tokens" — Boop logs token
// *counts* (prompt_tokens, max_tokens, total_tokens) constantly, and blanking
// those would destroy the statistics in §28 while protecting nothing.
func SensitiveKey(key string) bool {
	n := normalizeKey(key)
	if n == "" {
		return false
	}
	if _, ok := sensitiveKeys[n]; ok {
		return true
	}
	for _, suffix := range sensitiveSuffixes {
		if len(n) > len(suffix) && strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// credentialShaped reports whether a value kind could carry a credential.
//
// Numbers, booleans, durations and timestamps cannot, and skipping them is the
// second half of the token-count defence: even if a key called "token" holds
// an integer, it is a count, not a secret.
func credentialShaped(v slog.Value) bool {
	switch v.Kind() {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool,
		slog.KindDuration, slog.KindTime:
		return false
	default:
		return true
	}
}

// redactHandler is the slog.Handler middleware that enforces §45.
type redactHandler struct {
	next slog.Handler
}

// Redact wraps a handler so that no record reaching it can carry a credential.
//
// What it catches:
//
//   - Any attribute whose key looks sensitive (see [SensitiveKey]), at any
//     nesting depth, including inside groups.
//   - Any registered literal (see [RegisterSecret]) or known credential shape
//     (see [RedactString]) appearing in the message, in a string attribute, in
//     a []byte, in an error's text, in a fmt.Stringer's text, or in the keys
//     and values of map and slice attributes.
//   - Values behind slog.LogValuer, which are resolved before inspection.
//   - Arbitrary values passed to slog.Any: every rendering a sink could emit
//     (%+v, encoding.TextMarshaler, json.Marshaler) is scanned; if a secret is
//     found the attribute is replaced by the redacted rendering, losing the
//     original type but not the safety.
//
// What it cannot catch, stated honestly:
//
//   - A secret that a type reveals only through a rendering none of the three
//     above produce — for instance a custom slog.Handler that reflects over
//     the value in its own way.
//   - A credential with no recognisable shape, under an innocuous key, that
//     was never registered — for example a self-hosted gateway key that is a
//     bare word. Register it.
//   - Secrets that are encoded or split before logging (base64 of a key, a key
//     assembled from two attributes).
//   - Anything written to the log file by code that bypasses this handler.
//
// The middleware must be the outermost handler so that Logger.With and
// Logger.WithGroup also pass through it; [New] arranges that.
func Redact(next slog.Handler) slog.Handler {
	if next == nil {
		return nil
	}
	return &redactHandler{next: next}
}

// Enabled delegates: redaction never changes which records are emitted.
func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

// Handle redacts the message and every attribute, then forwards the record.
func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, RedactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs redacts bound attributes once, at binding time, rather than on
// every record that inherits them.
func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	safe := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		safe[i] = redactAttr(a)
	}
	return &redactHandler{next: h.next.WithAttrs(safe)}
}

// WithGroup delegates. Group names are structural, not values, so they are
// left alone; attributes inside the group are still redacted by key and value.
func (h *redactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &redactHandler{next: h.next.WithGroup(name)}
}

// redactAttr returns a copy of a with any credential removed.
func redactAttr(a slog.Attr) slog.Attr {
	// Resolve LogValuer first: the interesting value is the resolved one, and
	// resolving twice (here and in the sink) is both wasteful and unsafe for
	// one-shot valuers.
	a.Value = a.Value.Resolve()

	if a.Value.Kind() == slog.KindGroup {
		if SensitiveKey(a.Key) {
			// A group under a sensitive name is a credential bundle; keep the
			// key so the reader knows it was there, drop everything inside.
			return slog.String(a.Key, Placeholder)
		}
		members := a.Value.Group()
		safe := make([]slog.Attr, len(members))
		for i, m := range members {
			safe[i] = redactAttr(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(safe...)}
	}

	if SensitiveKey(a.Key) && credentialShaped(a.Value) {
		return slog.String(a.Key, Placeholder)
	}

	a.Value = redactValue(a.Value)
	return a
}

// redactValue scans a non-group value for credential shapes.
func redactValue(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if r := RedactString(s); r != s {
			return slog.StringValue(r)
		}
		return v
	case slog.KindAny:
		if out, changed := redactAny(v.Any()); changed {
			return slog.AnyValue(out)
		}
		return v
	default:
		return v
	}
}

// redactAny inspects an arbitrary value carried by slog.Any.
//
// The common container types are walked structurally so that a sensitive key
// inside, say, a header map is caught by name and not only by shape. Anything
// else falls back to a %+v rendering: if the rendering contains a secret the
// attribute becomes that redacted text, and if it does not the original value
// is returned untouched so JSON output keeps its structure and typing.
func redactAny(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case string:
		if r := RedactString(t); r != t {
			return r, true
		}
		return v, false
	case []byte:
		s := string(t)
		if r := RedactString(s); r != s {
			return r, true
		}
		return v, false
	case error:
		s := t.Error()
		if r := RedactString(s); r != s {
			return r, true
		}
		return v, false
	case []string:
		out := make([]string, len(t))
		changed := false
		for i, item := range t {
			out[i] = RedactString(item)
			changed = changed || out[i] != item
		}
		if changed {
			return out, true
		}
		return v, false
	case map[string]string:
		out := make(map[string]string, len(t))
		changed := false
		for k, item := range t {
			switch {
			case SensitiveKey(k):
				out[k] = Placeholder
				changed = changed || item != Placeholder
			default:
				out[k] = RedactString(item)
				changed = changed || out[k] != item
			}
		}
		if changed {
			return out, true
		}
		return v, false
	case map[string]any:
		out := make(map[string]any, len(t))
		changed := false
		for k, item := range t {
			if SensitiveKey(k) {
				out[k] = Placeholder
				changed = true
				continue
			}
			red, c := redactAny(item)
			out[k] = red
			changed = changed || c
		}
		if changed {
			return out, true
		}
		return v, false
	case []any:
		out := make([]any, len(t))
		changed := false
		for i, item := range t {
			red, c := redactAny(item)
			out[i] = red
			changed = changed || c
		}
		if changed {
			return out, true
		}
		return v, false
	case fmt.Stringer:
		s := t.String()
		if r := RedactString(s); r != s {
			return r, true
		}
		return scanRendered(v)
	default:
		return scanRendered(v)
	}
}

// scanRendered is the last-resort check for opaque values, typically structs
// handed to slog.Any.
//
// It inspects every rendering a sink could plausibly emit — see renderings —
// and only rewrites the attribute when a secret is actually present, so
// ordinary structured values keep their type and their JSON shape.
func scanRendered(v any) (any, bool) {
	for _, rendered := range renderings(v) {
		if r := RedactString(rendered); r != rendered {
			return r, true
		}
	}
	return v, false
}

// renderings returns the text forms a handler might produce for v.
//
// Checking only %+v would not be enough: slog's text handler prefers
// encoding.TextMarshaler and its JSON handler prefers json.Marshaler, and a
// type is free to expose through those what its %+v rendering hides. Scanning
// all of them costs one marshal per opaque attribute and closes the gap.
// Marshalers are assumed side-effect free, which is the same assumption the
// handler itself makes when it calls them.
func renderings(v any) []string {
	out := make([]string, 0, 3)
	out = append(out, fmt.Sprintf("%+v", v))
	if m, ok := v.(encoding.TextMarshaler); ok {
		if b, err := m.MarshalText(); err == nil {
			out = append(out, string(b))
		}
	}
	if m, ok := v.(json.Marshaler); ok {
		if b, err := m.MarshalJSON(); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}
