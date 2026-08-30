package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/kawaiipantsu/boop/internal/permissions"

	"github.com/kawaiipantsu/boop/internal/webclient"
)

// ErrBlockedAddress reports a request refused by the SSRF guard.
var ErrBlockedAddress = errors.New("blocked address")

// DefaultHTTPTimeout bounds a single HTTP request end to end.
const DefaultHTTPTimeout = 30 * time.Second

// DefaultMaxResponseBytes caps how much of a response body is read.
const DefaultMaxResponseBytes int64 = 1 << 20

// DefaultMaxRedirects caps the redirect chain. Each hop is re-checked against
// the SSRF guard, so the limit exists to stop loops, not to provide safety.
const DefaultMaxRedirects = 5

// execHTTPMethods is the set of methods the tool will issue. Anything else —
// including CONNECT and TRACE — is refused.
var execHTTPMethods = map[string]permissions.Risk{
	http.MethodGet:     permissions.RiskLow,
	http.MethodHead:    permissions.RiskLow,
	http.MethodOptions: permissions.RiskLow,
	http.MethodPost:    permissions.RiskMedium,
	http.MethodPut:     permissions.RiskMedium,
	http.MethodPatch:   permissions.RiskMedium,
	http.MethodDelete:  permissions.RiskHigh,
}

// HTTPTool performs outbound HTTP requests on the model's behalf.
//
// # SSRF defence
//
// The local machine and the network it sits on are not fair game. By default
// the tool refuses to connect to loopback, unspecified, link-local, multicast,
// carrier-NAT and RFC1918/ULA private addresses. The check runs in the dialer's
// Control hook, so it applies to the address actually being connected to: every
// redirect hop is re-checked, and a hostname that resolves to a private address
// (including a DNS-rebinding answer) is refused at connect time rather than at
// URL-parse time.
//
// Cloud metadata endpoints (169.254.169.254, 169.254.170.2, fd00:ec2::254) are
// refused unconditionally. They are the classic credential-theft target and no
// legitimate tool call needs them, so even the local-development opt-in does
// not unlock them.
//
// Setting AllowPrivateNetworks re-enables private and loopback destinations for
// developing against a local server. It is a Go field set from configuration,
// deliberately not a tool argument: the model must not be able to lift its own
// restriction. Proxies from the environment are ignored for the same reason —
// an HTTP_PROXY could otherwise forward a request to an address the guard just
// rejected.
type HTTPTool struct {
	// Timeout bounds a request when the caller does not specify one.
	Timeout time.Duration
	// MaxTimeout caps a caller-requested timeout.
	MaxTimeout time.Duration
	// MaxResponseBytes caps the body read into memory. Excess is discarded and
	// reported as truncation.
	MaxResponseBytes int64
	// MaxRedirects caps the redirect chain length.
	MaxRedirects int
	// AllowPrivateNetworks opts in to private, loopback and link-local
	// destinations for local development. Cloud metadata stays blocked.
	AllowPrivateNetworks bool
	// UserAgent identifies Boop to servers. It defaults to the same
	// RFC 9110 product token webclient sends, so every outbound request
	// Boop makes is attributable to one recognisable agent.
	UserAgent string

	once      sync.Once
	transport *http.Transport
}

// NewHTTPTool returns an HTTP tool with the safe defaults described on HTTPTool.
func NewHTTPTool() *HTTPTool {
	return &HTTPTool{
		Timeout:          DefaultHTTPTimeout,
		MaxTimeout:       5 * time.Minute,
		MaxResponseBytes: DefaultMaxResponseBytes,
		MaxRedirects:     DefaultMaxRedirects,
		UserAgent:        webclient.DefaultUserAgent(),
	}
}

