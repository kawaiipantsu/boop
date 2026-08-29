package webclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/config"
)

// ddgLiteFixture reproduces the shapes the live lite endpoint emits, verified
// against https://lite.duckduckgo.com/lite/ for the query "golang context
// package". It deliberately mixes attribute orders — class before href and
// href before class — and includes one legacy /l/?uddg= redirect link, because
// assuming a single shape is exactly how this parser breaks in production.
const ddgLiteFixture = `<!DOCTYPE html>
<html lang="en-US">
<head><title>golang context package at DuckDuckGo</title></head>
<body>
<form action="/lite/" method="post">
<input name="q" type="text" value="golang context package" />
<input name="kl" type="hidden" value="wt-wt" />
</form>
<table border="0">
<tr>
  <td valign="top">1.&nbsp;</td>
  <td><a rel="nofollow" href="https://pkg.go.dev/context" class="result-link">context package - context - Go Packages</a></td>
</tr>
<tr>
  <td>&nbsp;&nbsp;&nbsp;</td>
  <td class="result-snippet">Package context defines the Context type, which carries deadlines, cancellation signals, and other request-scoped values.</td>
</tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">2.&nbsp;</td>
  <td><a class="result-link" href="https://go.dev/blog/context" rel="nofollow">Go Concurrency Patterns: Context - The Go Programming Language</a></td>
</tr>
<tr>
  <td>&nbsp;</td>
  <td class="result-snippet">In Go servers, each incoming request is handled in its own goroutine.</td>
</tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">3.&nbsp;</td>
  <td><a href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgobyexample.com%2Fcontext&amp;rut=abc123" class="result-link">Go by Example: Context</a></td>
</tr>
<tr>
  <td>&nbsp;</td>
  <td class="result-snippet">Go by Example: Context &amp; cancellation, worked through.</td>
</tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">4.&nbsp;</td>
  <td><a class="result-link" href="https://www.digitalocean.com/community/tutorials/how-to-use-contexts-in-go">How To Use Contexts in Go | DigitalOcean</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">A tutorial on the context package and its use in servers.</td></tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">5.&nbsp;</td>
  <td><a href="https://blog.golang.org/context" class="result-link" rel="nofollow">Go Blog: Context</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">The original blog post introducing context.</td></tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">6.&nbsp;</td>
  <td><a class="result-link" href="https://stackoverflow.com/questions/tagged/go-context">Newest &#39;go-context&#39; Questions - Stack Overflow</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">Questions tagged go-context on Stack Overflow.</td></tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">7.&nbsp;</td>
  <td><a class="result-link" href="https://www.practical-go-lessons.com/chap-40-context.html">Chapter 40: Context</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">Practical Go Lessons chapter about contexts.</td></tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">8.&nbsp;</td>
  <td><a class="result-link" href="https://pkg.go.dev/context#WithCancel">context.WithCancel</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">WithCancel returns a copy of parent with a new Done channel.</td></tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">9.&nbsp;</td>
  <td><a class="result-link" href="https://github.com/golang/go/wiki/CodeReviewComments#contexts">Code Review Comments &middot; golang/go Wiki</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">Values stored in a Context should be request-scoped.</td></tr>
<tr><td colspan="2">&nbsp;</td></tr>
<tr>
  <td valign="top">10.&nbsp;</td>
  <td><a class="result-link" href="https://www.reddit.com/r/golang/comments/context/">Understanding context : r/golang</a></td>
</tr>
<tr><td>&nbsp;</td><td class="result-snippet">A discussion thread about idiomatic context use.</td></tr>
</table>
</body>
</html>`

// ddgChallengeFixture is the shape of DuckDuckGo's bot-block page: HTTP 200,
// no results, a challenge in the body.
const ddgChallengeFixture = `<!DOCTYPE html>
<html>
<head><title>DuckDuckGo</title></head>
<body>
<div id="anomaly-modal__mask">
<p>Our systems have detected unusual traffic from your computer network.</p>
<p>Please try again later. If this error persists, please let us know.</p>
<form action="/anomaly.html" method="post"><input type="submit" value="Continue"></form>
</div>
</body>
</html>`

