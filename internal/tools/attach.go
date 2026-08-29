package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/documents"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// Budgets for attachments.
//
// The text cap is shared with the other filesystem tools rather than picked
// separately: whatever the source format, the constraint is the same context
// window, and a PDF should not be allowed to evict more of the conversation
// than a text file of the same length would.
//
// The file cap is the size Boop is willing to read off disk and decompress. It
// is larger than the text cap because a 20 MB DOCX may extract to 40 KB of
// prose, and refusing it on source size alone would be wrong.
const (
	attachMaxTextBytes = fsMaxOutputBytes // returned to the model
	attachMaxFileBytes = 32 << 20         // read from disk before giving up
)

// AttachTool turns a file in the workspace into something a model can read.
//
// It is the bridge between internal/documents and the tool layer: the model
// names a path, and gets back extracted text for a PDF, DOCX, XLSX, PPTX or
// text file, or — for an image — a prepared content part plus a plain
// statement that the bytes are not readable as text.
//
// It exists as its own tool rather than as a branch inside read because the
// two answer different questions. read returns a line range of a text file and
// is cheap and exact; attach runs format-specific extraction that can partly
// fail, produce diagnostics, need a vision-capable model, or be truncated, and
// all of that needs somewhere to live in the result. Folding it into read
// would make the common case pay for the rare one, and would hide the
// capability requirement behind a tool whose contract says "text file".
type AttachTool struct{ ws *Workspace }

// NewAttachTool returns an attach tool confined to ws.
func NewAttachTool(ws *Workspace) *AttachTool { return &AttachTool{ws: ws} }

type attachArgs struct {
	Path     string `json:"path"`
	MaxChars int    `json:"max_chars"`
	Pages    int    `json:"pages"`
}

// AttachData is the structured payload of an attach result.
//
// Parts is the point of the whole tool: it is the multimodal body, ready to
// hand to provider.Message. For an image it is the only useful output, because
// Content can say nothing except that the file is an image.
type AttachData struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Bytes    int64  `json:"bytes"`
	// Kind is a short label for humans: "PDF", "DOCX", "PNG", "text".
	Kind     string `json:"kind,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Handler  string `json:"handler,omitempty"`
	// Mismatch reports that the extension claimed a type the bytes contradict.
	Mismatch bool `json:"mismatch,omitempty"`

	TextBytes int    `json:"text_bytes,omitempty"`
	Lines     int    `json:"lines,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	Pages         int `json:"pages,omitempty"`
	PagesWithText int `json:"pages_with_text,omitempty"`
	Paragraphs    int `json:"paragraphs,omitempty"`
	Tables        int `json:"tables,omitempty"`
	Sheets        int `json:"sheets,omitempty"`
	Slides        int `json:"slides,omitempty"`

	Image          *documents.ImageInfo `json:"image,omitempty"`
	RequiresVision bool                 `json:"requires_vision,omitempty"`
	// RequiredCapabilities is what a model must advertise to accept Parts (§8).
	RequiredCapabilities []provider.Capability `json:"required_capabilities,omitempty"`

	// Parts is the attachment body, ready for provider.Message.Parts.
	Parts []provider.ContentPart `json:"parts,omitempty"`
	// Diagnostics are notes about what was skipped or degraded.
	Diagnostics []string `json:"diagnostics,omitempty"`
	// Reason names the failure category on an error result, so a UI can
	// branch on it without parsing the message.
	Reason string `json:"reason,omitempty"`
}

// Name implements Tool.
func (t *AttachTool) Name() string { return "attach" }

// Description implements Tool.
func (t *AttachTool) Description() string {
	return "Read a document or image from the project that the read tool cannot handle: " +
		"PDF, DOCX, XLSX, PPTX, and PNG/JPEG/GIF/WebP images, as well as plain text. " +
		"Returns the extracted text of a document; an image is prepared as an attachment " +
		"and needs a vision-capable model, because it has no text to return. " +
		"Use read for ordinary text and source files — it is cheaper and supports line ranges."
}