// HTTPResponse is the structured payload attached to the tool Result.
type HTTPResponse struct {
	RequestURL string            `json:"request_url"`
	FinalURL   string            `json:"final_url"`
	Method     string            `json:"method"`
	StatusCode int               `json:"status_code"`
	Status     string            `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	BodyBytes  int               `json:"body_bytes"`
	Truncated  bool              `json:"truncated"`
	Redirects  []string          `json:"redirects,omitempty"`
	Duration   time.Duration     `json:"duration"`
}

// execHTTPArgs are the decoded arguments of the http tool.
type execHTTPArgs struct {
	Method         string            `json:"method,omitempty"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// Name implements Tool.
func (t *HTTPTool) Name() string { return "http" }

// Description implements Tool.
func (t *HTTPTool) Description() string {
	return "Make an HTTP request and return the status, headers and body. " +
		"Requests to loopback, private and link-local addresses (including cloud metadata " +
		"endpoints) are refused. Error responses such as 404 are returned as data to act on."
}

// Schema implements Tool.
func (t *HTTPTool) Schema() map[string]any {
	methods := make([]string, 0, len(execHTTPMethods))
	for m := range execHTTPMethods {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Absolute http:// or https:// URL.",
			},
			"method": map[string]any{
				"type":        "string",
				"enum":        methods,
				"default":     http.MethodGet,
				"description": "HTTP method. Defaults to GET.",
			},
			"headers": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description":          "Request headers.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Request body for POST, PUT and PATCH.",
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Request timeout in seconds.",
			},
		},
		"required": []string{"url"},
	}
}

// Permission classifies the request.
//
// Risk follows the method: reads are low, writes medium, DELETE high. The
// detail shows the request as it will be sent, with credential-bearing headers
// redacted so an approval prompt never leaks a token into a transcript.
func (t *HTTPTool) Permission(call Call) (permissions.Action, error) {
	var args execHTTPArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	method := execHTTPMethod(args.Method)
	risk, ok := execHTTPMethods[method]
	if !ok {
		risk = permissions.RiskHigh
	}
	target := strings.TrimSpace(args.URL)
	if target == "" {
		return permissions.Action{}, fmt.Errorf("http: url is required")
	}
	return permissions.Action{
		Category: permissions.CatNetworkHTTP,
		Risk:     risk,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("HTTP %s %s", method, execSummarize(target, 120)),
		Detail:   execHTTPDetail(method, target, args.Headers, args.Body),
	}, nil
}

// Execute performs the request.
//
// Transport failures, blocked addresses and HTTP error statuses all come back
// as Result{IsError: true} with the details: a 404 or a 500 is something the
// model can react to, not a reason to abort the exchange.
func (t *HTTPTool) Execute(ctx context.Context, call Call) (Result, error) {
	started := time.Now()
	var args execHTTPArgs
	if err := call.Bind(&args); err != nil {
		return Errorf(call, "http: %v", err), nil
	}
	target := strings.TrimSpace(args.URL)
	if target == "" {
		return Errorf(call, "http: url is required"), nil
	}
	method := execHTTPMethod(args.Method)
	if _, ok := execHTTPMethods[method]; !ok {
		return Errorf(call, "http: method %q is not supported", method), nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return Errorf(call, "http: invalid url %q: %v", target, err), nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Errorf(call, "http: unsupported scheme %q; only http and https are allowed", u.Scheme), nil
	}
	if u.Host == "" {
		return Errorf(call, "http: url %q has no host", target), nil
	}
	if reason, blocked := t.execCheckHost(u.Hostname()); blocked {
		return Errorf(call, "http: refused to request %s — %s.%s", target, reason, execHTTPOptInHint()), nil
	}

	timeout := execTimeout(args.TimeoutSeconds, t.execTimeout(), t.MaxTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if args.Body != "" {
		body = strings.NewReader(args.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return Errorf(call, "http: %v", err), nil
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" && t.UserAgent != "" {
		req.Header.Set("User-Agent", t.UserAgent)
	}

	var redirects []string
	client := &http.Client{
		Transport: t.execTransport(),
		Timeout:   timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > t.execMaxRedirects() {
				return fmt.Errorf("stopped after %d redirects", t.execMaxRedirects())
			}
			if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect to unsupported scheme %q", ErrBlockedAddress, r.URL.Scheme)
			}
			if reason, blocked := t.execCheckHost(r.URL.Hostname()); blocked {
				return fmt.Errorf("%w: redirect to %s — %s", ErrBlockedAddress, r.URL.Redacted(), reason)
			}
			redirects = append(redirects, r.URL.Redacted())
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return Result{
			CallID:   call.ID,
			Tool:     call.Name,
			Content:  execFormatHTTPError(method, target, redirects, time.Since(started), err),
			IsError:  true,
			Duration: time.Since(started),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	limit := t.execMaxBytes()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	truncated := int64(len(raw)) > limit
	if truncated {
		raw = raw[:limit]
	}

	out := HTTPResponse{
		RequestURL: target,
		FinalURL:   resp.Request.URL.Redacted(),
		Method:     method,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Headers:    execHTTPHeaders(resp.Header),
		Body:       string(raw),
		BodyBytes:  len(raw),
		Truncated:  truncated,
		Redirects:  redirects,
		Duration:   time.Since(started),
	}

	content := execFormatHTTPResponse(out)
	if readErr != nil {
		content += fmt.Sprintf("\n[body read failed after %d bytes: %v]", len(raw), readErr)
	}
	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  content,
		Data:     out,
		IsError:  resp.StatusCode >= 400 || readErr != nil,
		Duration: time.Since(started),
	}, nil
}

func (t *HTTPTool) execTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return DefaultHTTPTimeout
}

func (t *HTTPTool) execMaxBytes() int64 {
	if t.MaxResponseBytes > 0 {
		return t.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

func (t *HTTPTool) execMaxRedirects() int {
	if t.MaxRedirects > 0 {
		return t.MaxRedirects
	}
	return DefaultMaxRedirects
}

// execTransport builds the guarded transport once.
//
// Proxy is nil on purpose: honouring HTTP_PROXY would let a proxy reach an
// address the dial guard just refused.
func (t *HTTPTool) execTransport() *http.Transport {
	t.once.Do(func() {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   t.execDialControl,
		}
		t.transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	})
	return t.transport
}

// execDialControl is the last line of SSRF defence: it sees the resolved
// address immediately before the socket connects.
func (t *HTTPTool) execDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: cannot verify address %q", ErrBlockedAddress, address)
	}
	if reason, blocked := t.execBlockReason(ip); blocked {
		return fmt.Errorf("%w: %s is %s", ErrBlockedAddress, ip, reason)
	}
	return nil
}

