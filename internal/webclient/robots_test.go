package webclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRobotsAllowed(t *testing.T) {
	const doc = `
# comment line
User-agent: *
Disallow: /private/
Disallow: /tmp
Allow: /private/public.html
Crawl-delay: 2

User-agent: boop
Disallow: /nope
Allow: /nope/ok

User-agent: EvilBot
Disallow: /
`
	r := ParseRobots([]byte(doc))
	tests := []struct {
		name  string
		agent string
		path  string
		allow bool
	}{
		{"boop group disallow", "boop", "/nope", false},
		{"boop group nested allow wins", "boop", "/nope/ok", true},
		{"boop ignores wildcard group", "boop", "/private/secret", true},
		{"wildcard group applies to others", "otherbot", "/private/secret", false},
		{"wildcard allow beats disallow", "otherbot", "/private/public.html", true},
		{"prefix match", "otherbot", "/tmpfile", false},
		{"unlisted path allowed", "otherbot", "/", true},
		{"evilbot fully blocked", "evilbot", "/anything", false},
		{"agent match is case insensitive", "BOOP", "/nope", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Allowed(tc.agent, tc.path); got != tc.allow {
				t.Fatalf("Allowed(%q, %q) = %v, want %v", tc.agent, tc.path, got, tc.allow)
			}
		})
	}

	if d := r.CrawlDelay("otherbot"); d != 2*time.Second {
		t.Fatalf("CrawlDelay(otherbot) = %v, want 2s", d)
	}
	if d := r.CrawlDelay("boop"); d != 0 {
		t.Fatalf("CrawlDelay(boop) = %v, want 0 (its own group sets none)", d)
	}
}

func TestParseRobotsEdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		doc   string
		agent string
		path  string
		allow bool
	}{
		{"empty file allows", "", "boop", "/x", true},
		{"only comments allows", "# nothing here\n", "boop", "/x", true},
		{"html error page allows", "<html><body>404</body></html>", "boop", "/x", true},
		{"empty disallow means allow all", "User-agent: *\nDisallow:\n", "boop", "/x", true},
		{"disallow root blocks all", "User-agent: *\nDisallow: /\n", "boop", "/x", false},
		{"crlf line endings", "User-agent: *\r\nDisallow: /x\r\n", "boop", "/x", false},
		{"inline comment stripped", "User-agent: *\nDisallow: /x # secret\n", "boop", "/x", false},
		{"rules before any agent", "Disallow: /orphan\n", "boop", "/orphan", false},
		{"unknown directive ignored", "User-agent: *\nSitemap: http://x/s.xml\nDisallow: /a\n", "boop", "/a", false},
		{"grouped agents share rules", "User-agent: boop\nUser-agent: otherbot\nDisallow: /shared\n", "boop", "/shared", false},
		{"grouped agents share rules 2", "User-agent: boop\nUser-agent: otherbot\nDisallow: /shared\n", "otherbot", "/shared", false},
		{"wildcard in pattern", "User-agent: *\nDisallow: /*.pdf\n", "boop", "/docs/file.pdf", false},
		{"wildcard non match", "User-agent: *\nDisallow: /*.pdf\n", "boop", "/docs/file.html", true},
		{"dollar anchor matches", "User-agent: *\nDisallow: /page$\n", "boop", "/page", false},
		{"dollar anchor does not match prefix", "User-agent: *\nDisallow: /page$\n", "boop", "/page/sub", true},
		{"query string considered", "User-agent: *\nDisallow: /s?q=\n", "boop", "/s?q=secret", false},
		{"longest match wins for deny", "User-agent: *\nAllow: /a\nDisallow: /a/b\n", "boop", "/a/b/c", false},
		{"longest match wins for allow", "User-agent: *\nDisallow: /a\nAllow: /a/b\n", "boop", "/a/b/c", true},
		{"malformed line skipped", "User-agent: *\nthis is not a directive\nDisallow: /z\n", "boop", "/z", false},
		{"most specific agent wins", "User-agent: *\nDisallow: /\nUser-agent: boop\nAllow: /\n", "boop", "/x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ParseRobots([]byte(tc.doc))
			if got := r.Allowed(tc.agent, tc.path); got != tc.allow {
				t.Fatalf("Allowed(%q, %q) = %v, want %v", tc.agent, tc.path, got, tc.allow)
			}
		})
	}
}

func TestRobotsNilSafe(t *testing.T) {
	var r *Robots
	if !r.Allowed("boop", "/x") {
		t.Fatal("a nil Robots must allow everything")
	}
	if r.CrawlDelay("boop") != 0 {
		t.Fatal("a nil Robots must have no crawl delay")
	}
}

// robotsServer serves a robots.txt and a page, counting robots.txt requests.
func robotsServer(t *testing.T, robotsStatus int, robotsBody string) (*httptest.Server, *int32) {
	t.Helper()
	var robotsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			atomic.AddInt32(&robotsHits, 1)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(robotsStatus)
			fmt.Fprint(w, robotsBody)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "page body")
	}))
	t.Cleanup(srv.Close)
	return srv, &robotsHits
}

