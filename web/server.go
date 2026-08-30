package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/agent"
	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/stats"
)

// Default bind behaviour (§22). Loopback is not a suggestion: it is what keeps
// a tool that can run shell commands off the network until asked otherwise.
const (
	// DefaultListen is the default bind address.
	DefaultListen = "127.0.0.1"
	// DefaultPort is the WebUI port.
	DefaultPort = 8585
)

// Timeouts for the HTTP server. ReadHeaderTimeout bounds a slow-loris
// handshake; there is no WriteTimeout because a hijacked WebSocket lives far
// longer than any sane response deadline.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownGrace     = 5 * time.Second
)

// Options configures a Server.
type Options struct {
	// App is the assembled Boop runtime. The API endpoints that touch
	// sessions, models, tools or the event bus require it; without one the
	// server still starts and serves static assets and /api/status, which is
	// what makes the security behaviour testable in isolation.
	App *app.App
	// Config overrides App.Config. One of Config or App is required.
	Config *config.Config
	// Broker serves the approval queue (§50). The API refuses approval
	// operations rather than inventing a second approval path when it is nil.
	Broker *permissions.Broker
	// Stats supplies the /api/stats snapshot. When it is nil the tracker the
	// runtime already owns (App.Stats) is used, so /api/stats works without
	// every caller remembering to pass one; only when there is neither does
	// the endpoint fall back to the persisted per-session usage totals.
	Stats *stats.Tracker

	// Agents builds the agent fleet for a session. Nil uses
	// agent.NewFromApp, which returns nil when agents are disabled.
	//
	// It is injectable so the API can be tested against a deterministic task
	// runner: the production coordinator drives real models, and §41 forbids
	// tests that need one.
	Agents func(*app.App, string) *agent.Coordinator

	// Listen and Port override the configured bind address, for `boop --web
	// --listen ... --port ...`. Zero values keep the configuration.
	Listen string
	Port   int
	// Listener, when set, is served instead of binding Listen:Port. It exists
	// for socket activation and for tests that need an ephemeral port; the
	// bind-safety check in §23 is applied to its address, not to the
	// configuration, so supplying one cannot smuggle past the check.
	Listener net.Listener

	// AllowInsecureBind permits a non-loopback bind with authentication
	// disabled. It corresponds to an explicit user decision; see
	// InsecureBindEnv for the environment equivalent.
	AllowInsecureBind bool

	// Logger receives startup banners and connection errors. Nil logs to
	// stderr; use io.Discard in tests.
	Logger *log.Logger

	// LookupEnv overrides os.LookupEnv, so tests can supply a token without
	// mutating the process environment.
	LookupEnv func(string) (string, bool)
	// SaveConfig persists a configuration accepted by PUT /api/config. Nil
	// uses (*config.Config).Save, which writes the platform config file.
	SaveConfig func(*config.Config) error
	// Now overrides the clock, for deterministic tests.
	Now func() time.Time
}

// Server is the WebUI HTTP and WebSocket server.
//
// It owns no application state of its own beyond the currently selected
// session and the set of connected clients; everything else is read from, or
// delegated to, the runtime in Options.App.
type Server struct {
	app     *app.App
	cfg     *config.Config
	web     config.WebConfig
	broker  *permissions.Broker
	stats   *stats.Tracker
	log     *log.Logger
	auth    *authenticator
	origins *originPolicy
	hub     *hub

	handler    http.Handler
	httpSrv    *http.Server
	lookupEnv  func(string) (string, bool)
	saveConfig func(*config.Config) error
	now        func() time.Time
	startedAt  time.Time

	baseCtx    context.Context
	baseCancel context.CancelFunc

	// unsubscribe detaches the bus bridge; approvalsDone reports that the
	// approval forwarder has stopped.
	unsubscribe  func()
	approvalDone chan struct{}

	mu             sync.Mutex
	started        bool
	listener       net.Listener
	serveErr       chan error
	currentSession string
	running        map[string]context.CancelFunc
	restart        bool

	// agentMu guards the fleet bookkeeping. It is separate from mu because a
	// coordinator call can take a while and must not block a status request.
	agentMu     sync.Mutex
	fleets      map[string]*agent.Coordinator
	agentRuns   map[string]*agentRun
	fleetClosed bool
	newFleet    func(*app.App, string) *agent.Coordinator

	// project caches the last discovery result; see handleProject.
	projectMu   sync.Mutex
	projectInfo *projectCacheEntry

	shutdownOnce sync.Once
	shutdownErr  error
}