// execCheckHost pre-screens a URL host so an obviously local target produces a
// clear message before any DNS lookup. It is an ergonomic shortcut; the dial
// guard is what actually enforces the policy.
func (t *HTTPTool) execCheckHost(host string) (string, bool) {
	host = strings.Trim(host, "[]")
	if host == "" {
		return "empty host", true
	}
	if ip := net.ParseIP(host); ip != nil {
		return t.execBlockReason(ip)
	}
	if t.AllowPrivateNetworks {
		return "", false
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return "a local hostname, which boop does not fetch by default", true
	}
	return "", false
}

// execMetadataAddresses are cloud instance metadata endpoints. They are never
// reachable through this tool, opt-in or not.
var execMetadataAddresses = []string{
	"169.254.169.254", // AWS, Azure, GCP, DigitalOcean, OpenStack
	"169.254.170.2",   // AWS ECS task metadata
	"100.100.100.200", // Alibaba Cloud
	"fd00:ec2::254",   // AWS IMDS over IPv6
}

// execRestrictedCIDRs are ranges that are never a legitimate public target.
var execRestrictedCIDRs = func() []*net.IPNet {
	blocks := []string{
		"0.0.0.0/8",     // "this network"
		"100.64.0.0/10", // carrier-grade NAT
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
		"240.0.0.0/4",   // reserved
		"::/128",        // unspecified
		"64:ff9b::/96",  // NAT64
		"2002::/16",     // 6to4
		"100::/64",      // discard-only
	}
	nets := make([]*net.IPNet, 0, len(blocks))
	for _, b := range blocks {
		if _, n, err := net.ParseCIDR(b); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// execBlockReason reports why ip may not be contacted, if it may not.
func (t *HTTPTool) execBlockReason(ip net.IP) (string, bool) {
	// Normalise IPv4-mapped IPv6 (::ffff:127.0.0.1) to its IPv4 form so the
	// range checks below cannot be bypassed by encoding.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, addr := range execMetadataAddresses {
		if ip.Equal(net.ParseIP(addr)) {
			return "a cloud instance metadata endpoint, which boop never fetches", true
		}
	}
	if t.AllowPrivateNetworks {
		return "", false
	}
	switch {
	case ip.IsLoopback():
		return "a loopback address", true
	case ip.IsUnspecified():
		return "an unspecified address", true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "a link-local address", true
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "a multicast address", true
	case ip.IsPrivate():
		return "a private-range address", true
	}
	for _, n := range execRestrictedCIDRs {
		if n.Contains(ip) {
			return "in the reserved range " + n.String(), true
		}
	}
	return "", false
}

// execHTTPOptInHint explains the escape hatch without inviting the model to
// try to set it: it is configuration, not an argument.
func execHTTPOptInHint() string {
	return " Local and private destinations require the operator to enable them in boop's configuration; they cannot be unlocked from a tool call."
}

// execHTTPMethod normalises the method, defaulting to GET.
func execHTTPMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if m == "" {
		return http.MethodGet
	}
	return m
}

