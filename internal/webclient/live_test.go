package webclient

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/config"
)

// liveEnvVar opts in to tests that talk to the real internet.
//
// PROJECT.md §41 requires the default suite to be self-contained, so these are
// skipped unless BOOP_NETWORK_TESTS is set. They exist because the DuckDuckGo
// lite endpoint is undocumented markup that can change without notice: running
// them occasionally is how that gets noticed before a user does.
const liveEnvVar = "BOOP_NETWORK_TESTS"

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnvVar) == "" {
		t.Skipf("set %s=1 to run tests against the real internet", liveEnvVar)
	}
}

func liveConfig() config.NetworkConfig {
	cfg := config.DefaultNetwork()
	cfg.Enabled = true
	cfg.Timeout = config.Duration(20 * time.Second)
	return cfg
}

func TestLiveFetch(t *testing.T) {
	requireLive(t)
	c, err := New(liveConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	page, err := c.FetchPage(ctx, "https://example.com/")
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if page.Response.Status != 200 {
		t.Fatalf("status = %d", page.Response.Status)
	}
	if !strings.Contains(strings.ToLower(page.Document.Title), "example") {
		t.Fatalf("title = %q", page.Document.Title)
	}
	if page.Document.Text == "" {
		t.Fatal("no text extracted")
	}
}

func TestLiveSearch(t *testing.T) {
	requireLive(t)
	c, err := New(liveConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := c.Search(ctx, "golang context package", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("no results; the lite endpoint markup may have changed")
	}
	for i, r := range resp.Results {
		t.Logf("%2d. %s\n    %s\n    %s", r.Rank, r.Title, r.URL, r.Snippet)
		if r.Title == "" || !strings.HasPrefix(r.URL, "http") {
			t.Errorf("result %d is malformed: %+v", i, r)
		}
	}
	t.Logf("abstract: %q (%s)", resp.Abstract, resp.AbstractURL)
}

func TestLiveInstantAnswer(t *testing.T) {
	requireLive(t)
	c, err := New(liveConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ddg := c.SearchBackend().(*DuckDuckGo)
	ia, err := ddg.instantAnswer(ctx, "duckduckgo", SearchOptions{}.withDefaults(c.cfg.Search))
	if err != nil {
		t.Fatalf("instantAnswer: %v", err)
	}
	t.Logf("heading=%q source=%q url=%q related=%d\nabstract=%q",
		ia.Heading, ia.AbstractSource, ia.AbstractURL, len(ia.related(5)), ia.AbstractText)
	if ia.Heading == "" && ia.AbstractText == "" {
		t.Error("expected an instant answer for a query that has one; the API shape may have changed")
	}
}

func TestLiveRobots(t *testing.T) {
	requireLive(t)
	c, err := New(liveConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	u, err := c.guard.CheckRawURL(ctx, "https://www.google.com/search?q=x")
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if err := c.checkRobots(ctx, u); err == nil {
		t.Fatal("google.com/search is disallowed by robots.txt; the check should have refused it")
	}
}
