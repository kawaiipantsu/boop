package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/webclient"
)

// WebSearchTool runs a web search and returns ranked results.
//
// It is named websearch rather than search because search is already taken by
// the workspace content search: a model choosing between "grep this repository"
// and "ask the internet" should not have to disambiguate by argument shape.
//
// Searching is classified separately from fetching because it discloses the
// user's query text — often the most sensitive thing in a session — to a
// third-party engine.
type WebSearchTool struct {
	client *webclient.Client
}

// NewWebSearchTool returns a search tool backed by client.
func NewWebSearchTool(client *webclient.Client) *WebSearchTool {
	return &WebSearchTool{client: client}
}

// WebSearchData is the structured payload attached to the tool Result.
type WebSearchData struct {
	Query    string                   `json:"query"`
	Provider string                   `json:"provider"`
	Results  []webclient.SearchResult `json:"results"`
	Heading  string                   `json:"heading,omitempty"`
	Abstract string                   `json:"abstract,omitempty"`
	// AbstractURL sources the abstract, so a model can cite or follow it.
	AbstractURL string `json:"abstract_url,omitempty"`
	Answer      string `json:"answer,omitempty"`
	Definition  string `json:"definition,omitempty"`
}

// webSearchArgs are the decoded arguments of the websearch tool.
type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
	SafeSearch string `json:"safe_search,omitempty"`
	Region     string `json:"region,omitempty"`
}

// Name implements Tool.
func (t *WebSearchTool) Name() string { return "websearch" }

// Description implements Tool.
func (t *WebSearchTool) Description() string {
	return "Search the web with DuckDuckGo and return ranked results with titles, URLs " +
		"and snippets, plus an instant answer when one exists. Use the fetch tool " +
		"afterwards to read a result in full. Requires outbound web access to be " +
		"enabled in the Boop configuration."
}

// Schema implements Tool.
func (t *WebSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query.",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return.",
			},
			"safe_search": map[string]any{
				"type":        "string",
				"enum":        []string{"off", "moderate", "strict"},
				"description": "Safe search level.",
			},
			"region": map[string]any{
				"type":        "string",
				"description": "DuckDuckGo region code such as wt-wt or dk-da.",
			},
		},
		"required": []string{"query"},
	}
}

// Permission implements Tool.
//
// The query itself is the disclosure, so it goes in Detail where an approval
// prompt will show the user exactly what would be sent.
func (t *WebSearchTool) Permission(call Call) (permissions.Action, error) {
	var args webSearchArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return permissions.Action{}, errors.New("websearch: query is required")
	}
	return permissions.Action{
		Category: permissions.CatNetworkSearch,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Search the web for %q", webTruncate(query, 60)),
		Detail:   query,
	}, nil
}

// Execute implements Tool.
func (t *WebSearchTool) Execute(ctx context.Context, call Call) (Result, error) {
	started := time.Now()
	var args webSearchArgs
	if err := call.Bind(&args); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return Errorf(call, "query is required"), nil
	}

	resp, err := t.client.Search(ctx, query, webclient.SearchOptions{
		MaxResults: args.MaxResults,
		SafeSearch: args.SafeSearch,
		Region:     args.Region,
	})
	if err != nil {
		return webFailure(call, err, started), nil
	}

	data := WebSearchData{
		Query: resp.Query, Provider: resp.Provider, Results: resp.Results,
		Heading: resp.Heading, Abstract: resp.Abstract,
		AbstractURL: resp.AbstractURL, Answer: resp.Answer, Definition: resp.Definition,
	}
	return Result{
		CallID: call.ID, Tool: call.Name, Data: data,
		Duration: time.Since(started),
		IsError:  len(resp.Results) == 0 && resp.Abstract == "" && resp.Answer == "",
		Content:  webRenderSearch(data),
	}, nil
}

// webRenderSearch formats search results for the model.
func webRenderSearch(d WebSearchData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "query: %s\nprovider: %s\n", d.Query, d.Provider)

	if d.Answer != "" {
		fmt.Fprintf(&b, "--- instant answer ---\n%s\n", d.Answer)
	}
	if d.Definition != "" {
		fmt.Fprintf(&b, "--- definition ---\n%s\n", d.Definition)
	}
	if d.Abstract != "" {
		fmt.Fprintf(&b, "--- summary ---\n")
		if d.Heading != "" {
			fmt.Fprintf(&b, "%s\n", d.Heading)
		}
		fmt.Fprintf(&b, "%s\n", d.Abstract)
		if d.AbstractURL != "" {
			fmt.Fprintf(&b, "source: %s\n", d.AbstractURL)
		}
	}

	if len(d.Results) == 0 {
		b.WriteString("--- results ---\n(no results; try different search terms)\n--- end ---")
		return b.String()
	}
	fmt.Fprintf(&b, "--- results (%d) ---\n", len(d.Results))
	for _, r := range d.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", r.Rank, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	b.WriteString("--- end ---")
	return b.String()
}

// webTruncate shortens s for display in an approval prompt.
func webTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