func TestParseDuckDuckGoLite(t *testing.T) {
	results, err := parseDuckDuckGoLite(ddgLiteFixture, "https://lite.duckduckgo.com/lite/", 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("got %d results, want 10", len(results))
	}

	want := []SearchResult{
		{Rank: 1, Title: "context package - context - Go Packages", URL: "https://pkg.go.dev/context",
			Snippet: "Package context defines the Context type, which carries deadlines, cancellation signals, and other request-scoped values."},
		{Rank: 2, Title: "Go Concurrency Patterns: Context - The Go Programming Language", URL: "https://go.dev/blog/context",
			Snippet: "In Go servers, each incoming request is handled in its own goroutine."},
		{Rank: 3, Title: "Go by Example: Context", URL: "https://gobyexample.com/context",
			Snippet: "Go by Example: Context & cancellation, worked through."},
	}
	for i, w := range want {
		got := results[i]
		if got.Rank != w.Rank || got.Title != w.Title || got.URL != w.URL || got.Snippet != w.Snippet {
			t.Errorf("result %d =\n  %+v\nwant\n  %+v", i, got, w)
		}
	}
	for i, r := range results {
		if r.Rank != i+1 {
			t.Errorf("result %d has rank %d", i, r.Rank)
		}
		if r.Title == "" || r.URL == "" || r.Snippet == "" {
			t.Errorf("result %d is incomplete: %+v", i, r)
		}
		if !strings.HasPrefix(r.URL, "https://") {
			t.Errorf("result %d URL is not absolute https: %q", i, r.URL)
		}
		if strings.Contains(r.URL, "uddg") {
			t.Errorf("result %d still wraps a DuckDuckGo redirect: %q", i, r.URL)
		}
	}
	if results[5].Title != "Newest 'go-context' Questions - Stack Overflow" {
		t.Errorf("entities in titles were not decoded: %q", results[5].Title)
	}
}

func TestParseDuckDuckGoLiteRespectsMax(t *testing.T) {
	results, err := parseDuckDuckGoLite(ddgLiteFixture, "https://lite.duckduckgo.com/lite/", 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[2].Snippet == "" {
		t.Fatal("snippets must still pair correctly when the list is capped")
	}
}

func TestParseDuckDuckGoLiteFailureModes(t *testing.T) {
	tests := []struct {
		name string
		body string
		kind ErrorKind
		want int
	}{
		{"challenge page", ddgChallengeFixture, KindSearchBlocked, 0},
		{"captcha page", "<html><body>Please complete the captcha</body></html>", KindSearchBlocked, 0},
		{"layout changed", "<html><body><div class=\"web-result\">something new</div></body></html>", KindMalformed, 0},
		{"genuinely no results", "<html><body><p>No results found for that query.</p></body></html>", "", 0},
		{"results present", ddgLiteFixture, "", 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results, err := parseDuckDuckGoLite(tc.body, "https://lite.duckduckgo.com/lite/", 10)
			if got := KindOf(err); got != tc.kind {
				t.Fatalf("kind = %q, want %q (err=%v)", got, tc.kind, err)
			}
			if len(results) != tc.want {
				t.Fatalf("got %d results, want %d", len(results), tc.want)
			}
		})
	}
}

func TestUnwrapDDGRedirect(t *testing.T) {
	base, _ := url.Parse("https://lite.duckduckgo.com/lite/")
	tests := []struct {
		name string
		href string
		want string
	}{
		{"direct url", "https://example.com/page", "https://example.com/page"},
		{"redirect form", "//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fx&rut=z", "https://example.com/x"},
		{"redirect absolute", "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fy", "https://example.com/y"},
		{"redirect without uddg", "//duckduckgo.com/l/?rut=z", "https://duckduckgo.com/l/?rut=z"},
		{"relative", "/settings", "https://lite.duckduckgo.com/settings"},
		{"javascript refused", "javascript:alert(1)", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unwrapDDGRedirect(tc.href, base); got != tc.want {
				t.Fatalf("unwrapDDGRedirect(%q) = %q, want %q", tc.href, got, tc.want)
			}
		})
	}
}