// execSensitiveHeaders are header names whose values are never shown.
var execSensitiveHeaders = []string{"authorization", "cookie", "set-cookie", "proxy-authorization", "x-api-key"}

// execIsSensitiveHeader reports whether a header value must be redacted.
//
// Redaction is a spec requirement (§45): tool output reaches transcripts, the
// WebUI and crash reports, none of which should ever hold a credential.
func execIsSensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range execSensitiveHeaders {
		if lower == s {
			return true
		}
	}
	for _, needle := range []string{"token", "secret", "password", "api-key", "apikey", "auth"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// execHTTPHeaders flattens response headers, redacting credentials.
func execHTTPHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		if execIsSensitiveHeader(name) {
			out[name] = "[redacted]"
			continue
		}
		out[name] = strings.Join(values, ", ")
	}
	return out
}

// execHTTPDetail renders the outgoing request for an approval prompt.
func execHTTPDetail(method, target string, headers map[string]string, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", method, target)
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := headers[name]
		if execIsSensitiveHeader(name) {
			value = "[redacted]"
		}
		fmt.Fprintf(&b, "\n%s: %s", name, value)
	}
	if body != "" {
		fmt.Fprintf(&b, "\nbody (%d bytes): %s", len(body), execSummarize(body, 200))
	}
	return b.String()
}

// execFormatHTTPResponse renders a response in the same delimited layout the
// command tools use.
func execFormatHTTPResponse(r HTTPResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", r.Method, r.RequestURL)
	fmt.Fprintf(&b, "status: %d %s\n", r.StatusCode, strings.TrimSpace(strings.TrimPrefix(r.Status, fmt.Sprint(r.StatusCode))))
	fmt.Fprintf(&b, "duration: %s\n", execFormatDuration(r.Duration))
	if r.FinalURL != "" && r.FinalURL != r.RequestURL {
		fmt.Fprintf(&b, "final_url: %s\n", r.FinalURL)
	}
	if len(r.Redirects) > 0 {
		fmt.Fprintf(&b, "redirects: %d\n", len(r.Redirects))
	}
	fmt.Fprintf(&b, "body_bytes: %d\n", r.BodyBytes)

	b.WriteString("--- headers ---\n")
	names := make([]string, 0, len(r.Headers))
	for name := range r.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "%s: %s\n", name, r.Headers[name])
	}

	b.WriteString("--- body ---\n")
	switch {
	case r.BodyBytes == 0:
		b.WriteString("(empty)\n")
	case !utf8.ValidString(r.Body) || strings.ContainsRune(r.Body, 0):
		fmt.Fprintf(&b, "(binary body, %d bytes, not shown)\n", r.BodyBytes)
	default:
		body := execTrimLines(r.Body, DefaultMaxOutputLines)
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Truncated {
		fmt.Fprintf(&b, "[body truncated at %d bytes]\n", r.BodyBytes)
	}
	b.WriteString("--- end ---")
	return b.String()
}

// execFormatHTTPError renders a failed request, naming a blocked address
// explicitly so the model does not retry it in a loop.
func execFormatHTTPError(method, target string, redirects []string, d time.Duration, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", method, target)
	fmt.Fprintf(&b, "duration: %s\n", execFormatDuration(d))
	for _, hop := range redirects {
		fmt.Fprintf(&b, "redirected_to: %s\n", hop)
	}
	if errors.Is(err, ErrBlockedAddress) {
		fmt.Fprintf(&b, "error: request refused by boop's SSRF protection: %v\n", execUnwrapURLError(err))
		b.WriteString("This destination is not reachable from a tool call. Do not retry it.")
		return b.String()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		b.WriteString("error: request timed out\n")
		return b.String()
	}
	fmt.Fprintf(&b, "error: %v", execUnwrapURLError(err))
	return b.String()
}

// execUnwrapURLError strips the *url.Error wrapper so the message names the
// cause rather than repeating the URL.
func execUnwrapURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err
	}
	return err
}
