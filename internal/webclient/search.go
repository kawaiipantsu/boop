package webclient

import (
	"context"
	"encoding/json"
	stdhtml "html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/boop-dev/boop/internal/config"
)

// Search limits.
const (
	// MaxSearchResults is the hard ceiling on results, whatever the config
	// asks for. The backends here return a single page.
	MaxSearchResults = 50
	// maxSearchBytes caps a search response body.
	maxSearchBytes int64 = 4 << 20
)

// SearchResult is one ranked web result.
type SearchResult struct {
	// Rank is the 1-based position in the result list.
	Rank int
	// Title is the result heading.
	Title string
	// URL is the absolute target URL.
	URL string
	// Snippet is the engine's summary of the page.
	Snippet string
}

// SearchResponse is a completed search.
type SearchResponse struct {
	// Query is the query as sent.
	Query string
	// Provider names the backend that answered.
	Provider string
	// Results are the ranked web results, best first.
	Results []SearchResult
	// Heading, Abstract, AbstractURL and AbstractSource carry the engine's
	// instant answer, when it had one. They enrich the results; they do not
	// replace them.
	Heading        string
	Abstract       string
	AbstractURL    string
	AbstractSource string
	// Answer is a direct computed answer (a conversion, a calculation).
	Answer string
	// Definition is a dictionary definition, when the query looked like one.
	Definition string
	// RelatedTopics are DuckDuckGo's related entity links.
	RelatedTopics []SearchResult
}

// SearchOptions overrides the configured search settings for one call. Zero
// fields fall back to config.SearchConfig.
type SearchOptions struct {
	// MaxResults caps the returned results.
	MaxResults int
	// SafeSearch is "off", "moderate" or "strict".
	SafeSearch string
	// Region is a DuckDuckGo region code such as "wt-wt" or "uk-en".
	Region string
	// SkipInstantAnswer suppresses the second, enrichment request.
	SkipInstantAnswer bool
}

// withDefaults fills unset fields from the configuration.
func (o SearchOptions) withDefaults(cfg config.SearchConfig) SearchOptions {
	if o.MaxResults <= 0 {
		o.MaxResults = cfg.MaxResults
	}
	if o.MaxResults <= 0 {
		o.MaxResults = config.DefaultSearchMaxResults
	}
	if o.MaxResults > MaxSearchResults {
		o.MaxResults = MaxSearchResults
	}
	if o.SafeSearch == "" {
		o.SafeSearch = cfg.SafeSearch
	}
	if o.Region == "" {
		o.Region = cfg.Region
	}
	return o
}

// Backend is a web search engine. It exists so a second engine can be added
// without touching the client: the DuckDuckGo implementation below scrapes an
// undocumented endpoint, and that is not a foundation to bet the feature on.
type Backend interface {
	// Name is the provider identifier, matching config.SearchConfig.Provider.
	Name() string
	// Search runs one query. opts arrives already merged with the config.
	Search(ctx context.Context, query string, opts SearchOptions) (*SearchResponse, error)
}

// newBackend selects a backend for a provider name.
func newBackend(provider string, c *Client) Backend {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "duckduckgo", "ddg":
		return &DuckDuckGo{client: c}
	default:
		return unsupportedBackend{name: provider}
	}
}

// unsupportedBackend stands in for a configured provider Boop does not have,
// so the failure arrives at Search time with a clear message rather than as a
// nil backend panic.
type unsupportedBackend struct{ name string }

func (u unsupportedBackend) Name() string { return u.name }

func (u unsupportedBackend) Search(context.Context, string, SearchOptions) (*SearchResponse, error) {
	return nil, newError(KindUnsupported, "search", "",
		"search provider %q is not implemented; only %q is supported",
		u.name, config.DefaultSearchProvider)
}