func TestSafeSearchParam(t *testing.T) {
	tests := []struct{ in, want string }{
		{"off", "-2"},
		{"moderate", "-1"},
		{"", "-1"},
		{"strict", "1"},
		{"STRICT", "1"},
		{"nonsense", ""},
	}
	for _, tc := range tests {
		if got := safeSearchParam(tc.in); got != tc.want {
			t.Errorf("safeSearchParam(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSearchOptionsDefaults(t *testing.T) {
	cfg := config.DefaultNetwork().Search
	tests := []struct {
		name string
		in   SearchOptions
		want SearchOptions
	}{
		{"all defaults", SearchOptions{}, SearchOptions{MaxResults: 10, SafeSearch: "moderate", Region: "wt-wt"}},
		{"explicit overrides", SearchOptions{MaxResults: 3, SafeSearch: "off", Region: "uk-en"},
			SearchOptions{MaxResults: 3, SafeSearch: "off", Region: "uk-en"}},
		{"clamped to hard max", SearchOptions{MaxResults: 9999},
			SearchOptions{MaxResults: MaxSearchResults, SafeSearch: "moderate", Region: "wt-wt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.withDefaults(cfg); got != tc.want {
				t.Fatalf("withDefaults = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// ddgTestServers starts stand-ins for both DuckDuckGo endpoints and returns a
// client wired to them.
func ddgTestServers(t *testing.T, cfg config.NetworkConfig, lite, instant http.HandlerFunc) (*Client, *httptest.Server, *httptest.Server) {
	t.Helper()
	liteSrv := httptest.NewServer(lite)
	t.Cleanup(liteSrv.Close)
	instantSrv := httptest.NewServer(instant)
	t.Cleanup(instantSrv.Close)

	c := newTestClient(t, cfg)
	c.backend = &DuckDuckGo{client: c, liteURL: liteSrv.URL + "/lite/", instantURL: instantSrv.URL + "/"}
	return c, liteSrv, instantSrv
}

func TestSearchEndToEnd(t *testing.T) {
	var gotQuery, gotRegion, gotKP, gotMethod, gotCT, gotUA string
	lite := func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		_ = r.ParseForm()
		gotQuery = r.PostFormValue("q")
		gotRegion = r.PostFormValue("kl")
		gotKP = r.PostFormValue("kp")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgLiteFixture)
	}
	instant := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("instant answer format = %q", r.URL.Query().Get("format"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Heading":"Context","AbstractText":"A context carries deadlines.",
			"AbstractURL":"https://en.wikipedia.org/wiki/Context","AbstractSource":"Wikipedia",
			"Answer":"","Definition":"",
			"RelatedTopics":[{"FirstURL":"https://duckduckgo.com/Goroutine","Text":"Goroutine"},
			{"Topics":[{"FirstURL":"https://duckduckgo.com/Nested","Text":"Nested topic"}]}]}`)
	}

	c, _, _ := ddgTestServers(t, testConfig(), lite, instant)
	resp, err := c.Search(context.Background(), "golang context package", SearchOptions{Region: "uk-en", SafeSearch: "strict"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("lite endpoint method = %q, want POST", gotMethod)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", gotCT)
	}
	if !strings.Contains(gotUA, "Boop") {
		t.Errorf("search must identify Boop, got UA %q", gotUA)
	}
	if gotQuery != "golang context package" {
		t.Errorf("q = %q", gotQuery)
	}
	if gotRegion != "uk-en" {
		t.Errorf("kl = %q, want uk-en", gotRegion)
	}
	if gotKP != "1" {
		t.Errorf("kp = %q, want 1 for strict", gotKP)
	}
	if len(resp.Results) != 10 {
		t.Fatalf("got %d results, want 10", len(resp.Results))
	}
	if resp.Provider != "duckduckgo" || resp.Query != "golang context package" {
		t.Errorf("response metadata = %+v", resp)
	}
	if resp.Abstract != "A context carries deadlines." || resp.Heading != "Context" ||
		resp.AbstractURL != "https://en.wikipedia.org/wiki/Context" || resp.AbstractSource != "Wikipedia" {
		t.Errorf("instant answer enrichment missing: %+v", resp)
	}
	if len(resp.RelatedTopics) != 2 || resp.RelatedTopics[1].URL != "https://duckduckgo.com/Nested" {
		t.Errorf("related topics = %+v", resp.RelatedTopics)
	}
}

func TestSearchHonoursConfiguredMaxResults(t *testing.T) {
	lite := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgLiteFixture)
	}
	cfg := testConfig()
	cfg.Search.MaxResults = 4
	c, _, _ := ddgTestServers(t, cfg, lite, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})
	resp, err := c.Search(context.Background(), "q", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 4 {
		t.Fatalf("got %d results, want the configured 4", len(resp.Results))
	}
}

func TestSearchSurvivesInstantAnswerFailure(t *testing.T) {
	lite := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgLiteFixture)
	}
	instant := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	c, _, _ := ddgTestServers(t, testConfig(), lite, instant)
	resp, err := c.Search(context.Background(), "q", SearchOptions{})
	if err != nil {
		t.Fatalf("a failing enrichment call must not fail the search: %v", err)
	}
	if len(resp.Results) != 10 || resp.Abstract != "" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestSearchSkipInstantAnswer(t *testing.T) {
	var instantHits int
	lite := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgLiteFixture)
	}
	instant := func(w http.ResponseWriter, r *http.Request) {
		instantHits++
		fmt.Fprint(w, "{}")
	}
	c, _, _ := ddgTestServers(t, testConfig(), lite, instant)
	if _, err := c.Search(context.Background(), "q", SearchOptions{SkipInstantAnswer: true}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if instantHits != 0 {
		t.Fatalf("instant answer endpoint was called %d times despite SkipInstantAnswer", instantHits)
	}
}

func TestSearchChallengeIsTyped(t *testing.T) {
	lite := func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 with a challenge body: the failure mode that must not
		// look like "no results".
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgChallengeFixture)
	}
	c, _, _ := ddgTestServers(t, testConfig(), lite, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	})
	_, err := c.Search(context.Background(), "q", SearchOptions{})
	if !errors.Is(err, ErrSearchBlocked) {
		t.Fatalf("error = %v (kind %q), want ErrSearchBlocked", err, KindOf(err))
	}
}

func TestSearchBadStatus(t *testing.T) {
	lite := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}
	c, _, _ := ddgTestServers(t, testConfig(), lite, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	})
	_, err := c.Search(context.Background(), "q", SearchOptions{})
	if !errors.Is(err, ErrBadStatus) {
		t.Fatalf("error = %v, want ErrBadStatus", err)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	c := newTestClient(t, testConfig())
	if _, err := c.Search(context.Background(), "   ", SearchOptions{}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("error = %v, want ErrMalformed", err)
	}
}

func TestSearchUnsupportedProvider(t *testing.T) {
	cfg := testConfig()
	cfg.Search.Provider = "bing"
	c := newTestClient(t, cfg)
	if c.SearchBackend().Name() != "bing" {
		t.Fatalf("backend name = %q", c.SearchBackend().Name())
	}
	_, err := c.Search(context.Background(), "q", SearchOptions{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestSearchBackendSelection(t *testing.T) {
	for _, name := range []string{"", "duckduckgo", "DuckDuckGo", "ddg"} {
		cfg := testConfig()
		cfg.Search.Provider = name
		c := newTestClient(t, cfg)
		if _, ok := c.SearchBackend().(*DuckDuckGo); !ok {
			t.Fatalf("provider %q did not select the DuckDuckGo backend", name)
		}
	}
}

func TestSearchEndpointsListed(t *testing.T) {
	c := newTestClient(t, testConfig())
	ddg, ok := c.SearchBackend().(*DuckDuckGo)
	if !ok {
		t.Fatal("default backend is not DuckDuckGo")
	}
	eps := ddg.SearchEndpoints()
	if len(eps) != 2 || !strings.Contains(eps[0], "lite.duckduckgo.com") || !strings.Contains(eps[1], "api.duckduckgo.com") {
		t.Fatalf("SearchEndpoints() = %v", eps)
	}
}

func TestSearchWithCustomBackend(t *testing.T) {
	c, err := New(testConfig(), WithMinHostInterval(0), WithSearchBackend(stubBackend{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Search(context.Background(), "anything", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Provider != "stub" || len(resp.Results) != 1 {
		t.Fatalf("custom backend was not used: %+v", resp)
	}
}

type stubBackend struct{}

func (stubBackend) Name() string { return "stub" }

func (stubBackend) Search(_ context.Context, q string, _ SearchOptions) (*SearchResponse, error) {
	return &SearchResponse{
		Query:    q,
		Provider: "stub",
		Results:  []SearchResult{{Rank: 1, Title: "stub", URL: "https://example.com/", Snippet: "s"}},
	}, nil
}