// New builds a Server and applies §23's start-up safety checks.
//
// It returns an error rather than a warning for a configuration that is unsafe
// to serve: an unauthenticated bind beyond loopback, an unresolvable access
// token, or a wildcard origin. Everything that is risky but deliberate is
// logged instead.
func New(opts Options) (*Server, error) {
	cfg := opts.Config
	if cfg == nil && opts.App != nil {
		cfg = opts.App.Config
	}
	if cfg == nil {
		return nil, errors.New("web: a configuration is required")
	}

	webCfg := cfg.Web
	if strings.TrimSpace(webCfg.Listen) == "" {
		webCfg.Listen = DefaultListen
	}
	if webCfg.Port == 0 {
		webCfg.Port = DefaultPort
	}
	if l := strings.TrimSpace(opts.Listen); l != "" {
		webCfg.Listen = l
	}
	if opts.Port != 0 {
		webCfg.Port = opts.Port
	}
	if webCfg.Port < 0 || webCfg.Port > 65535 {
		return nil, fmt.Errorf("web: port %d is out of range (want 0-65535)", webCfg.Port)
	}
	if opts.Listener != nil {
		host, port := splitListenAddr(opts.Listener.Addr().String())
		webCfg.Listen, webCfg.Port = host, port
	}

	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	saveConfig := opts.SaveConfig
	if saveConfig == nil {
		saveConfig = func(c *config.Config) error { return c.Save() }
	}

	allowInsecure := opts.AllowInsecureBind
	if !allowInsecure {
		if v, ok := lookupEnv(InsecureBindEnv); ok && isTruthy(v) {
			allowInsecure = true
		}
	}
	if err := checkBindSafety(webCfg, allowInsecure); err != nil {
		return nil, err
	}

	auth, err := newAuthenticator(webCfg.Auth, lookupEnv)
	if err != nil {
		return nil, err
	}
	origins, err := newOriginPolicy(webCfg)
	if err != nil {
		return nil, err
	}

	// The runtime already owns a tracker; an explicit one only overrides it.
	tracker := opts.Stats
	if tracker == nil && opts.App != nil {
		tracker = opts.App.Stats
	}
	newFleet := opts.Agents
	if newFleet == nil {
		newFleet = agent.NewFromApp
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		app:          opts.App,
		cfg:          cfg,
		web:          webCfg,
		broker:       opts.Broker,
		stats:        tracker,
		log:          logger,
		auth:         auth,
		origins:      origins,
		hub:          newHub(),
		lookupEnv:    lookupEnv,
		saveConfig:   saveConfig,
		now:          now,
		startedAt:    now(),
		baseCtx:      ctx,
		baseCancel:   cancel,
		running:      make(map[string]context.CancelFunc),
		fleets:       make(map[string]*agent.Coordinator),
		agentRuns:    make(map[string]*agentRun),
		newFleet:     newFleet,
		approvalDone: make(chan struct{}),
		serveErr:     make(chan error, 1),
		listener:     opts.Listener,
	}
	s.handler = s.routes()
	s.httpSrv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          logger,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	s.bridgeBus()
	s.bridgeApprovals()

	for _, w := range bindWarnings(webCfg) {
		s.warn(w)
	}
	if allowInsecure && !isLoopbackListen(webCfg.Listen) && !webCfg.Auth.Enabled {
		s.warn(InsecureBindEnv + " is set: the safety check that would have refused this bind was skipped")
	}
	return s, nil
}

// isTruthy reads an environment override permissively but not carelessly:
// "0", "false" and "" mean off.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// warn emits a prominent, hard-to-miss log banner.
//
// A one-line notice about an exposed unauthenticated service scrolls past;
// §22 asks for a clear warning, so it gets one.
func (s *Server) warn(msg string) {
	s.log.Printf("!!! WARNING: %s", msg)
}

// Handler returns the HTTP handler, for httptest and for embedding the WebUI
// behind another mux.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the address the server is listening on, which is only known
// after Start. It is the resolved address, so a configured port of 0 reports
// the port the kernel chose.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return net.JoinHostPort(s.web.Listen, strconv.Itoa(s.web.Port))
	}
	return s.listener.Addr().String()
}