// Search runs a web search through the configured backend.
//
// Like Fetch it refuses outright when outbound access is disabled. An empty
// query is a malformed request, not an empty result set.
func (c *Client) Search(ctx context.Context, query string, opts SearchOptions) (*SearchResponse, error) {
	if !c.cfg.Enabled {
		return nil, errDisabled("search")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, newError(KindMalformed, "search", "", "the search query is empty")
	}
	return c.backend.Search(ctx, q, opts.withDefaults(c.cfg.Search))
}

// SearchBackend returns the backend in use, mainly so a caller can name it in
// a permission prompt or a tool result.
func (c *Client) SearchBackend() Backend { return c.backend }

// DuckDuckGo endpoints.
const (
	// ddgLiteURL is the no-JavaScript HTML endpoint. It is undocumented:
	// DuckDuckGo can change its markup, rate-limit it or withdraw it at any
	// time, and when they do, parsing here breaks. That is the price of
	// having web results without an API key. Failures are reported as
	// KindSearchBlocked or KindMalformed so the model is told the search
	// engine failed rather than being handed a silent empty list.
	ddgLiteURL = "https://lite.duckduckgo.com/lite/"
	// ddgInstantURL is the official, documented Instant Answer API. It is
	// stable but returns no general web results, so it is used only to
	// enrich the scraped list with an abstract.
	ddgInstantURL = "https://api.duckduckgo.com/"
)

// DuckDuckGo searches DuckDuckGo.
//
// It combines two endpoints: the lite HTML endpoint for ranked web results and
// the Instant Answer API for a structured abstract. The two run concurrently
// against different hosts; a failure of the enrichment call is swallowed,
// because an abstract is a bonus and the results are the point.
type DuckDuckGo struct {
	client *Client
	// liteURL and instantURL default to the constants above. They are
	// fields so tests can point the backend at an httptest server.
	liteURL    string
	instantURL string
}

// NewDuckDuckGo returns a DuckDuckGo backend bound to a client.
func NewDuckDuckGo(c *Client) *DuckDuckGo { return &DuckDuckGo{client: c} }

// lite returns the configured lite endpoint.
func (d *DuckDuckGo) lite() string {
	if d.liteURL != "" {
		return d.liteURL
	}
	return ddgLiteURL
}

// instant returns the configured instant answer endpoint.
func (d *DuckDuckGo) instant() string {
	if d.instantURL != "" {
		return d.instantURL
	}
	return ddgInstantURL
}

// SearchEndpoints lists the URLs this backend contacts. When
// network.allowed_domains is set, these hosts must be on it or search cannot
// run at all — the allowlist applies to the search engine too.
func (d *DuckDuckGo) SearchEndpoints() []string { return []string{d.lite(), d.instant()} }

// Name implements Backend.
func (d *DuckDuckGo) Name() string { return "duckduckgo" }

// Search implements Backend.
func (d *DuckDuckGo) Search(ctx context.Context, query string, opts SearchOptions) (*SearchResponse, error) {
	out := &SearchResponse{Query: query, Provider: d.Name()}

	var (
		wg       sync.WaitGroup
		instant  *instantAnswer
		results  []SearchResult
		liteErr  error
		instMu   sync.Mutex
		wantInst = !opts.SkipInstantAnswer
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		results, liteErr = d.searchLite(ctx, query, opts)
	}()
	if wantInst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ia, err := d.instantAnswer(ctx, query, opts)
			if err != nil {
				return // enrichment is best-effort
			}
			instMu.Lock()
			instant = ia
			instMu.Unlock()
		}()
	}
	wg.Wait()

	if liteErr != nil {
		// Without web results a bare abstract is not a search result set;
		// report the failure rather than pretend the search succeeded.
		return nil, liteErr
	}
	out.Results = results
	if instant != nil {
		out.Heading = instant.Heading
		out.Abstract = instant.AbstractText
		out.AbstractURL = instant.AbstractURL
		out.AbstractSource = instant.AbstractSource
		out.Answer = strings.TrimSpace(instant.Answer)
		out.Definition = strings.TrimSpace(instant.Definition)
		out.RelatedTopics = instant.related(opts.MaxResults)
	}
	return out, nil
}