// Schema implements Tool.
func (t *AttachTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to the project root.",
			},
			"max_chars": map[string]any{
				"type":    "integer",
				"minimum": 1,
				"description": fmt.Sprintf(
					"Maximum characters of extracted text to return. Defaults to the %s cap, "+
						"which is also the ceiling: a larger value is clamped to it.",
					fsHumanBytes(attachMaxTextBytes)),
			},
			"pages": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "For a PDF, stop after this many pages. Defaults to the whole document.",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
//
// Attaching is a read: same category and same risk grading as the read tool,
// including the raised risk for credential-shaped filenames, since extraction
// pulls the contents into a model context just as read does.
//
// The summary names the size and format because that is what the user is
// actually approving — "Attach report.pdf (2.1 MB PDF)" is a decision, "Attach
// report.pdf" is a shrug. Detection reads only the file's first 512 bytes, so
// classifying a call stays cheap enough to run before every approval.
func (t *AttachTool) Permission(call Call) (permissions.Action, error) {
	var a attachArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, a.Path)
	summary := fmt.Sprintf("Attach %s", display)
	if desc := attachDescribe(abs, contained); desc != "" {
		summary = fmt.Sprintf("Attach %s (%s)", display, desc)
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     fsReadRisk(display, contained),
		Tool:     t.Name(),
		Summary:  summary,
		Detail:   fsTargetDetail(abs, contained),
		Paths:    []string{abs},
	}, nil
}

// Execute implements Tool.
func (t *AttachTool) Execute(ctx context.Context, call Call) (Result, error) {
	started := time.Now()

	var a attachArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return Errorf(call, "the %q argument is required", "path"), nil
	}
	if a.MaxChars < 0 {
		return Errorf(call, "max_chars must be a positive number of characters, got %d", a.MaxChars), nil
	}
	if a.Pages < 0 {
		return Errorf(call, "pages must be a positive page count, got %d", a.Pages), nil
	}
	abs, err := t.ws.Resolve(a.Path)
	if err != nil {
		return Errorf(call, "cannot attach %q: %v", a.Path, err), nil
	}
	rel := t.ws.Rel(abs)

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "file not found: %s", rel), nil
	case err != nil:
		return Errorf(call, "cannot attach %s: %v", rel, err), nil
	case info.IsDir():
		return Errorf(call, "%s is a directory; use the list tool to see its contents", rel), nil
	case info.Size() == 0:
		return Errorf(call, "%s is empty (0 bytes); there is nothing to extract", rel), nil
	}

	// Cancellation is checked here rather than threaded into extraction:
	// documents works on an in-memory buffer with hard size caps, so the
	// bounded work between this point and the result is short.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	doc, loadErr := documents.Load(abs, attachOptions(a))
	if loadErr != nil {
		return attachFailureResult(call, rel, abs, doc, loadErr, started), nil
	}

	data := attachDataFor(rel, doc)
	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  attachContent(rel, doc, a),
		Data:     data,
		Display:  attachOutcome(data),
		Duration: time.Since(started),
	}, nil
}

// attachOptions maps tool arguments onto extraction limits.
//
// max_chars can only lower the text budget, never raise it: the cap protects
// the context window, and a model that has just been told its result was
// truncated must not be able to answer by asking for ten times as much.
//
// The byte budget is applied as the character budget because UTF-8 never
// encodes a character in fewer than one byte, so capping bytes at N
// guarantees at most N characters.
func attachOptions(a attachArgs) documents.Options {
	opts := documents.DefaultOptions()
	opts.MaxFileBytes = attachMaxFileBytes
	opts.MaxTextBytes = attachMaxTextBytes
	if a.MaxChars > 0 && a.MaxChars < attachMaxTextBytes {
		opts.MaxTextBytes = a.MaxChars
	}
	if a.Pages > 0 {
		opts.MaxPDFPages = a.Pages
	}
	return opts
}

