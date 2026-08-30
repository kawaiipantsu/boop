package webclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/config"
)

// Default bounds applied when the configuration leaves a field at zero.
const (
	// DefaultMinHostInterval is the minimum gap between two requests to the
	// same host. One request per second is the conventional politeness bar
	// for an unattended client; robots.txt Crawl-delay can raise it.
	DefaultMinHostInterval = time.Second
	// DefaultMaxCrawlDelay caps how long a robots.txt Crawl-delay may make
	// Boop wait. A hostile or careless value of 3600 must not hang a tool
	// call; past this point the request is refused instead.
	DefaultMaxCrawlDelay = 30 * time.Second
	// defaultAccept is sent on Fetch. HTML first, but plain text and JSON
	// are perfectly good answers.
	defaultAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5"
)

// Response is the outcome of a single fetch.
type Response struct {
	// RequestURL is the URL as requested, before redirects.
	RequestURL string
	// FinalURL is the URL the body actually came from.
	FinalURL string
	// Status is the HTTP status code.
	Status int
	// StatusText is the HTTP status line text.
	StatusText string
	// Header is the response header set.
	Header http.Header
	// ContentType is the media type without parameters, lowercased.
	ContentType string
	// Charset is the encoding the body was decoded from, when known.
	Charset string
	// Body is the raw body after content-decoding (gzip) and truncation.
	Body []byte
	// Text is Body decoded to UTF-8. Empty when CharsetSupported is false.
	Text string
	// CharsetSupported reports whether Text is meaningful. When false, the
	// declared charset is one Boop cannot decode and callers must not
	// pretend Text is the page: say so instead.
	CharsetSupported bool
	// Truncated reports that the body hit MaxResponseBytes and the tail was
	// discarded.
	Truncated bool
	// Redirects lists the URLs followed, in order, excluding RequestURL.
	Redirects []string
	// Duration is the wall time of the request.
	Duration time.Duration
}

// Option customises a Client. Options exist mainly so tests can remove the
// politeness delay and inject a resolver; production uses the config alone.
type Option func(*Client)

// WithLookupIP overrides hostname resolution in the request guard.
func WithLookupIP(fn LookupIPFunc) Option {
	return func(c *Client) { c.lookup = fn }
}

// WithMinHostInterval sets the minimum gap between requests to one host.
// Zero disables the delay.
func WithMinHostInterval(d time.Duration) Option {
	return func(c *Client) { c.minInterval = d; c.minIntervalSet = true }
}

// WithRobotsTTL sets how long a parsed robots.txt is cached.
func WithRobotsTTL(d time.Duration) Option {
	return func(c *Client) { c.robotsTTL = d }
}

// WithSearchBackend replaces the search backend, which is how an alternative
// engine is plugged in without touching the client.
func WithSearchBackend(b Backend) Option {
	return func(c *Client) { c.backend = b }
}

// WithClock overrides the time source used by the rate limiter and caches.
func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// Client is Boop's outbound web client.
//
// It is safe for concurrent use. One Client per process is expected: the
// robots.txt cache and the per-host rate limiter only do their job if requests
// share them.
type Client struct {
	cfg            config.NetworkConfig
	ua             string
	agentToken     string
	guard          *Guard
	http           *http.Client
	robots         *robotsCache
	limiter        *hostLimiter
	backend        Backend
	lookup         LookupIPFunc
	minInterval    time.Duration
	minIntervalSet bool
	robotsTTL      time.Duration
	now            func() time.Time
}

// New builds a Client from a NetworkConfig.
//
// It returns an error only for a configuration Boop refuses to run with — today
// that means an invalid User-Agent override. A disabled configuration is not an
// error: the Client is constructed and every call refuses with ErrDisabled, so
// the tool layer can still register the tools and explain why they are off.
func New(cfg config.NetworkConfig, opts ...Option) (*Client, error) {
	ua, err := ResolveUserAgent(cfg.UserAgent)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:         cfg,
		ua:          ua,
		agentToken:  agentToken(ua),
		minInterval: DefaultMinHostInterval,
		robotsTTL:   defaultRobotsTTL,
		now:         time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	c.guard = NewGuardWithLookup(cfg, c.lookup)
	c.limiter = newHostLimiter(c.minInterval, c.now)
	c.robots = newRobotsCache(c.robotsTTL, defaultRobotsMaxEntries, c.now)
	c.http = &http.Client{
		Transport: c.newTransport(),
		Timeout:   0, // per-request contexts carry the deadline instead.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return c.checkRedirect(req, via)
		},
	}
	if c.backend == nil {
		c.backend = newBackend(cfg.Search.Provider, c)
	}
	return c, nil
}

