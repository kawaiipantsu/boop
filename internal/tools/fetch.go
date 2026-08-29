package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/webclient"
)

// FetchTool retrieves a web page and returns it as readable text.
//
// It exists alongside the lower-level http tool because a model asking "what
// does this page say" wants prose, not markup: raw HTML wastes most of the
// context window on tags. Boilerplate is stripped and the result is capped.
//
// Outbound access is governed entirely by the webclient, which enforces the
// network.enabled toggle, the SSRF guard and robots.txt. This type adds the
// tool surface and the permission classification, and nothing else.
type FetchTool struct {
	client *webclient.Client
}

// NewFetchTool returns a fetch tool backed by client.
func NewFetchTool(client *webclient.Client) *FetchTool {
	return &FetchTool{client: client}
}

// FetchData is the structured payload attached to the tool Result.
type FetchData struct {
	RequestURL  string   `json:"request_url"`
	FinalURL    string   `json:"final_url"`
	StatusCode  int      `json:"status_code"`
	ContentType string   `json:"content_type,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Canonical   string   `json:"canonical,omitempty"`
	Text        string   `json:"text"`
	Links       []string `json:"links,omitempty"`
	Truncated   bool     `json:"truncated"`
	Redirects   []string `json:"redirects,omitempty"`
}

// webFetchArgs are the decoded arguments of the fetch tool.
type webFetchArgs struct {
	URL string `json:"url"`
	// Raw returns the undecoded body instead of extracted text, for
	// non-HTML resources such as JSON or plain text.
	Raw bool `json:"raw,omitempty"`
	// MaxChars caps the returned text. Zero uses the extractor default.
	MaxChars int `json:"max_chars,omitempty"`
	// IncludeLinks appends the page's outbound links.
	IncludeLinks bool `json:"include_links,omitempty"`
}

// Name implements Tool.
func (t *FetchTool) Name() string { return "fetch" }

// Description implements Tool.
func (t *FetchTool) Description() string {
	return "Fetch a web page and return its readable text content, with the title and " +
		"optionally its links. Use this to read documentation, articles or any URL. " +
		"Set raw to true for non-HTML resources such as JSON. Requires outbound web " +
		"access to be enabled in the Boop configuration."
}

// Schema implements Tool.
func (t *FetchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Absolute http or https URL to fetch.",
			},
			"raw": map[string]any{
				"type":        "boolean",
				"description": "Return the raw body instead of extracted text. Use for JSON or plain text.",
			},
			"max_chars": map[string]any{
				"type":        "integer",
				"description": "Maximum characters of text to return.",
			},
			"include_links": map[string]any{
				"type":        "boolean",
				"description": "Include the page's outbound links.",
			},
		},
		"required": []string{"url"},
	}
}

// Permission implements Tool.
//
// Fetching discloses the target URL to a third-party server and pulls back
// content the model will act on, so it is classified separately from a plain
// scripted HTTP call.
func (t *FetchTool) Permission(call Call) (permissions.Action, error) {
	var args webFetchArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	target := strings.TrimSpace(args.URL)
	if target == "" {
		return permissions.Action{}, errors.New("fetch: url is required")
	}
	return permissions.Action{
		Category: permissions.CatNetworkFetch,
		Risk:     webRiskForURL(target),
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Fetch %s", webDisplayURL(target)),
		Detail:   target,
	}, nil
}

// Execute implements Tool.
func (t *FetchTool) Execute(ctx context.Context, call Call) (Result, error) {
	started := time.Now()
	var args webFetchArgs
	if err := call.Bind(&args); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	target := strings.TrimSpace(args.URL)
	if target == "" {
		return Errorf(call, "url is required"), nil
	}

	if args.Raw {
		resp, err := t.client.Fetch(ctx, target)
		if err != nil && resp == nil {
			return webFailure(call, err, started), nil
		}
		body := resp.Text
		if !resp.CharsetSupported {
			return Errorf(call, "response is in an unsupported character set (%s); cannot decode it to text", resp.Charset), nil
		}
		if args.MaxChars > 0 && len(body) > args.MaxChars {
			body = body[:args.MaxChars] + "\n[truncated]"
		}
		data := FetchData{
			RequestURL: resp.RequestURL, FinalURL: resp.FinalURL,
			StatusCode: resp.Status, ContentType: resp.ContentType,
			Text: body, Truncated: resp.Truncated, Redirects: resp.Redirects,
		}
		return Result{
			CallID: call.ID, Tool: call.Name, Data: data, Duration: time.Since(started),
			IsError: err != nil,
			Content: webRenderFetch(data, err),
		}, nil
	}

	page, err := t.client.FetchPage(ctx, target)
	if err != nil && page == nil {
		return webFailure(call, err, started), nil
	}
	doc := page.Document
	text := doc.Text
	if args.MaxChars > 0 && len(text) > args.MaxChars {
		text = text[:args.MaxChars]
		doc.Truncated = true
	}
	data := FetchData{
		RequestURL:  page.Response.RequestURL,
		FinalURL:    page.Response.FinalURL,
		StatusCode:  page.Response.Status,
		ContentType: page.Response.ContentType,
		Title:       doc.Title,
		Description: doc.Description,
		Canonical:   doc.Canonical,
		Text:        text,
		Truncated:   doc.Truncated || page.Response.Truncated,
		Redirects:   page.Response.Redirects,
	}
	if args.IncludeLinks {
		for _, l := range doc.Links {
			data.Links = append(data.Links, l.URL)
		}
	}
	return Result{
		CallID: call.ID, Tool: call.Name, Data: data, Duration: time.Since(started),
		IsError: err != nil,
		Content: webRenderFetch(data, err),
	}, nil
}

// webRenderFetch formats a fetch result for the model.
func webRenderFetch(d FetchData, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "url: %s\n", d.FinalURL)
	if d.FinalURL != d.RequestURL && d.RequestURL != "" {
		fmt.Fprintf(&b, "requested: %s\n", d.RequestURL)
	}
	fmt.Fprintf(&b, "status: %d\n", d.StatusCode)
	if d.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", d.Title)
	}
	if d.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", d.Description)
	}
	if err != nil {
		fmt.Fprintf(&b, "note: %s\n", err)
	}
	b.WriteString("--- content ---\n")
	if strings.TrimSpace(d.Text) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(d.Text)
		if !strings.HasSuffix(d.Text, "\n") {
			b.WriteString("\n")
		}
	}
	if d.Truncated {
		b.WriteString("[truncated: raise max_chars to read more]\n")
	}
	if len(d.Links) > 0 {
		fmt.Fprintf(&b, "--- links (%d) ---\n", len(d.Links))
		for _, l := range d.Links {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	b.WriteString("--- end ---")
	return b.String()
}

// webFailure turns a webclient error into a model-readable failed Result.
//
// These are all conditions the model can respond to — by enabling access,
// choosing a different URL, or giving up — so none of them is a Go error.
func webFailure(call Call, err error, started time.Time) Result {
	var msg string
	switch webclient.KindOf(err) {
	case webclient.KindDisabled:
		msg = fmt.Sprintf("outbound web access is disabled; %s", webclient.EnableHint)
	case webclient.KindBlocked:
		msg = fmt.Sprintf("that address is blocked by Boop's network policy: %v", err)
	case webclient.KindRobotsDenied:
		msg = fmt.Sprintf("the site's robots.txt disallows fetching this URL: %v", err)
	case webclient.KindTimeout:
		msg = fmt.Sprintf("the request timed out: %v", err)
	case webclient.KindTooLarge:
		msg = fmt.Sprintf("the response was too large to read: %v", err)
	case webclient.KindSearchBlocked:
		msg = fmt.Sprintf("the search endpoint returned a challenge page instead of results: %v", err)
	case webclient.KindUnsupported:
		msg = fmt.Sprintf("unsupported request: %v", err)
	default:
		msg = err.Error()
	}
	return Result{
		CallID: call.ID, Tool: call.Name, IsError: true,
		Content: msg, Duration: time.Since(started),
	}
}

// webRiskForURL rates a fetch target.
//
// Reading a public page is ordinary, but a non-https URL is downgraded-trust
// traffic and a URL carrying credentials is never routine.
func webRiskForURL(raw string) permissions.Risk {
	lower := strings.ToLower(raw)
	if strings.Contains(raw, "@") && (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) {
		return permissions.RiskHigh
	}
	if strings.HasPrefix(lower, "http://") {
		return permissions.RiskMedium
	}
	return permissions.RiskLow
}

// webDisplayURL shortens a URL for an approval prompt.
func webDisplayURL(raw string) string {
	const max = 90
	if len(raw) <= max {
		return raw
	}
	return raw[:max-1] + "…"
}

// compile-time check that the payload stays serialisable for the WebUI.
var _ = func() bool { _, err := json.Marshal(FetchData{}); return err == nil }()