// attachDataFor projects a loaded document onto the tool payload.
func attachDataFor(rel string, doc *documents.Document) AttachData {
	data := AttachData{Path: rel, Filename: filepath.Base(rel)}
	if doc == nil {
		return data
	}
	data.Bytes = doc.Size
	data.Kind = attachKind(doc.Type)
	data.MIMEType = doc.Type.MIMEType
	data.Handler = string(doc.Type.Handler)
	data.Mismatch = doc.Type.Mismatch
	data.TextBytes = len(doc.Text)
	data.Lines = doc.Lines
	data.Encoding = string(doc.Encoding)
	data.Truncated = doc.Truncated
	data.Image = doc.Image
	data.RequiresVision = doc.RequiresVision()
	data.RequiredCapabilities = doc.RequiredCapabilities
	data.Parts = doc.Parts
	data.Diagnostics = doc.Diagnostics
	if p := doc.PDF; p != nil {
		data.Pages = p.Pages
		data.PagesWithText = p.PagesWithText
	}
	if o := doc.Office; o != nil {
		data.Paragraphs = o.Paragraphs
		data.Tables = o.Tables
		data.Sheets = o.Sheets
		data.Slides = o.Slides
	}
	return data
}

// attachContent renders what the model reads.
//
// For anything with a text layer this is the labelled <attachment> block
// documents builds, verbatim: the label is what lets a model tell attached
// content apart from the user's own words, which is the whole defence against
// prompt injection by attachment. Notes are appended after it, outside the
// block, so they cannot be confused for document content.
func attachContent(rel string, doc *documents.Document, a attachArgs) string {
	var b strings.Builder

	if text, ok := attachTextPart(doc); ok {
		b.WriteString(text)
		if doc.Truncated {
			fmt.Fprintf(&b, "\n\n[boop: %s was truncated to %s of extracted text; "+
				"ask for a specific section, or lower max_chars and read in pieces]",
				rel, fsHumanBytes(int64(len(doc.Text))))
		}
		if n := attachImageParts(doc); n > 0 {
			fmt.Fprintf(&b, "\n\n[boop: %s also carries %s, attached separately; "+
				"they are visible only to a vision-capable model]", rel, plural(n, "image"))
		}
	} else {
		b.WriteString(attachImageContent(rel, doc))
	}

	for _, d := range doc.Diagnostics {
		fmt.Fprintf(&b, "\n[boop: %s]", d)
	}
	if a.Pages > 0 && doc.PDF != nil && doc.PDF.Pages > a.Pages {
		fmt.Fprintf(&b, "\n[boop: stopped at the requested %s of %d; raise pages to read further]",
			plural(a.Pages, "page"), doc.PDF.Pages)
	}
	return b.String()
}

// attachImageContent states plainly that an image is not text.
//
// The temptation is to return something that looks like content — the
// filename, the dimensions — but a model handed that will summarise the
// metadata as though it had seen the picture. Saying "there is no text here,
// and this needs a vision model" is the only honest answer, and it is the one
// the model can act on by asking the user to switch models.
func attachImageContent(rel string, doc *documents.Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is an image", rel)
	if doc.Image != nil {
		fmt.Fprintf(&b, " (%s, %d×%d, %s)",
			strings.ToUpper(doc.Image.Format), doc.Image.Width, doc.Image.Height,
			fsHumanBytes(int64(doc.Image.Bytes)))
	}
	b.WriteString(", not a text document: there is nothing in it to read as text and none " +
		"has been extracted.\n")
	b.WriteString("It has been prepared as an image attachment, which only a model with the " +
		"vision capability can look at. If the current model has no vision capability, say so " +
		"and ask the user to switch models rather than guessing at the contents. If the image " +
		"is a scan of a document, an OCR tool has to run over it first.")
	return b.String()
}

// attachTextPart returns the labelled text block from a loaded document.
func attachTextPart(doc *documents.Document) (string, bool) {
	for _, p := range doc.Parts {
		if p.Kind == provider.PartText && p.Text != "" {
			return p.Text, true
		}
	}
	if strings.TrimSpace(doc.Text) != "" {
		return doc.Text, true
	}
	return "", false
}

