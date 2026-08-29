package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
)

// envOf builds a LookupEnv stand-in, so tests never mutate the real process
// environment (which would make them order-dependent under -race).
func envOf(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := pairs[name]
		return v, ok
	}
}

// TestDefaultBindIsLoopback pins §22's default: Boop must not appear on the
// network because somebody enabled the WebUI.
func TestDefaultBindIsLoopback(t *testing.T) {
	cfg := config.Default()
	if cfg.Web.Listen != DefaultListen {
		t.Errorf("config default listen = %q, want %q", cfg.Web.Listen, DefaultListen)
	}
	if cfg.Web.Port != DefaultPort {
		t.Errorf("config default port = %d, want %d", cfg.Web.Port, DefaultPort)
	}

	srv, err := New(Options{Config: cfg, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { shutdownServer(t, srv) })
	if got := srv.Addr(); got != "127.0.0.1:8585" {
		t.Errorf("Addr() = %q, want 127.0.0.1:8585", got)
	}
	if !isLoopbackListen(srv.web.Listen) {
		t.Errorf("default bind %q is not loopback", srv.web.Listen)
	}
}

// TestRefusesInsecureBind is the §23 gate: reaching the network without
// authentication has to be a deliberate act, not a default.
func TestRefusesInsecureBind(t *testing.T) {
	tests := []struct {
		name          string
		listen        string
		auth          config.AuthConfig
		allowOption   bool
		env           map[string]string
		wantErr       bool
		wantErrIs     error
		wantErrSubstr string
	}{
		{name: "loopback without auth is fine", listen: "127.0.0.1"},
		{name: "loopback ipv6 without auth is fine", listen: "::1"},
		{
			name: "all interfaces without auth is refused", listen: "0.0.0.0",
			wantErr: true, wantErrIs: ErrInsecureBind,
		},
		{
			name: "lan address without auth is refused", listen: "192.168.1.10",
			wantErr: true, wantErrIs: ErrInsecureBind,
		},
		{name: "an empty listen falls back to the loopback default", listen: ""},
		{
			name:   "all interfaces with auth is allowed",
			listen: "0.0.0.0",
			auth:   config.AuthConfig{Enabled: true, TokenEnv: "BOOP_TOKEN"},
			env:    map[string]string{"BOOP_TOKEN": "s3cret"},
		},
		{
			name: "explicit option overrides the refusal", listen: "0.0.0.0",
			allowOption: true,
		},
		{
			name: "environment override overrides the refusal", listen: "0.0.0.0",
			env: map[string]string{InsecureBindEnv: "1"},
		},
		{
			name: "falsey environment override does not", listen: "0.0.0.0",
			env:     map[string]string{InsecureBindEnv: "0"},
			wantErr: true, wantErrIs: ErrInsecureBind,
		},
		{
			name:    "auth enabled with an unset token variable refuses to start",
			listen:  "127.0.0.1",
			auth:    config.AuthConfig{Enabled: true, TokenEnv: "BOOP_TOKEN"},
			env:     map[string]string{},
			wantErr: true, wantErrSubstr: "BOOP_TOKEN",
		},
		{
			name:    "auth enabled without a token variable refuses to start",
			listen:  "127.0.0.1",
			auth:    config.AuthConfig{Enabled: true},
			wantErr: true, wantErrSubstr: "token_env",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Web.Listen = tc.listen
			cfg.Web.Auth = tc.auth

			srv, err := New(Options{
				Config:            cfg,
				Logger:            discardLogger(),
				AllowInsecureBind: tc.allowOption,
				LookupEnv:         envOf(tc.env),
			})
			if srv != nil {
				t.Cleanup(func() { shutdownServer(t, srv) })
			}
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("New succeeded, want an error")
			case !tc.wantErr && err != nil:
				t.Fatalf("New: %v", err)
			case err == nil:
				return
			}
			if tc.wantErrIs != nil && !strings.Contains(err.Error(), tc.wantErrIs.Error()) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErrIs)
			}
			if tc.wantErrSubstr != "" && !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErrSubstr)
			}
		})
	}
}

// TestTokenAuth covers every accepted credential channel and every rejection.
func TestTokenAuth(t *testing.T) {
	const token = "correct-horse-battery-staple"

	tests := []struct {
		name       string
		apply      func(*http.Request)
		wantStatus int
	}{
		{
			name:       "no credential is rejected",
			apply:      func(*http.Request) {},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "the right bearer token is accepted",
			apply:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer is matched case-insensitively",
			apply:      func(r *http.Request) { r.Header.Set("Authorization", "bearer "+token) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "a wrong token is rejected",
			apply:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a token that is a prefix of the real one is rejected",
			apply:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token[:10]) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "an empty bearer token is rejected",
			apply:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a non-bearer scheme is rejected",
			apply:      func(r *http.Request) { r.Header.Set("Authorization", "Basic "+token) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "the token subprotocol is accepted",
			apply: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", Subprotocol+", "+tokenSubprotocolPrefix+token)
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a wrong token subprotocol is rejected",
			apply: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", tokenSubprotocolPrefix+"nope")
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "the query parameter is accepted",
			apply:      func(r *http.Request) { r.URL.RawQuery = tokenQueryParam + "=" + token },
			wantStatus: http.StatusOK,
		},
		{
			name:       "a wrong query parameter is rejected",
			apply:      func(r *http.Request) { r.URL.RawQuery = tokenQueryParam + "=nope" },
			wantStatus: http.StatusUnauthorized,
		},
	}

	cfg := config.Default()
	cfg.Web.Auth = config.AuthConfig{Enabled: true, TokenEnv: "BOOP_WEB_TOKEN"}
	srv, err := New(Options{
		Config:    cfg,
		Logger:    discardLogger(),
		LookupEnv: envOf(map[string]string{"BOOP_WEB_TOKEN": token}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { shutdownServer(t, srv) })

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			tc.apply(req)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if rec.Code == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
					t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
				}
				if strings.Contains(rec.Body.String(), token) {
					t.Error("the error body echoes the expected token")
				}
			}
		})
	}
}