func TestFetchRespectsRobots(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		path       string
		wantDenied bool
	}{
		{"allowed path", 200, "User-agent: *\nDisallow: /private\n", "/public", false},
		{"denied path", 200, "User-agent: *\nDisallow: /private\n", "/private/x", true},
		{"denied for boop specifically", 200, "User-agent: boop\nDisallow: /\n", "/anything", true},
		{"missing robots allows", 404, "not found", "/anything", false},
		{"unparseable robots allows", 200, "<html>oops</html>", "/anything", false},
		{"server error denies conservatively", 500, "boom", "/anything", true},
		{"rate limited denies conservatively", 429, "slow down", "/anything", true},
		{"forbidden robots allows", 403, "denied", "/anything", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := robotsServer(t, tc.status, tc.body)
			cfg := testConfig()
			cfg.RespectRobots = true
			c := newTestClient(t, cfg)

			_, err := c.Fetch(context.Background(), srv.URL+tc.path)
			if tc.wantDenied {
				if !errors.Is(err, ErrRobotsDenied) {
					t.Fatalf("error = %v (kind %q), want ErrRobotsDenied", err, KindOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
		})
	}
}

func TestRobotsIgnoredWhenDisabled(t *testing.T) {
	srv, hits := robotsServer(t, 200, "User-agent: *\nDisallow: /\n")
	cfg := testConfig()
	cfg.RespectRobots = false
	c := newTestClient(t, cfg)

	if _, err := c.Fetch(context.Background(), srv.URL+"/private"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("robots.txt was fetched %d times with RespectRobots off", *hits)
	}
}

func TestRobotsCacheReusesOneFetch(t *testing.T) {
	srv, hits := robotsServer(t, 200, "User-agent: *\nDisallow: /private\n")
	cfg := testConfig()
	cfg.RespectRobots = true
	c := newTestClient(t, cfg)

	for i := 0; i < 4; i++ {
		if _, err := c.Fetch(context.Background(), fmt.Sprintf("%s/page%d", srv.URL, i)); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1 (cached)", got)
	}
}

func TestRobotsCacheExpires(t *testing.T) {
	srv, hits := robotsServer(t, 200, "User-agent: *\nDisallow: /private\n")
	cfg := testConfig()
	cfg.RespectRobots = true

	now := time.Now()
	c, err := New(cfg, WithMinHostInterval(0), WithRobotsTTL(time.Minute), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Fetch(context.Background(), srv.URL+"/a"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := c.Fetch(context.Background(), srv.URL+"/b"); err != nil {
		t.Fatalf("Fetch after expiry: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("robots.txt fetched %d times, want 2 (TTL expired)", got)
	}
}

func TestRobotsCacheIsBounded(t *testing.T) {
	now := time.Now()
	cache := newRobotsCache(time.Hour, 8, func() time.Time { return now })
	for i := 0; i < 40; i++ {
		cache.put(fmt.Sprintf("https://host%d.example", i), robotsEntry{
			robots:  &Robots{},
			expires: now.Add(time.Hour),
		})
	}
	if got := cache.size(); got > 8 {
		t.Fatalf("cache holds %d entries, want at most 8", got)
	}
}

func TestRobotsCrawlDelayAppliedToLimiter(t *testing.T) {
	srv, _ := robotsServer(t, 200, "User-agent: *\nCrawl-delay: 0.15\n")
	cfg := testConfig()
	cfg.RespectRobots = true
	c := newTestClient(t, cfg)

	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := c.Fetch(context.Background(), fmt.Sprintf("%s/p%d", srv.URL, i)); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("two fetches took %s; Crawl-delay was not honoured", elapsed)
	}
}

func TestRobotsExcessiveCrawlDelayRefused(t *testing.T) {
	srv, _ := robotsServer(t, 200, "User-agent: *\nCrawl-delay: 3600\n")
	cfg := testConfig()
	cfg.RespectRobots = true
	c := newTestClient(t, cfg)

	_, err := c.Fetch(context.Background(), srv.URL+"/page")
	if !errors.Is(err, ErrRobotsDenied) {
		t.Fatalf("error = %v, want ErrRobotsDenied for an absurd crawl delay", err)
	}
	if !strings.Contains(err.Error(), "crawl delay") {
		t.Fatalf("error should explain the crawl delay: %v", err)
	}
}

func TestRobotsFetchIsNotItselfRobotsChecked(t *testing.T) {
	// A robots.txt that disallows /robots.txt must not deadlock or recurse.
	srv, hits := robotsServer(t, 200, "User-agent: *\nDisallow: /robots.txt\n")
	cfg := testConfig()
	cfg.RespectRobots = true
	c := newTestClient(t, cfg)

	if _, err := c.Fetch(context.Background(), srv.URL+"/page"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1", got)
	}
}

func TestRobotsMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"/", "/anything", true},
		{"/a", "/a", true},
		{"/a", "/ab", true},
		{"/a/", "/ab", false},
		{"/*.php", "/index.php", true},
		{"/*.php$", "/index.php", true},
		{"/*.php$", "/index.php?x=1", false},
		{"/x/*/y", "/x/mid/y", true},
		{"/x/*/y", "/x/y", false},
		{"/fish*", "/fishing", true},
		{"*", "/anything", true},
	}
	for _, tc := range tests {
		_, got := robotsMatch(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("robotsMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}
