package webclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/config"
)

// testConfig returns an enabled configuration suitable for httptest servers:
// loopback is permitted (the test server lives there) and robots.txt is off
// unless a test turns it on.
func testConfig() config.NetworkConfig {
	cfg := config.DefaultNetwork()
	cfg.Enabled = true
	cfg.AllowPrivateNetworks = true
	cfg.RespectRobots = false
	cfg.Timeout = config.Duration(5 * time.Second)
	return cfg
}

// newTestClient builds a client with the politeness delay removed.
func newTestClient(t *testing.T, cfg config.NetworkConfig, opts ...Option) *Client {
	t.Helper()
	opts = append([]Option{WithMinHostInterval(0)}, opts...)
	c, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsBadUserAgent(t *testing.T) {
	cfg := testConfig()
	cfg.UserAgent = "curl/8.0"
	if _, err := New(cfg); err == nil {
		t.Fatal("New must reject a User-Agent that does not identify Boop")
	}
}

func TestFetchDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	c := newTestClient(t, cfg)

	resp, err := c.Fetch(context.Background(), "http://example.com/")
	if resp != nil {
		t.Fatalf("Fetch returned a response while disabled: %+v", resp)
	}
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Fetch error = %v, want ErrDisabled", err)
	}
	if !strings.Contains(err.Error(), EnableHint) {
		t.Fatalf("the refusal must say how to enable it, got %q", err)
	}

	if _, err := c.Search(context.Background(), "go", SearchOptions{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Search error = %v, want ErrDisabled", err)
	}
}

func TestFetchBasics(t *testing.T) {
	var gotUA, gotAccept, gotAcceptEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Custom", "yes")
		fmt.Fprint(w, "<html><body><p>Hello</p></body></html>")
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	resp, err := c.Fetch(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if resp.FinalURL != srv.URL+"/page" {
		t.Fatalf("final URL = %q, want %q", resp.FinalURL, srv.URL+"/page")
	}
	if resp.ContentType != "text/html" {
		t.Fatalf("content type = %q", resp.ContentType)
	}
	if resp.Charset != "utf-8" || !resp.CharsetSupported {
		t.Fatalf("charset = %q supported=%v", resp.Charset, resp.CharsetSupported)
	}
	if !strings.Contains(resp.Text, "Hello") {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.Header.Get("X-Custom") != "yes" {
		t.Fatal("response headers were not returned")
	}
	if resp.Truncated {
		t.Fatal("small body reported as truncated")
	}
	if gotUA != c.UserAgent() || !strings.Contains(gotUA, "Boop") {
		t.Fatalf("User-Agent sent = %q", gotUA)
	}
	if !strings.Contains(gotAccept, "text/html") {
		t.Fatalf("Accept sent = %q", gotAccept)
	}
	if gotAcceptEncoding != "gzip" {
		t.Fatalf("Accept-Encoding sent = %q, want gzip", gotAcceptEncoding)
	}
}

func TestFetchGzip(t *testing.T) {
	body := strings.Repeat("compressed content. ", 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("client did not advertise gzip")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(body))
		_ = zw.Close()
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	resp, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Text != body {
		t.Fatalf("gzip body not decompressed: got %d bytes, want %d", len(resp.Text), len(body))
	}
}

func TestFetchUnsupportedContentEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("\x00\x01binary"))
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	if _, err := c.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestFetchSizeCapAndTruncation(t *testing.T) {
	// An endless response: io.LimitReader is the only thing stopping it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		chunk := bytes.Repeat([]byte("x"), 1024)
		for i := 0; i < 1024; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxResponseBytes = 4096
	c := newTestClient(t, cfg)
	resp, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(resp.Body) != 4096 {
		t.Fatalf("body = %d bytes, want the 4096 byte cap", len(resp.Body))
	}
	if !resp.Truncated {
		t.Fatal("truncation was not reported")
	}
}

func TestFetchExactlyAtCapIsNotTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("y"), 100))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxResponseBytes = 100
	c := newTestClient(t, cfg)
	resp, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Truncated {
		t.Fatal("a body exactly at the cap must not be reported as truncated")
	}
}

func TestFetchRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/one":
			http.Redirect(w, r, srv.URL+"/two", http.StatusFound)
		case "/two":
			http.Redirect(w, r, "/three", http.StatusMovedPermanently)
		case "/three":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "done")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	resp, err := c.Fetch(context.Background(), srv.URL+"/one")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.FinalURL != srv.URL+"/three" {
		t.Fatalf("final URL = %q, want /three", resp.FinalURL)
	}
	if len(resp.Redirects) != 2 {
		t.Fatalf("redirect chain = %v, want 2 hops", resp.Redirects)
	}
	if resp.Text != "done" {
		t.Fatalf("body = %q", resp.Text)
	}
}

func TestFetchTooManyRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRedirects = 2
	c := newTestClient(t, cfg)
	if _, err := c.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("error = %v, want ErrTooManyRedirects", err)
	}
}

func TestFetchRedirectToBlockedTargetIsRefused(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"metadata by name", "http://metadata.internal.example/computeMetadata/v1/"},
		{"file scheme", "file:///etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, tc.target, http.StatusFound)
			}))
			defer srv.Close()

			// metadata.internal.example resolves to the metadata address,
			// which is the redirect bypass this test exists for.
			c := newTestClient(t, testConfig(), WithLookupIP(staticLookup("169.254.169.254")))
			_, err := c.Fetch(context.Background(), srv.URL)
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("error = %v (kind %q), want ErrBlocked", err, KindOf(err))
			}
		})
	}
}

func TestFetchBadStatusReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "no such page")
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	resp, err := c.Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrBadStatus) {
		t.Fatalf("error = %v, want ErrBadStatus", err)
	}
	if resp == nil {
		t.Fatal("the response must still be returned so the model can read the error page")
	}
	if resp.Status != 404 || !strings.Contains(resp.Text, "no such page") {
		t.Fatalf("response = %+v", resp)
	}
	var werr *Error
	if !errors.As(err, &werr) || werr.Status != 404 {
		t.Fatalf("error does not carry the status: %v", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(release); srv.Close() }()

	cfg := testConfig()
	cfg.Timeout = config.Duration(50 * time.Millisecond)
	c := newTestClient(t, cfg)
	if _, err := c.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v (kind %q), want ErrTimeout", err, KindOf(err))
	}
}

func TestFetchCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	c := newTestClient(t, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := c.Fetch(ctx, srv.URL); !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v (kind %q), want ErrCancelled", err, KindOf(err))
	}
}

func TestFetchCharsetHandling(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantText    string
		wantCharset string
		wantOK      bool
	}{
		{
			name:        "utf-8 declared",
			contentType: "text/plain; charset=utf-8",
			body:        []byte("café"),
			wantText:    "café",
			wantCharset: "utf-8",
			wantOK:      true,
		},
		{
			name:        "latin-1 declared",
			contentType: "text/plain; charset=iso-8859-1",
			body:        []byte{'c', 'a', 'f', 0xE9},
			wantText:    "café",
			wantCharset: "iso-8859-1",
			wantOK:      true,
		},
		{
			name:        "windows-1252 smart quote",
			contentType: "text/plain; charset=windows-1252",
			body:        []byte{0x93, 'h', 'i', 0x94},
			wantText:    "“hi”",
			wantCharset: "windows-1252",
			wantOK:      true,
		},
		{
			name:        "unsupported charset is reported not guessed",
			contentType: "text/plain; charset=shift_jis",
			body:        []byte{0x82, 0xA0},
			wantText:    "",
			wantCharset: "shift-jis",
			wantOK:      false,
		},
		{
			name:        "meta charset used when header is silent",
			contentType: "text/html",
			body:        append([]byte(`<html><head><meta charset="iso-8859-1"></head><body>caf`), 0xE9, '<', '/', 'b', 'o', 'd', 'y', '>'),
			wantText:    `<html><head><meta charset="iso-8859-1"></head><body>café</body>`,
			wantCharset: "iso-8859-1",
			wantOK:      true,
		},
		{
			name:        "utf-8 bom wins",
			contentType: "text/plain; charset=iso-8859-1",
			body:        append([]byte{0xEF, 0xBB, 0xBF}, []byte("café")...),
			wantText:    "café",
			wantCharset: "utf-8",
			wantOK:      true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, testConfig())
			resp, err := c.Fetch(context.Background(), srv.URL)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if resp.CharsetSupported != tc.wantOK {
				t.Fatalf("CharsetSupported = %v, want %v", resp.CharsetSupported, tc.wantOK)
			}
			if resp.Charset != tc.wantCharset {
				t.Fatalf("Charset = %q, want %q", resp.Charset, tc.wantCharset)
			}
			if resp.Text != tc.wantText {
				t.Fatalf("Text = %q, want %q", resp.Text, tc.wantText)
			}
		})
	}
}

func TestFetchBinaryContentIsNotDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	resp, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Text != "" || resp.CharsetSupported {
		t.Fatalf("binary content must not be presented as text: %+v", resp)
	}
	if len(resp.Body) != 4 {
		t.Fatalf("raw bytes not returned: %v", resp.Body)
	}
}

func TestFetchPageExtracts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Doc</title></head><body>
			<script>ignore()</script>
			<p>First paragraph.</p>
			<a href="/next">Next</a>
		</body></html>`)
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	page, err := c.FetchPage(context.Background(), srv.URL+"/dir/index.html")
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if page.Document.Title != "Doc" {
		t.Fatalf("title = %q", page.Document.Title)
	}
	if strings.Contains(page.Document.Text, "ignore()") {
		t.Fatalf("script content leaked into text: %q", page.Document.Text)
	}
	if !strings.Contains(page.Document.Text, "First paragraph.") {
		t.Fatalf("text = %q", page.Document.Text)
	}
	want := srv.URL + "/next"
	if len(page.Document.Links) != 1 || page.Document.Links[0].URL != want {
		t.Fatalf("links = %+v, want %q", page.Document.Links, want)
	}
}

func TestFetchPagePlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "just words")
	}))
	defer srv.Close()

	c := newTestClient(t, testConfig())
	page, err := c.FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if page.Document.Text != "just words" {
		t.Fatalf("text = %q", page.Document.Text)
	}
}

func TestFetchDomainPolicyApplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.AllowedDomains = []string{"example.com"}
	c := newTestClient(t, cfg)
	if _, err := c.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked for a host outside the allowlist", err)
	}
}

func TestPerHostRateLimit(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig()
	c, err := New(cfg, WithMinHostInterval(120*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Fetch(context.Background(), srv.URL); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 240*time.Millisecond {
		t.Fatalf("three requests took %s; the per-host minimum interval was not applied", elapsed)
	}
	if hits != 3 {
		t.Fatalf("server saw %d requests, want 3", hits)
	}
}

func TestRateLimitRespectsContext(t *testing.T) {
	l := newHostLimiter(time.Hour, time.Now)
	if err := l.wait(context.Background(), "example.com"); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.wait(ctx, "example.com"); err == nil {
		t.Fatal("a blocked wait must honour context cancellation")
	}
}

func TestHostLimiterEviction(t *testing.T) {
	now := time.Now()
	l := newHostLimiter(time.Millisecond, func() time.Time { return now })
	for i := 0; i < maxLimiterHosts+50; i++ {
		if err := l.wait(context.Background(), fmt.Sprintf("host%d.example", i)); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	l.mu.Lock()
	size := len(l.next)
	l.mu.Unlock()
	if size > maxLimiterHosts {
		t.Fatalf("limiter tracks %d hosts, want at most %d", size, maxLimiterHosts)
	}
}

func TestParseContentType(t *testing.T) {
	tests := []struct {
		in       string
		wantType string
		wantCS   string
	}{
		{"text/html; charset=utf-8", "text/html", "utf-8"},
		{"TEXT/HTML;CHARSET=UTF-8", "text/html", "UTF-8"},
		{"application/json", "application/json", ""},
		{"", "", ""},
		{"text/html; charset=", "text/html", ""},
		{"text/html; ; broken", "text/html", ""},
	}
	for _, tc := range tests {
		gotType, gotCS := parseContentType(tc.in)
		if gotType != tc.wantType || gotCS != tc.wantCS {
			t.Errorf("parseContentType(%q) = (%q, %q), want (%q, %q)", tc.in, gotType, gotCS, tc.wantType, tc.wantCS)
		}
	}
}

func TestErrorKindPlumbing(t *testing.T) {
	err := newError(KindBlocked, "fetch", "http://x/", "nope")
	if KindOf(err) != KindBlocked {
		t.Fatalf("KindOf = %q", KindOf(err))
	}
	if !errors.Is(err, ErrBlocked) || errors.Is(err, ErrTimeout) {
		t.Fatal("sentinel matching is wrong")
	}
	wrapped := fmt.Errorf("outer: %w", err)
	if !errors.Is(wrapped, ErrBlocked) || KindOf(wrapped) != KindBlocked {
		t.Fatal("kind must survive wrapping")
	}
	if KindOf(errors.New("unrelated")) != "" {
		t.Fatal("KindOf must be empty for foreign errors")
	}
}