// newTransport builds the transport. Environment proxies are ignored on
// purpose: an HTTP_PROXY would otherwise forward a request to an address the
// guard has just refused, turning the proxy into the SSRF bypass.
func (c *Client) newTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   c.guard.control,
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Compression is handled explicitly so the Accept-Encoding header
		// and the decompression stay in one place.
		DisableCompression: true,
	}
}

// UserAgent returns the User-Agent this client sends.
func (c *Client) UserAgent() string { return c.ua }

// Enabled reports whether outbound web access is switched on.
func (c *Client) Enabled() bool { return c.cfg.Enabled }

// Config returns the network configuration in force.
func (c *Client) Config() config.NetworkConfig { return c.cfg }

// Guard returns the request guard, so a caller can pre-check a URL (for
// example when rendering a permission prompt) without issuing a request.
func (c *Client) Guard() *Guard { return c.guard }

// timeout returns the per-request timeout.
func (c *Client) timeout() time.Duration {
	if d := c.cfg.Timeout.Std(); d > 0 {
		return d
	}
	return config.DefaultNetworkTimeout
}

// maxBytes returns the response cap.
func (c *Client) maxBytes() int64 {
	if c.cfg.MaxResponseBytes > 0 {
		return c.cfg.MaxResponseBytes
	}
	return config.DefaultMaxResponseBytes
}

// maxRedirects returns the redirect cap. A negative value disables following
// redirects entirely; zero falls back to the default.
func (c *Client) maxRedirects() int {
	if c.cfg.MaxRedirects != 0 {
		return c.cfg.MaxRedirects
	}
	return config.DefaultMaxRedirects
}

// Fetch retrieves rawURL and returns the decoded response.
//
// Failures are normalized onto the ErrorKind categories. A 4xx or 5xx status
// yields ErrBadStatus *and* a non-nil Response, because the status line and
// body of an error page are often exactly what the model needs to see.
//
// A body larger than MaxResponseBytes is not an error: it is truncated and
// Response.Truncated is set.
func (c *Client) Fetch(ctx context.Context, rawURL string) (*Response, error) {
	return c.do(ctx, request{
		op:     "fetch",
		method: http.MethodGet,
		rawURL: rawURL,
		accept: defaultAccept,
	})
}

// Page couples a fetched response with the text extracted from it. It is the
// shape the fetch tool wants: one call, readable text, resolved links.
type Page struct {
	// Response is the underlying HTTP result.
	Response *Response
	// Document is the extracted text and metadata. For a non-HTML response
	// the Text field holds the decoded body unchanged.
	Document Document
}

// FetchPage fetches rawURL and extracts readable text from it.
//
// HTML is run through ExtractText; other textual content types are passed
// through as-is. A binary response yields a Page whose Document.Text is empty,
// with the media type reported in Response.ContentType.
func (c *Client) FetchPage(ctx context.Context, rawURL string) (*Page, error) {
	resp, err := c.Fetch(ctx, rawURL)
	if resp == nil {
		return nil, err
	}
	page := &Page{Response: resp}
	switch {
	case isHTMLType(resp.ContentType) && resp.CharsetSupported:
		page.Document = ExtractText(resp.Text, resp.FinalURL)
	case resp.CharsetSupported && isTextualType(resp.ContentType):
		page.Document = Document{Text: resp.Text, Truncated: resp.Truncated}
	}
	if resp.Truncated {
		page.Document.Truncated = true
	}
	return page, err
}

// request describes one outbound call. It is internal: Fetch and Search are the
// public entry points, and both funnel through do so the guard, robots check,
// rate limit and size cap cannot be skipped by accident.
type request struct {
	op          string
	method      string
	rawURL      string
	body        []byte
	contentType string
	accept      string
	header      http.Header
	// skipRobots omits the robots.txt check. Used for robots.txt itself and
	// for the search endpoint (see Search).
	skipRobots bool
	// maxBytes overrides the configured cap for this request.
	maxBytes int64
	// strictSize turns truncation into ErrTooLarge, for responses where a
	// partial body is worse than none (robots.txt, search result pages).
	strictSize bool
}

