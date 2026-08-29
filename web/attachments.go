package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/documents"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// Bounds on uploads (§27).
//
// The general request cap stays at one megabyte: the API takes prompts and
// configuration documents, and a body larger than that is a bug everywhere
// except here. POST /api/message is the one endpoint that carries files, so it
// is the one endpoint with a bigger — and still finite — budget.
const (
	// maxUploadBytes bounds the whole POST /api/message body when it carries
	// attachments. Base64 inflates by a third, so this is roughly 18 MiB of
	// actual file.
	maxUploadBytes = 24 << 20
	// maxAttachmentBytes bounds one decoded attachment.
	maxAttachmentBytes = 16 << 20
	// maxAttachments bounds how many files one message may carry, so a
	// thousand one-byte files cannot turn into a thousand extractions.
	maxAttachments = 8
	// multipartMemory is how much of a multipart body is buffered in memory
	// before Go spills to a temporary file.
	multipartMemory = 8 << 20
	// capabilityProbeTimeout bounds the §8 capability lookup for the selected
	// model. A provider that will not answer must not stall the turn.
	capabilityProbeTimeout = 5 * time.Second
	// alternativesTimeout bounds the model listing used to suggest a
	// replacement model in a capability error.
	alternativesTimeout = 5 * time.Second
)

// attachmentRequest is one uploaded file in a JSON message body.
//
// Exactly one of ContentBase64 and Text must be set. Text exists because a
// browser cannot base64 a UTF-8 string with btoa without tripping over it, and
// pasting a log into the chat is the commonest attachment there is.
type attachmentRequest struct {
	Filename string `json:"filename"`
	// ContentBase64 is the file's bytes, standard base64.
	ContentBase64 string `json:"content_base64,omitempty"`
	// Text is the file's content when it is already text.
	Text string `json:"text,omitempty"`
}

// upload is a decoded file on its way to internal/documents.
type upload struct {
	filename string
	data     []byte
}