// TestAuthDisabledAllowsEverything guards against the token check silently
// staying on (or off) independently of configuration.
func TestAuthDisabledAllowsEverything(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestOriginPolicy checks the allowlist, same-origin detection and every
// shape of origin a browser can produce.
func TestOriginPolicy(t *testing.T) {
	tests := []struct {
		name       string
		allowed    []string
		trustProxy bool
		host       string
		headers    map[string]string
		origin     string
		wantAllow  bool
	}{
		{name: "no origin is left to the caller", host: "127.0.0.1:8585", origin: "", wantAllow: true},
		{name: "same origin is allowed", host: "127.0.0.1:8585", origin: "http://127.0.0.1:8585", wantAllow: true},
		{name: "same origin with default port", host: "boop.local", origin: "http://boop.local:80", wantAllow: true},
		{name: "a different port is not same origin", host: "127.0.0.1:8585", origin: "http://127.0.0.1:5173"},
		{name: "an unlisted origin is refused", host: "127.0.0.1:8585", origin: "https://evil.example"},
		{
			name: "a listed origin is allowed", allowed: []string{"http://localhost:5173"},
			host: "127.0.0.1:8585", origin: "http://localhost:5173", wantAllow: true,
		},
		{
			name: "listing is case-insensitive on the host", allowed: []string{"http://Localhost:5173"},
			host: "127.0.0.1:8585", origin: "http://LOCALHOST:5173", wantAllow: true,
		},
		{
			name: "the scheme must match too", allowed: []string{"https://boop.example"},
			host: "127.0.0.1:8585", origin: "http://boop.example",
		},
		{name: "null origin is refused", host: "127.0.0.1:8585", origin: "null"},
		{name: "a garbage origin is refused", host: "127.0.0.1:8585", origin: "not-an-origin"},
		{name: "a non-http scheme is refused", host: "127.0.0.1:8585", origin: "file://"},
		{
			name: "forwarded headers are ignored without trust",
			host: "127.0.0.1:8585", origin: "https://boop.example",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "boop.example"},
		},
		{
			name: "forwarded headers count when trusted", trustProxy: true,
			host: "127.0.0.1:8585", origin: "https://boop.example",
			headers:   map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "boop.example"},
			wantAllow: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := newOriginPolicy(config.WebConfig{
				AllowedOrigins:      tc.allowed,
				TrustedProxyHeaders: tc.trustProxy,
			})
			if err != nil {
				t.Fatalf("newOriginPolicy: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			_, _, allowed := policy.allowOrigin(req)
			if allowed != tc.wantAllow {
				t.Errorf("allowOrigin(%q) = %v, want %v", tc.origin, allowed, tc.wantAllow)
			}
		})
	}
}

// TestWildcardOriginRejected: §23 names unsafe wildcard CORS explicitly, so a
// "*" in the allowlist must fail loudly instead of being reinterpreted.
func TestWildcardOriginRejected(t *testing.T) {
	for _, pattern := range []string{"*", "http://*.example.com", "https://*"} {
		cfg := config.Default()
		cfg.Web.AllowedOrigins = []string{pattern}
		srv, err := New(Options{Config: cfg, Logger: discardLogger()})
		if srv != nil {
			t.Cleanup(func() { shutdownServer(t, srv) })
		}
		if err == nil {
			t.Errorf("New accepted the wildcard origin %q", pattern)
			continue
		}
		if !strings.Contains(err.Error(), "wildcard") {
			t.Errorf("error for %q = %v, want it to mention a wildcard", pattern, err)
		}
	}
}

// TestCORSRejectsDisallowedOrigin verifies that a hostile page is refused
// outright rather than merely being denied a response header: a cross-origin
// POST does damage whether or not the attacker can read the reply.
func TestCORSRejectsDisallowedOrigin(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.Config.Web.AllowedOrigins = []string{"http://localhost:5173"}
	})

	tests := []struct {
		name       string
		origin     string
		wantStatus int
		wantEcho   string
	}{
		{name: "allowlisted", origin: "http://localhost:5173", wantStatus: http.StatusOK, wantEcho: "http://localhost:5173"},
		{name: "hostile", origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "absent", origin: "", wantStatus: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantEcho {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantEcho)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "*" {
				t.Error("the server emitted a wildcard CORS header")
			}
		})
	}
}

// TestProxyHeadersHonouredOnlyWhenTrusted covers §53: an untrusted client must
// not be able to rewrite its own address in the log by sending a header.
func TestProxyHeadersHonouredOnlyWhenTrusted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.RemoteAddr = "10.0.0.5:44321"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	if got := clientIP(req, false); got != "10.0.0.5" {
		t.Errorf("clientIP untrusted = %q, want the socket address 10.0.0.5", got)
	}
	if got := clientIP(req, true); got != "203.0.113.9" {
		t.Errorf("clientIP trusted = %q, want the forwarded 203.0.113.9", got)
	}
}