// URL returns the browsable address of the WebUI.
func (s *Server) URL() string {
	addr := s.Addr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	// 0.0.0.0 and :: are bind addresses, not addresses to visit.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Start binds the listener and serves in the background.
//
// Binding happens synchronously so that a port clash is reported to the caller
// instead of appearing later on a log line nobody reads, and so Addr is
// meaningful as soon as Start returns.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("web: the server is already started")
	}
	s.started = true
	ln := s.listener
	if ln == nil {
		addr := net.JoinHostPort(s.web.Listen, strconv.Itoa(s.web.Port))
		var err error
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			s.started = false
			s.mu.Unlock()
			return fmt.Errorf("web: cannot bind %s: %w", addr, err)
		}
		s.listener = ln
	}
	s.mu.Unlock()

	s.log.Printf("boop WebUI listening on %s", s.URL())
	go func() {
		err := s.httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.serveErr <- err
	}()
	return nil
}

// Run starts the server and blocks until ctx is cancelled, then shuts down
// cleanly (§58).
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case err := <-s.serveErr:
		if err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	return s.Shutdown(shutdownCtx)
}

// Shutdown stops the server, closing WebSocket connections cleanly first.
//
// Order matters (§58). http.Server.Shutdown does not wait for hijacked
// connections, and a WebSocket is hijacked, so the sockets are closed with a
// going-away status and their pumps drained before the HTTP server is asked to
// stop. Otherwise a connected browser would see a truncated TCP connection
// instead of a close frame.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		s.cancelTurns()
		// Before the hub closes, so the cancelled status of every worker
		// still reaches connected clients.
		s.stopFleets(ctx)
		s.hub.shutdown(ctx)
		s.baseCancel()
		<-s.approvalDone

		err := s.httpSrv.Shutdown(ctx)
		if err != nil {
			// A deadline here means a client would not let go; closing hard is
			// better than leaking the listener into the next run.
			_ = s.httpSrv.Close()
		}
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		if started {
			if serveErr := <-s.serveErr; serveErr != nil && err == nil {
				err = serveErr
			}
		}
		s.shutdownErr = err
	})
	return s.shutdownErr
}

// cancelTurns stops every model turn started through the API.
func (s *Server) cancelTurns() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.running))
	for _, cancel := range s.running {
		cancels = append(cancels, cancel)
	}
	s.running = make(map[string]context.CancelFunc)
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// bridgeBus forwards core events to every connected client (§25).
//
// The bus publishes synchronously on the caller's goroutine, so this handler
// must never block: it marshals once and hands the bytes to the hub, which
// only ever does non-blocking sends.
func (s *Server) bridgeBus() {
	if s.app == nil || s.app.Bus == nil {
		return
	}
	s.unsubscribe = s.app.Bus.Subscribe(func(ev app.Event) {
		s.hub.broadcastEvent(ev)
	})
}

// bridgeApprovals mirrors approval queue changes to clients so the WebUI, TUI
// and CLI show the same pending list (§50).
func (s *Server) bridgeApprovals() {
	if s.broker == nil {
		close(s.approvalDone)
		return
	}
	events, cancel := s.broker.Subscribe()
	go func() {
		defer close(s.approvalDone)
		defer cancel()
		for {
			select {
			case <-s.baseCtx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				s.hub.broadcastApproval(ev)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// routes builds the mux. Method dispatch happens inside each handler rather
// than in the pattern so that a wrong method produces the same JSON error
// envelope as everything else instead of net/http's plain-text 405.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	api := map[string]http.HandlerFunc{
		"/api/status":    s.handleStatus,
		"/api/config":    s.handleConfig,
		"/api/models":    s.handleModels,
		"/api/providers": s.handleProviders,
		"/api/agents":    s.handleAgents,
		"/api/agents/":   s.handleAgentByID,
		"/api/sessions":  s.handleSessions,
		"/api/session":   s.handleSession,
		"/api/stats":     s.handleStats,
		"/api/tools":     s.handleTools,
		"/api/message":   s.handleMessage,
		"/api/approval":  s.handleApproval,
		"/api/project":   s.handleProject,
		"/api/project/":  s.handleProjectSub,
	}
	for path, handler := range api {
		mux.Handle(path, s.secured(handler))
	}
	// Anything else under /api is a client bug; answer in the API's own
	// vocabulary rather than falling through to the SPA fallback.
	mux.Handle("/api/", s.secured(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "no such API endpoint: "+r.URL.Path)
	}))

	base := strings.TrimRight(s.web.BasePath, "/")
	if base != "" && !strings.HasPrefix(base, "/") {
		base = "/" + base
	}

	// The WebSocket runs its own, stricter origin and token checks because a
	// browser cannot set an Authorization header on an upgrade.
	mux.Handle(EventsPath, s.recovered(s.handleEvents))
	mux.Handle("/", s.recovered(newStaticHandler(base).ServeHTTP))

	var rootHandler http.Handler = mux

	if base != "" {
		prefixMux := http.NewServeMux()
		prefixMux.Handle(base+"/", http.StripPrefix(base, mux))
		prefixMux.Handle(base, http.RedirectHandler(base+"/", http.StatusMovedPermanently))
		rootHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, base) {
				prefixMux.ServeHTTP(w, r)
			} else {
				mux.ServeHTTP(w, r)
			}
		})
	}

	if s.web.TrustedProxyHeaders {
		inner := rootHandler
		rootHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fwdPrefix := r.Header.Get("X-Forwarded-Prefix"); fwdPrefix != "" && base == "" {
				fwdPrefix = "/" + strings.Trim(fwdPrefix, "/")
				if strings.HasPrefix(r.URL.Path, fwdPrefix) {
					http.StripPrefix(fwdPrefix, mux).ServeHTTP(w, r)
					return
				}
			}
			inner.ServeHTTP(w, r)
		})
	}

	return rootHandler
}