// attachImageParts counts image attachments lifted out of a container.
func attachImageParts(doc *documents.Document) int {
	n := 0
	for _, p := range doc.Parts {
		if p.Kind == provider.PartImage {
			n++
		}
	}
	return n
}

// attachOutcome summarises a successful attachment in a few words: "PDF, 12
// pages, 8.4 KB text". It is the Display field — what a watching user sees
// without unfolding the result.
func attachOutcome(d AttachData) string {
	kind := d.Kind
	if kind == "" {
		kind = "file"
	}
	fields := []string{kind}
	switch {
	case d.Image != nil:
		fields = append(fields,
			fmt.Sprintf("%d×%d", d.Image.Width, d.Image.Height),
			fsHumanBytes(d.Bytes), "needs vision")
	case d.Pages > 0:
		fields = append(fields, plural(d.Pages, "page"), attachTextSize(d))
	case d.Slides > 0:
		fields = append(fields, plural(d.Slides, "slide"), attachTextSize(d))
	case d.Sheets > 0:
		fields = append(fields, plural(d.Sheets, "sheet"), attachTextSize(d))
	default:
		fields = append(fields, plural(d.Lines, "line"), attachTextSize(d))
	}
	if d.Truncated {
		fields = append(fields, "truncated")
	}
	return strings.Join(fields, ", ")
}

func attachTextSize(d AttachData) string {
	return fsHumanBytes(int64(d.TextBytes)) + " text"
}

// attachKind names a detected type in the one or two words an approval prompt
// and a status line have room for.
func attachKind(t documents.TypeInfo) string {
	ext := strings.ToUpper(strings.TrimPrefix(t.Extension, "."))
	switch t.Handler {
	case documents.HandlerPDF:
		return "PDF"
	case documents.HandlerImage:
		if ext != "" {
			return ext
		}
		return "image"
	case documents.HandlerOffice:
		if ext != "" {
			return ext
		}
		return "office document"
	case documents.HandlerText:
		return "text"
	default:
		if t.MIMEType != "" {
			return t.MIMEType
		}
		return "file"
	}
}

// attachDescribe renders "2.1 MB PDF" for an approval summary, or "" when the
// file cannot be inspected — in which case the caller falls back to the bare
// path rather than inventing a description.
func attachDescribe(abs string, contained bool) string {
	if !contained {
		return ""
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return ""
	}
	kind := ""
	if t, err := documents.DetectFile(abs); err == nil {
		kind = attachKind(t)
	}
	if kind == "" {
		return fsHumanBytes(info.Size())
	}
	return fmt.Sprintf("%s %s", fsHumanBytes(info.Size()), kind)
}

// --- failures -------------------------------------------------------------

// attachFailure maps one documents error category onto what a user and a model
// each need to see.
//
// documents already writes the actionable half of every message — "it needs a
// password, re-save an unprotected copy", "this looks like a scan, Boop does
// not OCR". The job here is to preserve that text rather than flatten it to
// "attach failed", to attach a stable machine-readable reason, and to add a
// next step only for the few categories whose message states the fact without
// stating the remedy.
type attachFailure struct {
	// sentinel is the named error from internal/documents.
	sentinel error
	// reason is the stable category recorded in AttachData.Reason.
	reason string
	// display is the short outcome shown in the UI.
	display string
	// hint is appended only when documents' own message ends without one.
	hint string
}

