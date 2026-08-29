// Package web serves Boop's local WebUI and its HTTP/WebSocket API (§22-§26).
//
// The server is a thin frontend over internal/app: it translates HTTP and
// WebSocket traffic into calls on the same runtime the TUI uses, and forwards
// the same event bus (§2.3, §25). No business logic lives here.
//
// Everything in this file exists because §23 assumes the opposite of the usual
// web-server default: a LAN is not trustworthy, an unauthenticated bind beyond
// loopback is a mistake rather than a convenience, and a browser will happily
// point a hostile page at a loopback socket.
//
// # Reverse proxy deployment (§53)
//
// Boop is a local/LAN service. Public exposure is the proxy's job, not Boop's:
// the proxy terminates TLS, authenticates the public, and rate limits. Boop
// itself must be reachable from the proxy over a trusted private connection.
//
// A proxy in front of Boop must:
//
//   - forward the WebSocket upgrade for GET /api/events, passing through the
//     Connection, Upgrade and Sec-WebSocket-* headers, and disable response
//     buffering and any read timeout shorter than the connection's lifetime;
//   - preserve the browser's Origin header unchanged, since origin validation
//     happens in Boop;
//   - set X-Forwarded-Proto and X-Forwarded-Host to the public scheme and
//     host, and X-Forwarded-For to the client address.
//
// Boop reads none of those X-Forwarded-* headers unless web.trusted_proxy_headers
// is set. They are client-supplied, so trusting them by default would let any
// caller claim to be same-origin or forge its address in the log. With the flag
// set, they determine the origin a request is compared against and the address
// that is logged.
package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/kawaiipantsu/boop/internal/config"
)

// ErrUnauthorized reports a request without a usable credential.
//
// It is deliberately coarse: telling a caller whether a token was absent or
// merely wrong is information they can use.
var ErrUnauthorized = errors.New("web: authentication required")

// ErrInsecureBind reports a refusal to start: the configured bind address is
// reachable from other machines and no authentication is configured.
//
// This is fatal rather than a warning because §23's rule ("never assume a LAN
// is automatically trustworthy") is worthless if the unsafe configuration
// still starts. The user can override it, but only on purpose.
var ErrInsecureBind = errors.New("web: refusing to bind beyond loopback without authentication")

// InsecureBindEnv names the environment variable that overrides ErrInsecureBind.
//
// An environment variable rather than a config key: turning off the last
// safety check should be a decision made at the moment of launch, not
// something that can lie dormant in a config file copied between machines.
const InsecureBindEnv = "BOOP_WEB_ALLOW_INSECURE"

// tokenSubprotocolPrefix carries the access token in the WebSocket
// Sec-WebSocket-Protocol header.
//
// Browsers cannot set arbitrary headers on a WebSocket handshake, so the token
// has to travel either in a subprotocol or in the query string. The
// subprotocol is preferred: query strings are logged by reverse proxies, kept
// in browser history and leaked in Referer headers, whereas the subprotocol
// header is not written to an access log by default. The query parameter is
// supported anyway for clients (and proxies) that cannot manipulate
// subprotocols; see tokenFromRequest.
const tokenSubprotocolPrefix = "boop.token."

// Subprotocol is the plain, token-free subprotocol a client may request.
const Subprotocol = "boop.v1"

// tokenQueryParam carries the access token for clients that cannot set a
// subprotocol. See tokenSubprotocolPrefix for why it is the second choice.
const tokenQueryParam = "access_token"

// authenticator checks bearer credentials for API and WebSocket requests.
//
// The expected token is read once from the environment at construction and
// stored only as a SHA-256 digest, so a heap dump or a stray %v of the server
// cannot surrender it. Comparison is over digests, which is constant time in
// the value and also constant time in the length — comparing the raw strings
// would leak the token length through timing.
type authenticator struct {
	enabled bool
	envName string
	digest  [sha256.Size]byte
}

