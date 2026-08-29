package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/webclient"
)

// webTestClient builds a webclient allowed to reach a local httptest server.
// Private networks must be opened explicitly, since the guard blocks loopback
// by default — which is the whole point of the guard.
func webTestClient(t *testing.T, enabled bool) *webclient.Client {
	t.Helper()
	cfg := config.DefaultNetwork()
	cfg.Enabled = enabled
	cfg.AllowPrivateNetworks = true
	cfg.RespectRobots = false
	c, err := webclient.New(cfg, webclient.WithMinHostInterval(0))
	if err != nil {
		t.Fatalf("webclient.New() = %v", err)
	}
	return c
}

func webCall(t *testing.T, name string, args any) Call {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Call{ID: "call_1", Name: name, Arguments: raw}
}

func TestFetchToolExtractsReadableText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Boop Docs</title>
<meta name="description" content="How Boop works">
<style>.x{color:red}</style></head>
<body><script>console.log("noise")</script>
<h1>Getting started</h1><p>Install &amp; run boop.</p>
<a href="/next">Next page</a></body></html>`))
	}))
	defer srv.Close()

	tool := NewFetchTool(webTestClient(t, true))
	res, err := tool.Execute(context.Background(), webCall(t, "fetch", map[string]any{
		"url": srv.URL, "include_links": true,
	}))
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	data, ok := res.Data.(FetchData)
	if !ok {
		t.Fatalf("Data = %T, want FetchData", res.Data)
	}
	if data.Title != "Boop Docs" {
		t.Errorf("Title = %q, want %q", data.Title, "Boop Docs")
	}
	if !strings.Contains(data.Text, "Getting started") {
		t.Errorf("text missing heading: %q", data.Text)
	}
	// Entity decoding matters: a model reading "&amp;" learns the wrong thing.
	if !strings.Contains(data.Text, "Install & run boop") {
		t.Errorf("entities not decoded: %q", data.Text)
	}
	for _, noise := range []string{"console.log", "color:red"} {
		if strings.Contains(data.Text, noise) {
			t.Errorf("script/style leaked into text: %q", data.Text)
		}
	}
	if len(data.Links) == 0 {
		t.Error("include_links was set but no links were returned")
	}
}

func TestFetchToolRawReturnsBodyUnextracted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"n":42}`))
	}))
	defer srv.Close()

	tool := NewFetchTool(webTestClient(t, true))
	res, _ := tool.Execute(context.Background(), webCall(t, "fetch", map[string]any{
		"url": srv.URL, "raw": true,
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"n":42`) {
		t.Errorf("raw JSON body not returned verbatim: %s", res.Content)
	}
}

// The toggle is the feature: with it off, nothing reaches the network and the
// model is told how to turn it on rather than seeing an opaque failure.
func TestWebToolsRefuseWhenDisabled(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()
	client := webTestClient(t, false)

	fetch := NewFetchTool(client)
	res, err := fetch.Execute(context.Background(), webCall(t, "fetch", map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if !res.IsError {
		t.Fatal("fetch succeeded while outbound access was disabled")
	}
	if !strings.Contains(res.Content, "network.enabled") {
		t.Errorf("refusal should say how to enable access, got: %s", res.Content)
	}

	search := NewWebSearchTool(client)
	res, _ = search.Execute(context.Background(), webCall(t, "websearch", map[string]any{"query": "anything"}))
	if !res.IsError {
		t.Fatal("websearch succeeded while outbound access was disabled")
	}
	if reached {
		t.Error("a request reached the network despite the toggle being off")
	}
}

func TestFetchToolPermissionClassification(t *testing.T) {
	tool := NewFetchTool(webTestClient(t, true))
	tests := []struct {
		name string
		url  string
		want permissions.Risk
	}{
		{"https is ordinary", "https://example.com/docs", permissions.RiskLow},
		{"plain http is downgraded trust", "http://example.com/docs", permissions.RiskMedium},
		{"embedded credentials are never routine", "https://user:pw@example.com/", permissions.RiskHigh},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			act, err := tool.Permission(webCall(t, "fetch", map[string]any{"url": tc.url}))
			if err != nil {
				t.Fatalf("Permission() = %v", err)
			}
			if act.Category != permissions.CatNetworkFetch {
				t.Errorf("Category = %s, want %s", act.Category, permissions.CatNetworkFetch)
			}
			if act.Risk != tc.want {
				t.Errorf("Risk = %s, want %s", act.Risk, tc.want)
			}
			if act.Summary == "" {
				t.Error("Summary must be populated for the approval prompt")
			}
		})
	}
}

// The query is the disclosure, so it must reach the approval prompt verbatim.
func TestWebSearchPermissionExposesTheQuery(t *testing.T) {
	tool := NewWebSearchTool(webTestClient(t, true))
	const query = "how to rotate database credentials"
	act, err := tool.Permission(webCall(t, "websearch", map[string]any{"query": query}))
	if err != nil {
		t.Fatalf("Permission() = %v", err)
	}
	if act.Category != permissions.CatNetworkSearch {
		t.Errorf("Category = %s, want %s", act.Category, permissions.CatNetworkSearch)
	}
	if act.Detail != query {
		t.Errorf("Detail = %q, want the full query %q", act.Detail, query)
	}
	if !strings.Contains(act.Summary, "rotate database credentials") {
		t.Errorf("Summary should show what is being sent, got %q", act.Summary)
	}
}

func TestWebToolsRejectEmptyArguments(t *testing.T) {
	client := webTestClient(t, true)
	if _, err := NewFetchTool(client).Permission(webCall(t, "fetch", map[string]any{"url": "  "})); err == nil {
		t.Error("empty url should be rejected")
	}
	if _, err := NewWebSearchTool(client).Permission(webCall(t, "websearch", map[string]any{"query": ""})); err == nil {
		t.Error("empty query should be rejected")
	}
}

func TestWebToolSchemasAreWellFormed(t *testing.T) {
	client := webTestClient(t, true)
	for _, tool := range []Tool{NewFetchTool(client), NewWebSearchTool(client)} {
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Errorf("%s: schema type = %v, want object", tool.Name(), schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("%s: schema has no properties", tool.Name())
		}
		if _, err := json.Marshal(schema); err != nil {
			t.Errorf("%s: schema is not serialisable: %v", tool.Name(), err)
		}
		if tool.Description() == "" {
			t.Errorf("%s: description is empty", tool.Name())
		}
	}
}

func TestWebToolsRegisterAlongsideWorkspaceSearch(t *testing.T) {
	// websearch must not collide with the workspace content search tool.
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := webTestClient(t, true)
	reg := NewRegistry()
	reg.Register(NewSearchTool(ws))
	reg.Register(NewFetchTool(client))
	reg.Register(NewWebSearchTool(client))

	names := reg.Names()
	want := map[string]bool{"search": false, "fetch": false, "websearch": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("tool %q missing from registry: %v", n, names)
		}
	}
	if defs := reg.Definitions(nil); len(defs) != 3 {
		t.Errorf("Definitions() returned %d, want 3", len(defs))
	}
}