// secured wraps an API handler with panic recovery, origin enforcement, CORS
// and token authentication, in that order.
func (s *Server) secured(h http.HandlerFunc) http.Handler {
	return s.recovered(func(w http.ResponseWriter, r *http.Request) {
		if !s.applyCORS(w, r) {
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := s.auth.authenticate(r); err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="boop"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"a valid access token is required; send it as `Authorization: Bearer <token>`")
			return
		}
		h(w, r)
	})
}

// recovered turns a handler panic into a 500 instead of killing the process.
// A crashing WebUI must not take the agent runtime down with it.
func (s *Server) recovered(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Printf("web: panic serving %s %s from %s: %v",
					r.Method, r.URL.Path, clientIP(r, s.web.TrustedProxyHeaders), rec)
				writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
			}
		}()
		h(w, r)
	})
}

// applyCORS validates the Origin and sets the response headers.
//
// There is no wildcard: the header echoes the caller's origin only when that
// origin is same-origin or explicitly allowed (§23). A disallowed origin is
// refused outright rather than merely being denied a CORS header, because a
// state-changing POST does not need to read the response to have done damage.
//
// A request with no Origin at all is allowed through: that is curl, a health
// check, or Boop's own CLI, none of which a browser can be tricked into
// impersonating. The WebSocket upgrade is stricter; see handleEvents.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Origin")
	origin, present, allowed := s.origins.allowOrigin(r)
	if !allowed {
		writeError(w, http.StatusForbidden, codeForbidden,
			"origin "+origin+" is not allowed; add it to web.allowed_origins")
		return false
	}
	if !present {
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	return true
}

// ---------------------------------------------------------------------------
// Shared runtime state
// ---------------------------------------------------------------------------

// CurrentSession returns the session the WebUI is currently attached to.
func (s *Server) CurrentSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentSession
}

// setCurrentSession records the session subsequent requests default to.
func (s *Server) setCurrentSession(id string) {
	s.mu.Lock()
	s.currentSession = id
	s.mu.Unlock()
}

// beginTurn registers an in-flight turn for a session and reports whether it
// was accepted. One turn per session at a time: two concurrent turns would
// interleave tool calls and transcript writes on the same conversation.
func (s *Server) beginTurn(sessionID string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.running[sessionID]; busy {
		return false
	}
	s.running[sessionID] = cancel
	return true
}

// endTurn clears the in-flight marker for a session.
func (s *Server) endTurn(sessionID string) {
	s.mu.Lock()
	delete(s.running, sessionID)
	s.mu.Unlock()
}

// cancelTurn interrupts the running turn for a session (§51).
func (s *Server) cancelTurn(sessionID string) bool {
	s.mu.Lock()
	cancel, ok := s.running[sessionID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// splitListenAddr breaks a listener address into host and port, tolerating an
// address that does not parse rather than losing the whole configuration.
func splitListenAddr(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}