// searchLite queries the lite HTML endpoint.
func (d *DuckDuckGo) searchLite(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", query)
	if opts.Region != "" {
		form.Set("kl", opts.Region)
	}
	if kp := safeSearchParam(opts.SafeSearch); kp != "" {
		form.Set("kp", kp)
	}

	resp, err := d.client.do(ctx, request{
		op:          "search",
		method:      http.MethodPost,
		rawURL:      d.lite(),
		body:        []byte(form.Encode()),
		contentType: "application/x-www-form-urlencoded",
		accept:      "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5",
		// robots.txt governs crawling a site's content. This is the user's
		// own explicit query to a search endpoint, submitted the same way a
		// browser would, so the crawl rules are not the applicable contract.
		// Rate limiting and the identifying User-Agent still apply.
		skipRobots: true,
		maxBytes:   maxSearchBytes,
		strictSize: true,
	})
	if err != nil {
		return nil, err
	}
	if !resp.CharsetSupported {
		return nil, newError(KindUnsupported, "search", resp.FinalURL,
			"search response uses charset %q, which Boop cannot decode", resp.Charset)
	}
	return parseDuckDuckGoLite(resp.Text, resp.FinalURL, opts.MaxResults)
}

// safeSearchParam maps Boop's safe-search setting onto DuckDuckGo's kp value.
func safeSearchParam(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off":
		return "-2"
	case "moderate", "":
		return "-1"
	case "strict", "on":
		return "1"
	}
	return ""
}

// ddgTDRe matches a table cell and its contents on the lite page.
var ddgTDRe = regexp.MustCompile(`(?is)<td\s([^>]*)>(.*?)</td>`)

// challengeMarkers are strings DuckDuckGo serves, with HTTP 200, when it has
// decided the caller is a bot.
var challengeMarkers = []string{
	"anomaly.html",
	"anomaly-modal",
	"unusual traffic",
	"detected unusual",
	"are you a robot",
	"captcha",
	"blocked because of a possible",
	"if this error persists, please let us know",
}

// parseDuckDuckGoLite extracts results from the lite endpoint's HTML.
//
// The markup is a table: each result is an <a class="result-link"> whose href
// is normally the destination URL, followed by a <td class="result-snippet">.
// Attributes are matched order-independently — the live page does not put
// class before href consistently, and assuming it does produces zero results
// with no error, which is the worst possible failure mode.
func parseDuckDuckGoLite(body, baseURL string, max int) ([]SearchResult, error) {
	var base *url.URL
	if u, err := url.Parse(baseURL); err == nil && u.IsAbs() {
		base = u
	}

	var results []SearchResult
	for _, m := range anchorRe.FindAllStringSubmatch(body, -1) {
		attrs := parseAttrs(m[1])
		if !hasClass(attrs, "result-link") {
			continue
		}
		href := unwrapDDGRedirect(strings.TrimSpace(attrs["href"]), base)
		if href == "" {
			continue
		}
		title := collapseInline(stdhtml.UnescapeString(stripTags(m[2])))
		results = append(results, SearchResult{Rank: len(results) + 1, Title: title, URL: href})
		if max > 0 && len(results) >= max {
			break
		}
	}

	if len(results) == 0 {
		if marker, blocked := detectChallenge(body); blocked {
			return nil, newError(KindSearchBlocked, "search", baseURL,
				"DuckDuckGo served a bot challenge instead of results (matched %q); "+
					"wait before retrying", marker)
		}
		if !strings.Contains(strings.ToLower(body), "result-link") &&
			!strings.Contains(strings.ToLower(body), "no results") {
			return nil, newError(KindMalformed, "search", baseURL,
				"could not find any results in the DuckDuckGo response; the page layout may have changed")
		}
		return nil, nil
	}

	snippets := make([]string, 0, len(results))
	for _, m := range ddgTDRe.FindAllStringSubmatch(body, -1) {
		if !hasClass(parseAttrs(m[1]), "result-snippet") {
			continue
		}
		snippets = append(snippets, collapseInline(stdhtml.UnescapeString(stripTags(m[2]))))
	}
	for i := range results {
		if i < len(snippets) {
			results[i].Snippet = snippets[i]
		}
	}
	return results, nil
}

