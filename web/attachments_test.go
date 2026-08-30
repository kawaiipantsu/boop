package web

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
	"github.com/kawaiipantsu/boop/internal/stats"
	"github.com/kawaiipantsu/boop/tests/fixtures"
)

// newAttachmentServer wires a server to the scriptable fake provider, so a
// whole turn — attachment, capability check, model call — runs without a
// network or a paid API (§41).
func newAttachmentServer(t *testing.T, model string) (*Server, *fixtures.Server, *app.App) {
	t.Helper()
	fake := fixtures.NewServer(t)

	cfg := config.Default()
	cfg.Provider = "ollama"
	cfg.Model = model
	pc := cfg.Providers["ollama"]
	pc.Type = "ollama"
	pc.BaseURL = fake.URL()
	pc.Disabled = false
	cfg.Providers["ollama"] = pc

	application, err := app.New(t.Context(), app.Options{
		Config:       cfg,
		WorkingDir:   t.TempDir(),
		DatabasePath: ":memory:",
		Approver:     permissions.DenyAll(),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = cfg
	})
	return srv, fake, application
}

// tinyPNG encodes a 1x1 image, which is a real PNG as far as detection and
// header decoding are concerned.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// docxWithImage builds the smallest DOCX Boop's extractor recognises: a body
// with one paragraph and one image in word/media.
func docxWithImage(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	write("word/document.xml", []byte(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`+
		`<w:body><w:p><w:r><w:t>`+text+`</w:t></w:r></w:p></w:body></w:document>`))
	write("word/media/image1.png", tinyPNG(t))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// lastChatRequest decodes the most recent chat completion the model received.
func lastChatRequest(t *testing.T, fake *fixtures.Server) fixtures.ChatCompletionRequest {
	t.Helper()
	reqs := fake.RequestsTo("/v1/chat/completions")
	if len(reqs) == 0 {
		t.Fatal("the model was never called")
	}
	var body fixtures.ChatCompletionRequest
	if err := reqs[len(reqs)-1].JSON(&body); err != nil {
		t.Fatalf("decode chat request: %v", err)
	}
	return body
}

// TestMessageAttachesTextDocument: the extracted text reaches the model,
// labelled, and the transcript keeps it.
func TestMessageAttachesTextDocument(t *testing.T) {
	srv, fake, application := newAttachmentServer(t, "boop-test-model")
	fake.EnqueueText("I read your notes.")

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{
		Content: "summarise the attachment",
		Attachments: []attachmentRequest{
			{Filename: "notes.txt", Text: "the quick brown fox\njumped over the lazy dog\n"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/message = %d (body %s)", rec.Code, body)
	}
	var turn turnResponse
	if err := json.Unmarshal(body, &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if turn.Text != "I read your notes." {
		t.Errorf("text = %q", turn.Text)
	}
	if len(turn.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want one", turn.Attachments)
	}
	att := turn.Attachments[0]
	if att.Filename != "notes.txt" || !strings.HasPrefix(att.MIMEType, "text/") {
		t.Errorf("attachment = %+v, want a text/* notes.txt", att)
	}
	if att.Parts != 1 || att.Degraded {
		t.Errorf("attachment = %+v, want one undegraded part", att)
	}

	sent := lastChatRequest(t, fake)
	var user string
	for _, m := range sent.Messages {
		if m.Role == "user" {
			user = m.Text()
		}
	}
	if !strings.Contains(user, "the quick brown fox") {
		t.Errorf("the attachment never reached the model:\n%s", user)
	}
	if !strings.Contains(user, `<attachment filename="notes.txt"`) {
		t.Errorf("the attachment was not labelled, so the model cannot tell it from the prompt:\n%s", user)
	}
	if !strings.Contains(user, "summarise the attachment") {
		t.Errorf("the user's own words were lost:\n%s", user)
	}

	messages, err := application.Sessions.History().Messages(t.Context(), turn.SessionID, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	foundStored := false
	for _, m := range messages {
		if m.Role != provider.RoleUser {
			continue
		}
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "the quick brown fox") {
				foundStored = true
			}
		}
	}
	if !foundStored {
		t.Error("the transcript did not keep the attachment")
	}
}

// TestMessageAttachmentSizeLimits: every path that could put an unbounded
// upload into memory has a cap, and each says which one was hit.
func TestMessageAttachmentSizeLimits(t *testing.T) {
	srv, _, _ := newAttachmentServer(t, "boop-test-model")

	tests := []struct {
		name       string
		req        messageRequest
		wantStatus int
		wantIn     string
	}{
		{
			name: "one attachment over the per-file cap",
			req: messageRequest{
				Content: "read this",
				Attachments: []attachmentRequest{
					{Filename: "huge.txt", Text: strings.Repeat("a", maxAttachmentBytes+1)},
				},
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantIn:     "limit",
		},
		{
			name: "too many attachments",
			req: messageRequest{
				Content:     "read these",
				Attachments: manyAttachments(maxAttachments + 1),
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantIn:     "at most",
		},
		{
			name: "an attachment with no name",
			req: messageRequest{
				Content:     "read this",
				Attachments: []attachmentRequest{{Text: "no name"}},
			},
			wantStatus: http.StatusBadRequest,
			wantIn:     "filename",
		},
		{
			name: "an attachment with both bodies",
			req: messageRequest{
				Content:     "read this",
				Attachments: []attachmentRequest{{Filename: "a.txt", Text: "x", ContentBase64: "eA=="}},
			},
			wantStatus: http.StatusBadRequest,
			wantIn:     "exactly one",
		},
		{
			name: "malformed base64",
			req: messageRequest{
				Content:     "read this",
				Attachments: []attachmentRequest{{Filename: "a.txt", ContentBase64: "!!!not base64!!!"}},
			},
			wantStatus: http.StatusBadRequest,
			wantIn:     "base64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doJSON(t, srv, http.MethodPost, "/api/message", tc.req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, truncate(body))
			}
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(env.Error.Message, tc.wantIn) {
				t.Errorf("message = %q, want it to mention %q", env.Error.Message, tc.wantIn)
			}
		})
	}
}

// TestMessageBodyLimit: the declared length is refused before the body is
// read, so an oversized upload costs nothing.
func TestMessageBodyLimit(t *testing.T) {
	srv, _, _ := newAttachmentServer(t, "boop-test-model")

	req := httptest.NewRequest(http.MethodPost, "/api/message", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = maxUploadBytes + 1
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestImageAgainstTextOnlyModel is the §8 refusal: there is nothing to degrade
// an image-only attachment to, so the request is refused with the models that
// would have worked.
func TestImageAgainstTextOnlyModel(t *testing.T) {
	srv, fake, _ := newAttachmentServer(t, "boop-test-model")

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{
		Content:     "what is in this picture?",
		Attachments: []attachmentRequest{{Filename: "shot.png", ContentBase64: b64(tinyPNG(t))}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != codeUnsupportedCapability {
		t.Errorf("code = %q, want %q", env.Error.Code, codeUnsupportedCapability)
	}
	if !strings.Contains(env.Error.Message, "vision") {
		t.Errorf("message = %q, want it to name the missing capability", env.Error.Message)
	}
	if !containsString(env.Error.Details, "ollama/boop-test-vision") {
		t.Errorf("details = %v, want the vision-capable model suggested", env.Error.Details)
	}
	if len(fake.RequestsTo("/v1/chat/completions")) != 0 {
		t.Error("the refused message was sent to the model anyway")
	}
}

// TestImageAcceptedByVisionModel: the same attachment goes through unchanged
// when the model can see.
func TestImageAcceptedByVisionModel(t *testing.T) {
	srv, fake, _ := newAttachmentServer(t, "boop-test-vision")
	fake.EnqueueText("a single pixel")

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{
		Content:     "what is in this picture?",
		Attachments: []attachmentRequest{{Filename: "shot.png", ContentBase64: b64(tinyPNG(t))}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, body)
	}
	var turn turnResponse
	if err := json.Unmarshal(body, &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(turn.Attachments) != 1 || turn.Attachments[0].Degraded {
		t.Fatalf("attachments = %+v, want the image kept", turn.Attachments)
	}
	if len(turn.Notes) != 0 {
		t.Errorf("notes = %v, want none for a capable model", turn.Notes)
	}
	sent := lastChatRequest(t, fake)
	if !bytes.Contains(mustJSON(t, sent.Messages), []byte("image_url")) {
		t.Error("the image never reached the model")
	}
}

// TestDocumentImageDegradesForTextOnlyModel is the §27 promise: a text-only
// model still gets the textual content of a binary document, and is told what
// was withheld rather than being handed a silently shortened message.
func TestDocumentImageDegradesForTextOnlyModel(t *testing.T) {
	srv, fake, _ := newAttachmentServer(t, "boop-test-model")
	fake.EnqueueText("the document says hello")

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{
		Content:     "summarise the report",
		Attachments: []attachmentRequest{{Filename: "report.docx", ContentBase64: b64(docxWithImage(t, "hello from the report"))}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, body)
	}
	var turn turnResponse
	if err := json.Unmarshal(body, &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(turn.Attachments) != 1 || !turn.Attachments[0].Degraded {
		t.Fatalf("attachments = %+v, want the image dropped", turn.Attachments)
	}
	if len(turn.Notes) == 0 || !strings.Contains(strings.Join(turn.Notes, " "), "vision") {
		t.Fatalf("notes = %v, want an explanation of the dropped image", turn.Notes)
	}

	sent := lastChatRequest(t, fake)
	raw := mustJSON(t, sent.Messages)
	if bytes.Contains(raw, []byte("image_url")) {
		t.Error("an image was sent to a model with no vision capability")
	}
	var user string
	for _, m := range sent.Messages {
		if m.Role == "user" {
			user = m.Text()
		}
	}
	if !strings.Contains(user, "hello from the report") {
		t.Errorf("the extracted text did not reach the model:\n%s", user)
	}
	if !strings.Contains(user, "dropped") {
		t.Errorf("the model was not told anything was withheld:\n%s", user)
	}
}

// TestDegradeFalseReportsTheCapabilityError: a caller that would rather be
// told than be helped can ask for the §8 error instead.
func TestDegradeFalseReportsTheCapabilityError(t *testing.T) {
	srv, _, _ := newAttachmentServer(t, "boop-test-model")
	no := false

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{
		Content:     "summarise the report",
		Degrade:     &no,
		Attachments: []attachmentRequest{{Filename: "report.docx", ContentBase64: b64(docxWithImage(t, "hello"))}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != codeUnsupportedCapability {
		t.Errorf("code = %q, want %q", env.Error.Code, codeUnsupportedCapability)
	}
}

// TestMultipartUpload: a browser file input posts multipart, not JSON.
func TestMultipartUpload(t *testing.T) {
	srv, fake, _ := newAttachmentServer(t, "boop-test-model")
	fake.EnqueueText("read it")

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.WriteField("content", "what does this say?"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := form.CreateFormFile("file", "hello.md")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := part.Write([]byte("# heading\n\nbody text\n")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/message", &buf)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var turn turnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(turn.Attachments) != 1 || turn.Attachments[0].Filename != "hello.md" {
		t.Fatalf("attachments = %+v, want hello.md", turn.Attachments)
	}
	sent := lastChatRequest(t, fake)
	var user string
	for _, m := range sent.Messages {
		if m.Role == "user" {
			user = m.Text()
		}
	}
	if !strings.Contains(user, "body text") {
		t.Errorf("the uploaded file never reached the model:\n%s", user)
	}
}

// TestUnsupportedAttachmentType: a format Boop cannot read is refused with a
// reason, not attached as garbage.
func TestUnsupportedAttachmentType(t *testing.T) {
	srv, _, _ := newAttachmentServer(t, "boop-test-model")

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{
		Content:     "read this",
		Attachments: []attachmentRequest{{Filename: "legacy.doc", ContentBase64: b64([]byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0, 0})}},
	})
	if rec.Code != http.StatusUnsupportedMediaType && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 415 or 422 (body %s)", rec.Code, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Message == "" {
		t.Error("the refusal explained nothing")
	}
}

// TestStatsUseTheAppTracker: /api/stats works without the caller remembering
// to pass Options.Stats, because the runtime already owns one.
func TestStatsUseTheAppTracker(t *testing.T) {
	application := newTestApp(t)
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config()
	})
	if srv.stats != application.Stats {
		t.Fatal("the server did not adopt the runtime's stats tracker")
	}

	rec, body := doJSON(t, srv, http.MethodGet, "/api/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, body)
	}
	var resp statsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Snapshot == nil {
		t.Fatal("no snapshot; /api/stats fell back to persisted totals with a tracker available")
	}
}

// TestStatsOptionOverridesTheAppTracker keeps the explicit option meaningful.
func TestStatsOptionOverridesTheAppTracker(t *testing.T) {
	application := newTestApp(t)
	tracker := stats.New()
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config()
		o.Stats = tracker
	})
	if srv.stats != tracker {
		t.Error("Options.Stats did not override App.Stats")
	}
}

func manyAttachments(n int) []attachmentRequest {
	out := make([]attachmentRequest, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, attachmentRequest{Filename: "f.txt", Text: "x"})
	}
	return out
}

func truncate(body []byte) string {
	if len(body) > 400 {
		return string(body[:400]) + "..."
	}
	return string(body)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