// newAuthenticator resolves the configured token from the environment.
//
// It fails when authentication is enabled but no token can be resolved:
// starting with authentication that cannot possibly succeed is worse than not
// starting, because the operator believes they are protected.
func newAuthenticator(cfg config.AuthConfig, lookupEnv func(string) (string, bool)) (*authenticator, error) {
	if !cfg.Enabled {
		return &authenticator{}, nil
	}
	name := strings.TrimSpace(cfg.TokenEnv)
	if name == "" {
		return nil, errors.New("web: auth.enabled is true but auth.token_env names no environment variable")
	}
	token, _ := lookupEnv(name)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("web: auth.enabled is true but the environment variable %s is empty or unset", name)
	}
	return &authenticator{
		enabled: true,
		envName: name,
		digest:  sha256.Sum256([]byte(token)),
	}, nil
}

// Enabled reports whether a token is required.
func (a *authenticator) Enabled() bool { return a != nil && a.enabled }

// authenticate reports whether r carries the configured access token.
func (a *authenticator) authenticate(r *http.Request) error {
	if !a.Enabled() {
		return nil
	}
	token, ok := tokenFromRequest(r)
	if !ok {
		return ErrUnauthorized
	}
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(got[:], a.digest[:]) != 1 {
		return ErrUnauthorized
	}
	return nil
}

// tokenFromRequest extracts a token from, in order of preference, the
// Authorization header, the WebSocket subprotocol, and the query string.
func tokenFromRequest(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			if token := strings.TrimSpace(h[len(prefix):]); token != "" {
				return token, true
			}
		}
		return "", false
	}
	if token, ok := subprotocolToken(r); ok {
		return token, true
	}
	if token := strings.TrimSpace(r.URL.Query().Get(tokenQueryParam)); token != "" {
		return token, true
	}
	return "", false
}

// subprotocolToken returns the token offered in Sec-WebSocket-Protocol.
func subprotocolToken(r *http.Request) (string, bool) {
	for _, proto := range requestedSubprotocols(r) {
		if strings.HasPrefix(proto, tokenSubprotocolPrefix) {
			if token := strings.TrimSpace(strings.TrimPrefix(proto, tokenSubprotocolPrefix)); token != "" {
				return token, true
			}
		}
	}
	return "", false
}

// requestedSubprotocols splits the comma-separated Sec-WebSocket-Protocol
// header, which may also appear more than once.
func requestedSubprotocols(r *http.Request) []string {
	var out []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, field := range strings.Split(header, ",") {
			if field = strings.TrimSpace(field); field != "" {
				out = append(out, field)
			}
		}
	}
	return out
}

// negotiateSubprotocol picks the subprotocol to echo back on the handshake.
//
// RFC 6455 requires the server to answer with one of the offered values or
// none at all; echoing the token-bearing value back is correct and is what
// browser clients expect, since the token subprotocol is how they authenticated.
func negotiateSubprotocol(r *http.Request) []string {
	offered := requestedSubprotocols(r)
	for _, proto := range offered {
		if strings.HasPrefix(proto, tokenSubprotocolPrefix) {
			return []string{proto}
		}
	}
	for _, proto := range offered {
		if proto == Subprotocol {
			return []string{Subprotocol}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Origin validation
// ---------------------------------------------------------------------------

// originPolicy decides which browser origins may talk to this server (§23).
//
// There is no wildcard. The only origins accepted are the ones the operator
// listed and the server's own origin, because a page served from anywhere else
// asking a loopback socket to run shell commands is the attack this exists to
// stop.
type originPolicy struct {
	allowed    []string
	trustProxy bool
}

// newOriginPolicy normalises the configured allowlist.
//
// A literal "*" is rejected outright: §23 calls unsafe wildcard CORS out by
// name, and silently downgrading it to "same origin only" would leave the
// operator believing something that is not true.
func newOriginPolicy(cfg config.WebConfig) (*originPolicy, error) {
	p := &originPolicy{trustProxy: cfg.TrustedProxyHeaders}
	for _, raw := range cfg.AllowedOrigins {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		if origin == "*" || strings.Contains(origin, "*") {
			return nil, fmt.Errorf("web.allowed_origins: %q is a wildcard; list every allowed origin explicitly", raw)
		}
		normalised, ok := normalizeOrigin(origin)
		if !ok {
			return nil, fmt.Errorf("web.allowed_origins: %q is not an origin (want scheme://host[:port], for example http://localhost:5173)", raw)
		}
		p.allowed = append(p.allowed, normalised)
	}
	return p, nil
}

// allowOrigin reports whether the Origin of r may be served, and returns the
// normalised origin to echo in Access-Control-Allow-Origin.
//
// An empty Origin is reported as allowed with an empty echo value: the caller
// decides what absence means, because it means different things for a plain
// HTTP request (a non-browser client such as curl or a health check) and for a
// WebSocket upgrade (which a browser always labels).
func (p *originPolicy) allowOrigin(r *http.Request) (origin string, present, allowed bool) {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return "", false, true
	}
	// "null" is what a browser sends from a sandboxed iframe, a data: URL or a
	// file:// page. It is never same-origin and can never be listed.
	if raw == "null" {
		return raw, true, false
	}
	normalised, ok := normalizeOrigin(raw)
	if !ok {
		return raw, true, false
	}
	if normalised == p.selfOrigin(r) {
		return normalised, true, true
	}
	for _, candidate := range p.allowed {
		if candidate == normalised {
			return normalised, true, true
		}
	}
	return normalised, true, false
}

// selfOrigin returns the origin the request was addressed to, which is what
// "same origin" means for a page this server delivered.
func (p *originPolicy) selfOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if p.trustProxy {
		// §53: forwarded headers are honoured only when the operator has
		// declared that a trusted proxy sits in front. Otherwise any client
		// could assert its own origin is ours simply by claiming it.
		if v := firstHeaderValue(r, "X-Forwarded-Proto"); v != "" {
			scheme = strings.ToLower(v)
		}
		if v := firstHeaderValue(r, "X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	normalised, ok := normalizeOrigin(scheme + "://" + host)
	if !ok {
		return ""
	}
	return normalised
}

// normalizeOrigin lowercases scheme and host and drops the default port, so
// http://LocalHost:80 and http://localhost compare equal.
func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if u.Path != "" && u.Path != "/" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port), true
	}
	if strings.Contains(host, ":") { // bare IPv6 literal
		return scheme + "://[" + host + "]", true
	}
	return scheme + "://" + host, true
}

