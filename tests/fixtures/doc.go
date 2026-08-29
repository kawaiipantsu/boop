// Package fixtures provides the reusable provider test harness required by
// PROJECT.md §42: a scriptable fake model server plus a deterministic fake
// provider.Provider, so no adapter, agent flow or end-to-end test ever has to
// talk to a live — let alone paid — model server (§41).
//
// # Two independent tools
//
// [Server] is an httptest-backed HTTP server that emulates the wire formats
// Boop's adapters speak:
//
//   - OpenAI-compatible: GET /v1/models, POST /v1/chat/completions, both
//     non-streaming JSON and SSE. Also mounted under /api/v1 because Lemonade
//     serves the OpenAI-compatible surface from that prefix.
//   - Anthropic-shaped: POST /v1/messages, including its very different SSE
//     event framing (named events, no [DONE] sentinel).
//   - Ollama-native: GET /api/tags, POST /api/show.
//   - LM Studio-native: GET /api/v0/models.
//   - Lemonade lifecycle: POST /api/v1/{load,unload,pull}.
//   - Health probes: GET /, /health, /api/health, /api/v1/health.
//
// Behaviour is driven by a queue of [Response] values consumed one per chat
// request, so a test can script a multi-turn exchange. Every request is
// captured ([Server.Requests]) so tests can assert exactly what the adapter
// put on the wire. Failure injection covers HTTP status codes, malformed
// bodies, truncated streams, mid-stream connection drops and delays.
//
// [FakeProvider] implements provider.Provider directly with no HTTP at all.
// It replays scripted [Turn] values and is the right tool for end-to-end
// tests of the agent loop, where the HTTP wire format is irrelevant and
// determinism is everything.
//
// # Typical use
//
//	srv := fixtures.NewServer(t)
//	srv.Enqueue(fixtures.TextResponse("hello").WithUsage(10, 3))
//	adapter := openaicompat.New(srv.URL(), ...)
//	// ... exercise the adapter, then:
//	req := srv.MustLastChatRequest(t)
//	if req.Model != "boop-test-model" { t.Fatalf("...") }
//
// The server is registered with the test's Cleanup, so callers never need to
// close it explicitly.
//
// # No secrets
//
// Nothing here contains or requires a real credential (§45). [WithAPIKey]
// exists only so adapters can prove they send an Authorization header; the
// value passed must be a dummy supplied by the test.
package fixtures
