package fixtures_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/tests/fixtures"
)

func TestInjectedHTTPStatusCodes(t *testing.T) {
	cases := []struct {
		status   int
		wantType string
	}{
		{http.StatusUnauthorized, "invalid_request_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "server_error"},
		{http.StatusBadRequest, "invalid_request_error"},
	}
	for _, tc := range cases {
		srv := fixtures.NewServer(t)
		srv.Enqueue(fixtures.ErrorResponse(tc.status, "boom").WithHeader("Retry-After", "3"))

		resp := post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`)
		if resp.StatusCode != tc.status {
			t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
		}
		if got := resp.Header.Get("Retry-After"); got != "3" {
			t.Errorf("Retry-After = %q", got)
		}
		var out struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		decode(t, resp, &out)
		if out.Error.Message != "boom" {
			t.Errorf("message = %q", out.Error.Message)
		}
		if out.Error.Type != tc.wantType {
			t.Errorf("status %d: type = %q, want %q", tc.status, out.Error.Type, tc.wantType)
		}
	}
}

func TestMalformedResponseBody(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.MalformedResponse(`{"choices":[{"message":`))

	resp := post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (malformed bodies must not be signalled by status)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != `{"choices":[{"message":` {
		t.Fatalf("body = %q", body)
	}
}

func TestMalformedStreamBody(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.MalformedResponse("data: {\"choices\":[oops\n\n"))

	resp := post(t, srv, "/v1/chat/completions", streamReq)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	frames, err := fixtures.ReadSSE(resp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %+v", frames)
	}
	if _, err := fixtures.ReassembleOpenAIStream(frames); err == nil {
		t.Fatal("reassembling a malformed frame should fail")
	}
}

func TestTruncatedStream(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("one", "two", "three", "four").
		TruncateAfterFrames(3))

	frames := streamChat(t, srv, streamReq)
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	for _, f := range frames {
		if f.Data == "[DONE]" {
			t.Fatal("truncated stream must not send [DONE]")
		}
	}
	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if sum.Done {
		t.Error("summary should report an unterminated stream")
	}
	if sum.Text != "onetwo" {
		t.Errorf("text = %q, want the prefix that arrived", sum.Text)
	}
}

func TestConnectionDropMidStream(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("one", "two", "three", "four").
		DropAfterFrames(3))

	resp := post(t, srv, "/v1/chat/completions", streamReq)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// A chunked body cut short is an unexpected EOF, which is exactly what an
	// adapter must classify as a malformed/unavailable response rather than a
	// successful short answer.
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("reading a dropped stream should fail")
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Logf("drop produced %v (accepted: any read error)", err)
	}
}

func TestResponseDelayAndContextTimeout(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("late").WithDelay(500 * time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Client().Do(req); err == nil {
		t.Fatal("expected the delayed response to exceed the context deadline")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
}

func TestChunkDelayPacesStream(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("a", "b", "c").
		WithChunkDelay(15 * time.Millisecond))

	start := time.Now()
	streamChat(t, srv, streamReq)
	// Six frames at 15ms each; assert conservatively so the test is not flaky.
	if elapsed := time.Since(start); elapsed < 45*time.Millisecond {
		t.Errorf("stream completed in %v, chunk delay was not applied", elapsed)
	}
}

func TestPathFailureInjection(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.SetPathFailure("/v1/models", http.StatusServiceUnavailable, `{"error":"model server starting"}`)

	resp := get(t, srv, "/v1/models")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Chat is unaffected: the injection is endpoint-scoped.
	srv.EnqueueText("still fine")
	chat := post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	chat.Body.Close()
	if chat.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", chat.StatusCode)
	}

	srv.ClearPathFailure("/v1/models")
	resp = get(t, srv, "/v1/models")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after clearing: status = %d", resp.StatusCode)
	}
}

func TestGlobalLatency(t *testing.T) {
	srv := fixtures.NewServer(t, fixtures.WithLatency(20*time.Millisecond))
	start := time.Now()
	get(t, srv, "/v1/models").Body.Close()
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("request returned in %v, latency was not applied", elapsed)
	}
	srv.SetLatency(0)
}

func TestAPIKeyEnforcement(t *testing.T) {
	const key = "dummy-test-key"
	srv := fixtures.NewServer(t, fixtures.WithAPIKey(key))
	srv.EnqueueText("authorized").EnqueueText("authorized")

	resp := get(t, srv, "/v1/models")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	authed, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	authed.Body.Close()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("bearer status = %d", authed.StatusCode)
	}

	// Anthropic's header scheme is accepted too, since one server stands in
	// for every vendor.
	xkey := post(t, srv, "/v1/messages", `{"model":"m","max_tokens":10,"messages":[]}`, "x-api-key", key)
	xkey.Body.Close()
	if xkey.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key status = %d", xkey.StatusCode)
	}

	bad := post(t, srv, "/v1/messages", `{"model":"m","max_tokens":10,"messages":[]}`, "x-api-key", "wrong")
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d", bad.StatusCode)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decode(t, bad, &out)
	if out.Type != "error" || out.Error.Type != "authentication_error" {
		t.Errorf("anthropic 401 envelope = %+v", out)
	}
}

func TestUnscriptedRequestUsesDefaultAndNilDefaultFails(t *testing.T) {
	// With a nil default, an unscripted call must be reported as a test
	// failure rather than silently answered.
	rec := &recordingTB{T: t}
	srv := fixtures.NewServer(rec, fixtures.WithDefaultResponse(nil))
	resp := post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if len(rec.errors) == 0 {
		t.Fatal("an unscripted request should have failed the test")
	}
	if !strings.Contains(rec.errors[0], "empty response queue") {
		t.Errorf("error = %q", rec.errors[0])
	}
}

// recordingTB captures the harness's own failure reports so a test can assert
// on them without failing itself.
type recordingTB struct {
	*testing.T
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.errors = append(r.errors, sprintf(format, args...))
}

func sprintf(format string, args ...any) string {
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}
