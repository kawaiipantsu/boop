package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// shutdownServer stops a server with a fresh context. It deliberately does not
// use t.Context(), which is already cancelled by the time cleanups run and
// would turn every teardown into a hard close.
func shutdownServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// newTestServer builds a loopback server with no runtime attached, which is
// enough for every security and envelope test.
func newTestServer(t *testing.T, mutate func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Config:     config.Default(),
		Logger:     discardLogger(),
		LookupEnv:  envOf(nil),
		SaveConfig: func(*config.Config) error { return nil },
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { shutdownServer(t, srv) })
	return srv
}

// newTestApp assembles a real runtime against an in-memory database and a
// temporary project root, so the API tests exercise the same wiring the binary
// does rather than a mock of it.
func newTestApp(t *testing.T) *app.App {
	t.Helper()
	cfg := config.Default()
	application, err := app.New(context.Background(), app.Options{
		Config:       cfg,
		WorkingDir:   t.TempDir(),
		DatabasePath: ":memory:",
		Approver:     permissions.DenyAll(),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })
	return application
}

// newRunningServer starts a server on an ephemeral loopback port and returns
// it with its base URL.
func newRunningServer(t *testing.T, mutate func(*Options)) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newTestServer(t, func(o *Options) {
		o.Listener = ln
		if mutate != nil {
			mutate(o)
		}
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return srv, "http://" + ln.Addr().String()
}

// TestStartBindsTheConfiguredLoopbackAddress checks that the listener really
// is loopback-only, not just that the configuration says so.
func TestStartBindsTheConfiguredLoopbackAddress(t *testing.T) {
	srv, base := newRunningServer(t, nil)

	host, _, err := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", base, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("bound %q, want a loopback address", host)
	}
	if srv.Addr() != strings.TrimPrefix(base, "http://") {
		t.Errorf("Addr() = %q, want %q", srv.Addr(), strings.TrimPrefix(base, "http://"))
	}

	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestStartTwiceFails guards the listener against being replaced underneath a
// running server.
func TestStartTwiceFails(t *testing.T) {
	srv, _ := newRunningServer(t, nil)
	if err := srv.Start(); err == nil {
		t.Fatal("the second Start succeeded, want an error")
	}
}

// TestGracefulShutdown checks §58: connected WebSockets get a close frame and
// Shutdown returns promptly rather than waiting on hijacked connections.
func TestGracefulShutdown(t *testing.T) {
	srv, base := newRunningServer(t, nil)

	conn, _, err := websocket.Dial(t.Context(), wsURL(base), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{base}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Drain the hello so the connection is fully established before shutdown.
	if _, err := readServerMessage(t, conn); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if got := srv.hub.count(); got != 1 {
		t.Fatalf("connected clients = %d, want 1", got)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- srv.Shutdown(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}

	// The client should observe a clean close, not a reset connection.
	_, _, readErr := conn.Read(t.Context())
	if readErr == nil {
		t.Fatal("the connection survived shutdown")
	}
	if status := websocket.CloseStatus(readErr); status != websocket.StatusGoingAway {
		t.Errorf("close status = %v, want %v (err %v)", status, websocket.StatusGoingAway, readErr)
	}

	if _, err := http.Get(base + "/api/status"); err == nil {
		t.Error("the listener is still accepting requests after shutdown")
	}
}

// TestShutdownIsIdempotent: teardown paths call it more than once.
func TestShutdownIsIdempotent(t *testing.T) {
	srv := newTestServer(t, nil)
	ctx := context.Background()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestRunStopsWithContext checks the blocking entry point the orchestrator
// wires into cmd/boop.
func TestRunStopsWithContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := New(Options{
		Config:   config.Default(),
		Logger:   discardLogger(),
		Listener: ln,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	waitForServer(t, "http://"+ln.Addr().String()+"/api/status")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// TestStaticPlaceholder covers the degraded path: no frontend bundle must
// still produce a useful page rather than a 404 or a startup failure.
func TestStaticPlaceholder(t *testing.T) {
	srv := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if _, bundled := bundleFS(); !bundled {
		if !strings.Contains(rec.Body.String(), "web-build") {
			t.Error("the placeholder does not say how to build the bundle")
		}
	}
}

// TestUnknownAPIEndpoint keeps the API's error vocabulary consistent instead
// of falling through to the SPA handler.
func TestUnknownAPIEndpoint(t *testing.T) {
	srv := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if env.Error.Code != codeNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, codeNotFound)
	}
}

// waitForServer polls until the server answers or the deadline passes.
func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never became reachable", url)
}

// wsURL converts an http base URL to its ws equivalent.
func wsURL(base string) string {
	return "ws" + strings.TrimPrefix(base, "http") + EventsPath
}