// firstHeaderValue returns the first comma-separated value of a header, which
// is the client-most entry in an X-Forwarded-* chain.
func firstHeaderValue(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// clientIP returns the caller's address for logging.
//
// X-Forwarded-For is consulted only with TrustedProxyHeaders set (§53): it is
// a client-supplied header, so trusting it by default would let anyone write
// whatever they liked into the log.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if v := firstHeaderValue(r, "X-Forwarded-For"); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Bind safety
// ---------------------------------------------------------------------------

// isLoopbackListen reports whether a bind address reaches only this machine.
//
// An empty address means "every interface" to net.Listen, so it is treated as
// non-loopback: the failure mode of guessing wrong here is an exposed server.
func isLoopbackListen(listen string) bool {
	host := strings.TrimSpace(listen)
	if host == "" {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// checkBindSafety refuses configurations that expose an unauthenticated
// WebUI to the network (§23).
//
// The WebUI can run arbitrary commands through the tool runtime, so an
// unauthenticated non-loopback bind is a remote shell for anyone on the
// network. allowInsecure is the deliberate escape hatch.
func checkBindSafety(cfg config.WebConfig, allowInsecure bool) error {
	if isLoopbackListen(cfg.Listen) || cfg.Auth.Enabled || allowInsecure {
		return nil
	}
	return fmt.Errorf(
		"%w: web.listen is %q, which other machines can reach, but web.auth.enabled is false. "+
			"Set web.auth.enabled with web.auth.token_env, bind 127.0.0.1 instead, "+
			"or set %s=1 to accept the risk deliberately",
		ErrInsecureBind, cfg.Listen, InsecureBindEnv)
}

// bindWarnings lists risks that are allowed but worth saying out loud.
func bindWarnings(cfg config.WebConfig) []string {
	if isLoopbackListen(cfg.Listen) {
		return nil
	}
	warnings := []string{fmt.Sprintf(
		"the WebUI is bound to %s: every machine that can route to this host can reach it", cfg.Listen)}
	if !cfg.Auth.Enabled {
		warnings = append(warnings,
			"authentication is DISABLED: anyone who reaches the port can run commands through Boop")
	}
	if len(cfg.AllowedOrigins) == 0 {
		warnings = append(warnings,
			"web.allowed_origins is empty: only same-origin browser requests will be accepted")
	}
	warnings = append(warnings,
		"put a reverse proxy in front of Boop for TLS and public authentication (§53); "+
			"do not expose this port to the Internet directly")
	return warnings
}