// redirectChainKey carries the per-request redirect log through the context,
// since CheckRedirect is set once on the shared http.Client.
type redirectChainKey struct{}

type redirectChain struct {
	mu   sync.Mutex
	urls []string
}

func (r *redirectChain) add(u string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, u)
}

func (r *redirectChain) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

// checkRedirect enforces the redirect cap and re-runs the guard on each hop.
//
// Re-checking is the whole point: a public URL that 302s to
// http://169.254.169.254/latest/meta-data/ is the classic SSRF bypass, and the
// guard's answer for the first URL says nothing about the second.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	max := c.maxRedirects()
	if max < 0 {
		return http.ErrUseLastResponse
	}
	if len(via) > max {
		return newError(KindTooManyRedirects, "fetch", req.URL.Redacted(),
			"stopped after %d redirects (network.max_redirects)", max)
	}
	if err := c.guard.CheckURL(req.Context(), req.URL); err != nil {
		return err
	}
	if chain, ok := req.Context().Value(redirectChainKey{}).(*redirectChain); ok {
		chain.add(req.URL.Redacted())
	}
	req.Header.Set("User-Agent", c.ua)
	return nil
}

// do performs a guarded request end to end.
func (c *Client) do(ctx context.Context, r request) (*Response, error) {
	if !c.cfg.Enabled {
		return nil, errDisabled(r.op)
	}
	u, err := c.guard.CheckRawURL(ctx, r.rawURL)
	if err != nil {
		return nil, retagOp(err, r.op)
	}
	target := u.Redacted()

	if c.cfg.RespectRobots && !r.skipRobots {
		if err := c.checkRobots(ctx, u); err != nil {
			return nil, err
		}
	}
	if err := c.limiter.wait(ctx, u.Host); err != nil {
		return nil, classifyContextErr(ctx, err, r.op, target)
	}

	timeout := c.timeout()
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	chain := &redirectChain{}
	reqCtx = context.WithValue(reqCtx, redirectChainKey{}, chain)

	var bodyReader io.Reader
	if len(r.body) > 0 {
		// A *bytes.Reader lets http.NewRequest populate GetBody, so a
		// redirected POST can be replayed instead of silently losing its body.
		bodyReader = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(reqCtx, r.method, u.String(), bodyReader)
	if err != nil {
		return nil, wrapError(KindMalformed, r.op, target, err, "cannot build request")
	}
	for k, vs := range r.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", c.ua)
	if r.accept != "" {
		req.Header.Set("Accept", r.accept)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	if req.Header.Get("Accept-Language") == "" {
		req.Header.Set("Accept-Language", "en;q=0.9,*;q=0.5")
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}

	started := c.now()
	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, c.classifyDoErr(ctx, err, r.op, target)
	}
	defer func() { _ = httpResp.Body.Close() }()

	limit := r.maxBytes
	if limit <= 0 {
		limit = c.maxBytes()
	}
	body, truncated, err := readCapped(httpResp, limit)
	if err != nil {
		return nil, c.classifyDoErr(ctx, err, r.op, target)
	}
	if truncated && r.strictSize {
		return nil, newError(KindTooLarge, r.op, target,
			"response exceeds the %d byte limit and cannot be used partially", limit)
	}

	resp := &Response{
		RequestURL: target,
		FinalURL:   httpResp.Request.URL.Redacted(),
		Status:     httpResp.StatusCode,
		StatusText: httpResp.Status,
		Header:     httpResp.Header,
		Body:       body,
		Truncated:  truncated,
		Redirects:  chain.list(),
		Duration:   c.now().Sub(started),
	}
	ctype, declared := parseContentType(httpResp.Header.Get("Content-Type"))
	resp.ContentType = ctype
	if isTextualType(ctype) {
		resp.Text, resp.Charset, resp.CharsetSupported = decodeBody(body, declared, ctype)
		if !resp.CharsetSupported && resp.Charset == "" {
			resp.Charset = normalizeCharset(declared)
		}
	} else {
		resp.Charset = normalizeCharset(declared)
	}

	if httpResp.StatusCode >= 400 {
		return resp, &Error{
			Kind:    KindBadStatus,
			Op:      r.op,
			URL:     resp.FinalURL,
			Status:  httpResp.StatusCode,
			Message: "server returned " + httpResp.Status,
		}
	}
	return resp, nil
}

// readCapped reads at most limit bytes, decompressing gzip, and reports
// whether the body was cut short. io.LimitReader is what keeps an endless
// response from exhausting memory; the cap is applied to the decompressed
// stream, since that is what ends up in the model's context.
func readCapped(resp *http.Response, limit int64) (body []byte, truncated bool, err error) {
	var reader io.Reader = resp.Body
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		zr, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			return nil, false, wrapError(KindMalformed, "fetch", resp.Request.URL.Redacted(), gzErr,
				"response claims gzip encoding but is not readable")
		}
		defer func() { _ = zr.Close() }()
		reader = zr
	default:
		return nil, false, newError(KindUnsupported, "fetch", resp.Request.URL.Redacted(),
			"unsupported Content-Encoding %q", enc)
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// parseContentType splits a Content-Type header into a lowercased media type
// and its charset parameter, tolerating malformed values.
func parseContentType(v string) (mediaType, charset string) {
	if v == "" {
		return "", ""
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil {
		// Salvage the type before the first ";" rather than lose it.
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
		return mt, ""
	}
	return strings.ToLower(mt), params["charset"]
}

// classifyDoErr turns a transport failure into a typed error.
func (c *Client) classifyDoErr(ctx context.Context, err error, op, target string) error {
	// Errors raised by CheckRedirect and by the dialer Control hook come back
	// wrapped in *url.Error; recover ours so the kind survives.
	var werr *Error
	if errors.As(err, &werr) {
		return retagOp(werr, op)
	}
	return classifyContextErr(ctx, err, op, target)
}

// classifyContextErr distinguishes "the caller cancelled" from "we ran out of
// time", which the model should be told differently.
func classifyContextErr(ctx context.Context, err error, op, target string) error {
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
		return wrapError(KindCancelled, op, target, err, "cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return wrapError(KindTimeout, op, target, err, "timed out")
	case errors.Is(err, context.Canceled):
		return wrapError(KindCancelled, op, target, err, "cancelled")
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return wrapError(KindTimeout, op, target, err, "timed out")
	}
	return wrapError(KindTransport, op, target, err, "request failed")
}

// retagOp relabels an error's operation while keeping its kind, so a guard
// refusal raised during a search reads as a search failure.
func retagOp(err error, op string) error {
	var werr *Error
	if errors.As(err, &werr) && werr.Op != op {
		clone := *werr
		clone.Op = op
		return &clone
	}
	return err
}

// hostLimiter enforces a minimum interval between requests to the same host.
// Being unattended is not an excuse for hammering a server.
type hostLimiter struct {
	mu        sync.Mutex
	defaultIv time.Duration
	intervals map[string]time.Duration
	next      map[string]time.Time
	now       func() time.Time
}

// maxLimiterHosts bounds the limiter's bookkeeping so a long session that
// touches thousands of hosts cannot grow it without limit.
const maxLimiterHosts = 1024

func newHostLimiter(interval time.Duration, now func() time.Time) *hostLimiter {
	if now == nil {
		now = time.Now
	}
	return &hostLimiter{
		defaultIv: interval,
		intervals: make(map[string]time.Duration),
		next:      make(map[string]time.Time),
		now:       now,
	}
}

// setInterval raises the interval for one host, as robots.txt Crawl-delay asks.
func (l *hostLimiter) setInterval(host string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d > l.intervals[host] {
		l.intervals[host] = d
	}
}

// wait blocks until the host may be contacted again, or the context ends.
func (l *hostLimiter) wait(ctx context.Context, host string) error {
	l.mu.Lock()
	iv := l.defaultIv
	if h := l.intervals[host]; h > iv {
		iv = h
	}
	now := l.now()
	var delay time.Duration
	if iv > 0 {
		if t, ok := l.next[host]; ok && t.After(now) {
			delay = t.Sub(now)
		}
		if len(l.next) >= maxLimiterHosts {
			l.evictLocked(now)
		}
		l.next[host] = now.Add(delay + iv)
	}
	l.mu.Unlock()

	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// evictLocked drops entries that no longer constrain anything.
func (l *hostLimiter) evictLocked(now time.Time) {
	for h, t := range l.next {
		if !t.After(now) {
			delete(l.next, h)
			delete(l.intervals, h)
		}
	}
	// Still full of live entries: drop an arbitrary one rather than grow.
	if len(l.next) >= maxLimiterHosts {
		for h := range l.next {
			delete(l.next, h)
			delete(l.intervals, h)
			break
		}
	}
}

// hostKey is the limiter and cache key for a URL: scheme plus authority.
func hostKey(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