// detectChallenge reports whether the body looks like a bot-block page.
func detectChallenge(body string) (string, bool) {
	lower := strings.ToLower(body)
	for _, marker := range challengeMarkers {
		if strings.Contains(lower, marker) {
			return marker, true
		}
	}
	return "", false
}

// unwrapDDGRedirect turns a DuckDuckGo click-tracking link into the real
// destination. The lite endpoint usually emits direct URLs, but it still falls
// back to /l/?uddg=… for some results, and an unwrapped redirect URL is not
// useful to a model.
func unwrapDDGRedirect(href string, base *url.URL) string {
	if href == "" {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil && !u.IsAbs() {
		u = base.ResolveReference(u)
	}
	if strings.HasPrefix(u.Path, "/l/") || strings.HasSuffix(u.Path, "/l/") {
		if target := u.Query().Get("uddg"); target != "" {
			if t, err := url.Parse(target); err == nil && t.IsAbs() {
				return t.String()
			}
		}
	}
	if !u.IsAbs() {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.String()
	}
	return ""
}

// instantAnswer is the subset of the DuckDuckGo Instant Answer API response
// Boop uses.
type instantAnswer struct {
	Heading        string `json:"Heading"`
	AbstractText   string `json:"AbstractText"`
	AbstractURL    string `json:"AbstractURL"`
	AbstractSource string `json:"AbstractSource"`
	Answer         string `json:"Answer"`
	Definition     string `json:"Definition"`
	RelatedTopics  []struct {
		FirstURL string `json:"FirstURL"`
		Text     string `json:"Text"`
		// Topics holds a nested group when DuckDuckGo categorises the
		// related entities.
		Topics []struct {
			FirstURL string `json:"FirstURL"`
			Text     string `json:"Text"`
		} `json:"Topics"`
	} `json:"RelatedTopics"`
}

// related flattens the related-topic tree into at most max results.
func (a *instantAnswer) related(max int) []SearchResult {
	if max <= 0 {
		max = config.DefaultSearchMaxResults
	}
	var out []SearchResult
	add := func(u, text string) {
		if u == "" || len(out) >= max {
			return
		}
		out = append(out, SearchResult{Rank: len(out) + 1, Title: text, URL: u, Snippet: text})
	}
	for _, t := range a.RelatedTopics {
		add(t.FirstURL, t.Text)
		for _, n := range t.Topics {
			add(n.FirstURL, n.Text)
		}
	}
	return out
}

// instantAnswer queries the official Instant Answer API.
func (d *DuckDuckGo) instantAnswer(ctx context.Context, query string, opts SearchOptions) (*instantAnswer, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("no_html", "1")
	q.Set("skip_disambig", "1")
	if kp := safeSearchParam(opts.SafeSearch); kp != "" {
		q.Set("kp", kp)
	}
	if opts.Region != "" {
		q.Set("kl", opts.Region)
	}

	resp, err := d.client.do(ctx, request{
		op:         "search",
		method:     http.MethodGet,
		rawURL:     d.instant() + "?" + q.Encode(),
		accept:     "application/json,text/javascript;q=0.9,*/*;q=0.5",
		skipRobots: true,
		maxBytes:   maxSearchBytes,
		strictSize: true,
	})
	if err != nil {
		return nil, err
	}
	var ia instantAnswer
	if err := json.Unmarshal(resp.Body, &ia); err != nil {
		return nil, wrapError(KindMalformed, "search", resp.FinalURL, err,
			"instant answer response was not valid JSON")
	}
	return &ia, nil
}