// attachmentInfo is what the response says about one attachment.
type attachmentInfo struct {
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	// Kind is the processing path documents chose: text, image, pdf or office.
	Kind string `json:"kind,omitempty"`
	// TextChars is how much extracted text was attached.
	TextChars int `json:"text_chars,omitempty"`
	Lines     int `json:"lines,omitempty"`
	// Truncated reports extracted text cut at a limit.
	Truncated bool `json:"truncated,omitempty"`
	// Parts is how many content parts were attached to the message.
	Parts int `json:"parts"`
	// RequiredCapabilities is what the model had to advertise to accept it.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	// Degraded reports that image content was dropped and the extracted text
	// sent instead, because the selected model has no vision (§27).
	Degraded bool `json:"degraded,omitempty"`
	// Diagnostics are the extractor's notes about what it skipped.
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// attachmentSet is the outcome of loading a message's uploads.
type attachmentSet struct {
	// parts are the content parts sent to the model this turn.
	parts []provider.ContentPart
	// stored are the parts written to the transcript. They differ from parts
	// only in that image bytes are replaced by a note; see storedPart.
	stored []provider.ContentPart
	// notes explain a degradation to the user, and are also prepended to the
	// message so the model knows something was withheld rather than absent.
	notes []string
	infos []attachmentInfo
	// vision is true when image parts survived into parts.
	vision bool
}

func (a *attachmentSet) empty() bool { return a == nil || len(a.parts) == 0 }

// attachmentError carries an HTTP status alongside the message, so one
// preparation path can serve both the REST handler and the WebSocket.
type attachmentError struct {
	status  int
	code    string
	message string
	details []string
}

func (e *attachmentError) Error() string { return e.message }

// writeAttachmentError answers a request that failed attachment preparation.
func writeAttachmentError(w http.ResponseWriter, err error) bool {
	var ae *attachmentError
	if !errors.As(err, &ae) {
		return false
	}
	writeError(w, ae.status, ae.code, ae.message, ae.details...)
	return true
}

// ---------------------------------------------------------------------------
// Collecting uploads
// ---------------------------------------------------------------------------

// collectUploads decodes the attachments carried in a JSON body.
func collectUploads(reqs []attachmentRequest) ([]upload, error) {
	out := make([]upload, 0, len(reqs))
	for i, a := range reqs {
		name := strings.TrimSpace(a.Filename)
		if name == "" {
			return nil, &attachmentError{
				status: http.StatusBadRequest, code: codeBadRequest,
				message: fmt.Sprintf("attachment %d has no `filename`; the name is what decides how it is read", i+1),
			}
		}
		hasB64, hasText := a.ContentBase64 != "", a.Text != ""
		if hasB64 == hasText {
			return nil, &attachmentError{
				status: http.StatusBadRequest, code: codeBadRequest,
				message: fmt.Sprintf("attachment %q needs exactly one of `content_base64` or `text`", name),
			}
		}
		if hasText {
			out = append(out, upload{filename: name, data: []byte(a.Text)})
			continue
		}
		data, err := decodeBase64(a.ContentBase64)
		if err != nil {
			return nil, &attachmentError{
				status: http.StatusBadRequest, code: codeBadRequest,
				message: fmt.Sprintf("attachment %q is not valid base64: %v", name, err),
			}
		}
		out = append(out, upload{filename: name, data: data})
	}
	return out, nil
}

// decodeBase64 accepts padded and unpadded standard base64, because the two
// are equally likely to arrive from a hand-written client.
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	// A data: URL is what a FileReader produces; accept it rather than making
	// every caller strip the prefix.
	if strings.HasPrefix(s, "data:") {
		if _, after, found := strings.Cut(s, ","); found {
			s = after
		}
	}
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

// collectMultipart reads the message fields and files out of a
// multipart/form-data body, which is what an HTML form or a FormData fetch
// sends.
func collectMultipart(r *http.Request) (messageRequest, []upload, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		return messageRequest{}, nil, multipartError(err)
	}

	req := messageRequest{
		SessionID: strings.TrimSpace(r.FormValue("session_id")),
		Content:   r.FormValue("content"),
		Provider:  strings.TrimSpace(r.FormValue("provider")),
		Model:     strings.TrimSpace(r.FormValue("model")),
		Async:     isTruthy(r.FormValue("async")),
	}
	if v := strings.TrimSpace(r.FormValue("degrade")); v != "" {
		degrade := isTruthy(v)
		req.Degrade = &degrade
	}

	if r.MultipartForm == nil {
		return req, nil, nil
	}
	// Field names are iterated in sorted order so the attachment order a
	// response reports is the same on every run.
	fields := make([]string, 0, len(r.MultipartForm.File))
	for field := range r.MultipartForm.File {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	var uploads []upload
	for _, field := range fields {
		for _, header := range r.MultipartForm.File[field] {
			data, err := readUpload(header)
			if err != nil {
				return req, nil, err
			}
			name := strings.TrimSpace(header.Filename)
			if name == "" {
				name = field
			}
			uploads = append(uploads, upload{filename: name, data: data})
		}
	}
	return req, uploads, nil
}

// readUpload reads one multipart file, refusing an oversized one before it is
// fully in memory.
func readUpload(header *multipart.FileHeader) ([]byte, error) {
	if header.Size > maxAttachmentBytes {
		return nil, &attachmentError{
			status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
			message: fmt.Sprintf("attachment %q is %d bytes, over the %d byte limit",
				header.Filename, header.Size, maxAttachmentBytes),
		}
	}
	f, err := header.Open()
	if err != nil {
		return nil, &attachmentError{
			status: http.StatusBadRequest, code: codeBadRequest,
			message: fmt.Sprintf("cannot read attachment %q: %v", header.Filename, err),
		}
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
	if err != nil {
		return nil, &attachmentError{
			status: http.StatusBadRequest, code: codeBadRequest,
			message: fmt.Sprintf("cannot read attachment %q: %v", header.Filename, err),
		}
	}
	if len(data) > maxAttachmentBytes {
		return nil, &attachmentError{
			status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
			message: fmt.Sprintf("attachment %q is over the %d byte limit", header.Filename, maxAttachmentBytes),
		}
	}
	return data, nil
}

// multipartError turns a body-limit failure into the right status.
func multipartError(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return &attachmentError{
			status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
			message: fmt.Sprintf("the upload exceeds the %d byte request limit", maxUploadBytes),
		}
	}
	return &attachmentError{
		status: http.StatusBadRequest, code: codeBadRequest,
		message: "the multipart body could not be read: " + err.Error(),
	}
}

// ---------------------------------------------------------------------------
// Loading and the capability check
// ---------------------------------------------------------------------------

// prepareTurn loads a message's attachments and decides what the selected
// model is allowed to see (§8, §27).
//
// It runs before the turn starts, and synchronously even for an async turn,
// because "your PDF is encrypted" and "that model cannot see images" are
// answers to the request the caller made — publishing them as an error event
// twenty milliseconds later would be strictly worse.
func (s *Server) prepareTurn(ctx context.Context, req messageRequest, extra []upload) (*attachmentSet, error) {
	uploads, err := collectUploads(req.Attachments)
	if err != nil {
		return nil, err
	}
	uploads = append(uploads, extra...)
	if len(uploads) == 0 {
		return nil, nil
	}
	if len(uploads) > maxAttachments {
		return nil, &attachmentError{
			status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
			message: fmt.Sprintf("a message may carry at most %d attachments, got %d", maxAttachments, len(uploads)),
		}
	}
	total := 0
	for _, u := range uploads {
		if len(u.data) > maxAttachmentBytes {
			return nil, &attachmentError{
				status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
				message: fmt.Sprintf("attachment %q is %d bytes, over the %d byte limit",
					u.filename, len(u.data), maxAttachmentBytes),
			}
		}
		total += len(u.data)
	}
	if total > maxUploadBytes {
		return nil, &attachmentError{
			status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
			message: fmt.Sprintf("the attachments total %d bytes, over the %d byte limit", total, maxUploadBytes),
		}
	}

	providerName := firstNonEmpty(req.Provider, s.cfg.Provider)
	model := firstNonEmpty(req.Model, s.cfg.Model)
	degrade := req.Degrade == nil || *req.Degrade

	opts := documents.Options{
		MaxFileBytes: maxAttachmentBytes,
		// The extracted text of a whole book is not a useful prompt, and the
		// context manager would drop most of it anyway (§47).
		MaxTextBytes: 512 << 10,
		// §27 asks for "images where useful" out of container documents. A
		// user who drags a specification with diagrams into the chat means the
		// diagrams; the count and size are already bounded by the package, and
		// a text-only model degrades to the extracted text below rather than
		// failing.
		ExtractEmbeddedImages: true,
	}

	set := &attachmentSet{}
	var caps provider.Capabilities
	capsKnown := false
	probed := false

	for _, u := range uploads {
		doc, err := documents.LoadBytes(u.filename, u.data, opts)
		if err != nil {
			return nil, loadError(u.filename, err)
		}
		info := attachmentInfo{
			Filename:  doc.Filename,
			MIMEType:  doc.Type.MIMEType,
			Size:      doc.Size,
			Kind:      string(doc.Type.Handler),
			TextChars: len(doc.Text),
			Lines:     doc.Lines,
			Truncated: doc.Truncated,
			// Diagnostics are surfaced rather than logged: they are the record
			// of what the extractor could not do (§27).
			Diagnostics: doc.Diagnostics,
		}
		for _, c := range doc.RequiredCapabilities {
			info.RequiredCapabilities = append(info.RequiredCapabilities, string(c))
		}

		parts := doc.Parts
		if doc.RequiresVision() {
			if !probed {
				caps, capsKnown = s.modelCapabilities(ctx, providerName, model)
				probed = true
			}
			switch {
			case !capsKnown:
				// The model's capabilities could not be read, so the router
				// gets to make the §8 decision instead; see execTurn, which
				// puts vision in Selection.Required.
			case documents.SupportsImages(caps):
				// Nothing to do: the model can see.
			case !degrade:
				return nil, capabilityError(s, ctx, doc.CheckCapabilities(providerName, model, caps, s.alternatives(ctx)))
			default:
				kept, notes, derr := doc.PartsFor(caps)
				if derr != nil {
					// Nothing survives the degradation — an image with no text
					// to fall back to. Report it rather than sending a message
					// that quietly lost its only content.
					return nil, capabilityError(s, ctx, derr)
				}
				parts = kept
				info.Degraded = true
				set.notes = append(set.notes, notes...)
			}
		}

		info.Parts = len(parts)
		set.infos = append(set.infos, info)
		for _, p := range parts {
			if p.Kind == provider.PartImage {
				set.vision = true
			}
			set.parts = append(set.parts, p)
			set.stored = append(set.stored, storedPart(p))
		}
	}
	return set, nil
}

// storedPart is the transcript form of a content part.
//
// Image bytes are replaced by a note. A single screenshot is megabytes of
// base64 in SQLite, and the history window replays the last forty turns into
// every request, so keeping the raw image would make one attachment a
// permanent tax on the session. The note keeps the fact of the attachment,
// which is what later turns actually need.
func storedPart(p provider.ContentPart) provider.ContentPart {
	if p.Kind != provider.PartImage && p.Kind != provider.PartDocument {
		return p
	}
	name := p.Filename
	if name == "" {
		name = "attachment"
	}
	return provider.ContentPart{
		Kind: provider.PartText,
		Text: fmt.Sprintf("<attachment filename=%q type=%q bytes=%d retained=\"false\">"+
			"binary content was sent to the model but is not kept in the transcript"+
			"</attachment>", name, p.MIMEType, len(p.Data)),
		MIMEType: p.MIMEType,
		Filename: p.Filename,
	}
}

// loadError maps an extraction failure onto the API's vocabulary.
func loadError(filename string, err error) error {
	switch {
	case errors.Is(err, documents.ErrFileTooLarge):
		return &attachmentError{
			status: http.StatusRequestEntityTooLarge, code: codeBadRequest,
			message: err.Error(),
		}
	case errors.Is(err, documents.ErrUnsupportedType):
		return &attachmentError{
			status: http.StatusUnsupportedMediaType, code: codeBadRequest,
			message: err.Error(),
		}
	default:
		return &attachmentError{
			status: http.StatusUnprocessableEntity, code: codeBadRequest,
			message: fmt.Sprintf("cannot read attachment %s: %v", filename, err),
		}
	}
}

// capabilityError renders a *documents.CapabilityError as the §8 refusal,
// listing the models that would work.
func capabilityError(s *Server, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var ce *documents.CapabilityError
	if !errors.As(err, &ce) {
		return &attachmentError{
			status: http.StatusUnprocessableEntity, code: codeBadRequest, message: err.Error(),
		}
	}
	details := make([]string, 0, len(ce.Alternatives))
	alternatives := ce.Alternatives
	if len(alternatives) == 0 {
		alternatives = documents.VisionModels(s.alternatives(ctx))
	}
	for _, m := range alternatives {
		details = append(details, m.Provider+"/"+m.ID)
	}
	return &attachmentError{
		status:  http.StatusUnprocessableEntity,
		code:    codeUnsupportedCapability,
		message: ce.Error(),
		details: details,
	}
}

// modelCapabilities reads what the selected model can do (§8).
//
// A failure is reported as "unknown", not as "no capabilities": treating an
// unreachable provider as incapable would silently strip images from a message
// a vision model would have accepted.
func (s *Server) modelCapabilities(ctx context.Context, providerName, model string) (provider.Capabilities, bool) {
	if s.app == nil || s.app.Router == nil {
		return nil, false
	}
	p, ok := s.app.Router.Registry().Get(providerName)
	if !ok {
		return nil, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()
	caps, err := p.Capabilities(probeCtx, model)
	if err != nil {
		return nil, false
	}
	return caps, true
}

// alternatives lists every model the configured providers offer, for the
// "switch to one of these" half of a capability error. It is only called on
// the error path, because it touches every backend.
func (s *Server) alternatives(ctx context.Context) []provider.Model {
	if s.app == nil || s.app.Router == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, alternativesTimeout)
	defer cancel()

	registry := s.app.Router.Registry()
	var (
		mu  sync.Mutex
		out []provider.Model
		wg  sync.WaitGroup
	)
	for _, name := range registry.Names() {
		p, ok := registry.Get(name)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string, p provider.Provider) {
			defer wg.Done()
			models, err := p.ListModels(listCtx)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range models {
				if m.Provider == "" {
					m.Provider = name
				}
				out = append(out, m)
			}
		}(name, p)
	}
	wg.Wait()
	return out
}