// attachFailures is ordered specific-first; the first matching entry wins.
var attachFailures = []attachFailure{
	{documents.ErrPDFEncrypted, "pdf_encrypted", "encrypted PDF", ""},
	{documents.ErrPDFNoTextLayer, "pdf_no_text_layer", "PDF has no text layer", ""},
	{documents.ErrPDFUnextractable, "pdf_unextractable", "PDF text not decodable", ""},
	{documents.ErrPDFDamaged, "pdf_damaged", "damaged PDF",
		"the file is corrupt rather than merely unusual; re-download or re-export it before trying again"},
	{documents.ErrNotPDF, "not_pdf", "not a PDF",
		"check what the file really is before attaching it again"},

	{documents.ErrOfficeUnsupported, "legacy_office", "legacy office format", ""},
	{documents.ErrOfficeEmpty, "office_empty", "no text in document",
		"the container parsed but holds no text; if its content is pictures of text it needs OCR or a vision-capable model"},
	{documents.ErrNotOffice, "not_office", "not an office document", ""},
	{documents.ErrArchiveTooLarge, "archive_too_large", "archive over limits",
		"Boop refused to expand it because the declared decompressed size is beyond its bounds"},
	{documents.ErrUnsafeArchivePath, "unsafe_archive", "unsafe archive entry",
		"an entry name pointed outside the archive, so the file was rejected as hostile rather than merely broken"},

	{documents.ErrNotAnImage, "not_an_image", "not a valid image",
		"the bytes do not decode as the image format the name claims"},
	{documents.ErrImageTooLarge, "image_too_large", "image too large",
		"re-encode it smaller before attaching"},
	{documents.ErrImageTooManyPixels, "image_too_many_pixels", "image resolution too high",
		"resize it before attaching"},
	{documents.ErrUnsupportedImageFormat, "unsupported_image", "unsupported image format",
		"convert it to PNG or JPEG"},

	{documents.ErrUnsupportedEncoding, "unsupported_encoding", "undecodable text", ""},
	{documents.ErrFileTooLarge, "file_too_large", "file too large",
		"attach the part you need, or split the file first"},
	{documents.ErrUnsupportedType, "unsupported_type", "unsupported file type", ""},
}

// attachFailureResult turns an extraction error into a failed Result.
//
// It is a Result and not a Go error on purpose: every one of these is
// something the model can respond to — by choosing another file, asking for
// OCR, requesting a vision model, or telling the user the PDF needs a
// password. Returning a Go error would abort the exchange and lose the
// explanation that makes the next attempt work.
//
// The partial document is still reported when there is one, so the UI can show
// that a PDF was recognised, its page count read, and only its text layer
// missing.
func attachFailureResult(call Call, rel, abs string, doc *documents.Document, err error, started time.Time) Result {
	reason, display, msg := attachExplain(rel, abs, err)
	data := attachDataFor(rel, doc)
	data.Reason = reason
	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  msg,
		Data:     data,
		Display:  display,
		IsError:  true,
		Duration: time.Since(started),
	}
}

// attachExplain classifies err and renders the message the model reads.
func attachExplain(rel, abs string, err error) (reason, display, msg string) {
	detail := attachCleanError(rel, abs, err)
	reason, display, hint := "extraction_failed", "attach failed", ""
	for _, f := range attachFailures {
		if errors.Is(err, f.sentinel) {
			reason, display, hint = f.reason, f.display, f.hint
			break
		}
	}
	msg = fmt.Sprintf("cannot attach %s: %s", rel, detail)
	if hint != "" {
		msg += ". " + hint
	}
	return reason, display, msg
}

// attachCleanError renders a documents error as a user-facing sentence.
//
// Three cosmetic fixes, each of which would otherwise show up verbatim in an
// approval log and a model transcript: the absolute path is rewritten to the
// workspace-relative one the model asked for, the "documents:" package prefix
// is dropped, and the leading filename is removed because the caller has
// already named the file.
func attachCleanError(rel, abs string, err error) string {
	msg := err.Error()
	if abs != "" {
		msg = strings.ReplaceAll(msg, abs, rel)
	}
	msg = strings.ReplaceAll(msg, "documents: ", "")
	msg = strings.TrimPrefix(msg, filepath.Base(rel)+": ")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "extraction failed for an unreported reason"
	}
	return msg
}

// compile-time check that the payload stays serialisable for the WebUI.
var _ = func() bool { _, err := json.Marshal(AttachData{}); return err == nil }()
