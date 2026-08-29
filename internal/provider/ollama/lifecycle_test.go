package ollama

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestLoadAndUnloadModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		call          func(*Client) error
		wantKeepAlive any
		reply         string
	}{
		{
			name:          "load asks for the configured residency",
			call:          func(c *Client) error { return c.LoadModel(context.Background(), "llama3.1:8b") },
			wantKeepAlive: "1m0s",
			reply:         `{"model":"llama3.1:8b","response":"","done":true,"done_reason":"load"}`,
		},
		{
			name:          "unload asks for zero residency",
			call:          func(c *Client) error { return c.UnloadModel(context.Background(), "llama3.1:8b") },
			wantKeepAlive: float64(0),
			reply:         `{"model":"llama3.1:8b","response":"","done":true,"done_reason":"unload"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.static(generatePath, tc.reply)
			srv, _ := f.start()
			c := New(srv.URL, srv.Client(), WithKeepAlive(time.Minute))

			if err := tc.call(c); err != nil {
				t.Fatalf("lifecycle call: %v", err)
			}

			body := f.lastBody(generatePath)
			if got := body["model"]; got != "llama3.1:8b" {
				t.Fatalf("model = %v", got)
			}
			if got := body["keep_alive"]; got != tc.wantKeepAlive {
				t.Fatalf("keep_alive = %#v, want %#v", got, tc.wantKeepAlive)
			}
			if got := body["stream"]; got != false {
				t.Fatalf("stream = %#v, want false (a streamed reply is not decodable here)", got)
			}
			if _, sent := body["prompt"]; sent {
				t.Fatal("an empty prompt is what makes this a pure load; none must be sent")
			}
		})
	}
}

func TestLifecycleErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		route   func(*fake)
		model   string
		want    provider.ErrorCategory
		wantMsg string
	}{
		{
			name:  "empty model id",
			route: func(f *fake) { f.static(generatePath, `{"done":true}`) },
			model: "  ",
			want:  provider.ErrInvalidRequest,
		},
		{
			name: "unknown model",
			route: func(f *fake) {
				f.status(generatePath, http.StatusNotFound, `{"error":"model 'ghost' not found"}`)
			},
			model:   "ghost",
			want:    provider.ErrInvalidRequest,
			wantMsg: "model 'ghost' not found",
		},
		{
			name:  "server reports an error inside a 200",
			route: func(f *fake) { f.static(generatePath, `{"error":"out of memory"}`) },
			model: "llama3.1:8b",
			want:  provider.ErrServer,
		},
		{
			name:  "unconfirmed load is not reported as success",
			route: func(f *fake) { f.static(generatePath, `{"model":"llama3.1:8b","done":false}`) },
			model: "llama3.1:8b",
			want:  provider.ErrMalformedResponse,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			tc.route(f)
			_, c := f.start()

			pe := providerError(t, c.LoadModel(context.Background(), tc.model))
			if pe.Category != tc.want {
				t.Fatalf("category = %q, want %q", pe.Category, tc.want)
			}
			if tc.wantMsg != "" && pe.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", pe.Message, tc.wantMsg)
			}
		})
	}
}

func TestLifecycleUnreachableServer(t *testing.T) {
	t.Parallel()

	c := unreachableClient(t)
	pe := providerError(t, c.LoadModel(context.Background(), "llama3.1:8b"))
	if pe.Category != provider.ErrUnavailable {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
	}
}

func TestKeepAliveValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want any
	}{
		{"positive renders as a duration string", 5 * time.Minute, "5m0s"},
		{"zero means unload now", 0, 0},
		{"negative means unload now", -time.Second, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := keepAliveValue(tc.in); got != tc.want {
				t.Fatalf("keepAliveValue(%v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
