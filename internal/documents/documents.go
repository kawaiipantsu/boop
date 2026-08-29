// Package documents turns a file on disk into something a model can read.
//
// It implements PROJECT.md §27: detect the MIME type, choose a processing
// path, extract or transform the content, and hand back
// []provider.ContentPart ready to attach to a message — plus the capability
// requirements the caller must check before sending (§8).
//
//	file → MIME detection → extract/transform → capability check → provider
//
// Three honest statements about coverage, because silently degraded output is
// worse than a clear refusal:
//
//   - Text, DOCX, XLSX and PPTX extraction is complete for the content Boop
//     cares about. OOXML is a zip of XML, so archive/zip and encoding/xml do
//     the whole job. Legacy .doc/.xls/.ppt (OLE compound files) are not
//     supported and say so.
//
//   - PDF extraction (github.com/ledongthuc/pdf, pure Go) reads the text
//     layer of unencrypted PDF 1.0–1.7 with FlateDecode or ASCII85 content
//     streams, mapping characters through /ToUnicode CMaps and the built-in
//     encodings. It does not OCR, does not rasterise pages, and cannot read
//     password-protected files, headers newer than 1.7, files missing a
//     trailing %%EOF, LZW-filtered streams, or subset fonts with no /ToUnicode
//     map. Each of those returns a specific error naming the reason — never
//     empty text. Layout is approximate: text arrives in content-stream order,
//     so multi-column pages interleave. The reader panics rather than
//     erroring on some malformed files, so every call into it is wrapped in a
//     recover; a corrupt attachment yields an error, never a dead process.
//
//   - Images are validated by decoding their header, never their pixels, and
//     require provider.CapabilityVision. PNG, JPEG and GIF go through the
//     stdlib decoders; WebP dimensions are read from the RIFF header directly
//     because the stdlib has no WebP decoder.
//
// Every entry point is bounded. Untrusted input cannot exhaust memory through
// a zip bomb, a lying uncompressed-size header, a huge-canvas image, a
// thousand-page PDF, or a file that is simply enormous.
package documents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// Loading errors.
var (
	// ErrFileTooLarge means the file exceeded Options.MaxFileBytes.
	ErrFileTooLarge = fmt.Errorf("documents: file too large")
	// ErrUnsupportedType means Boop identified the bytes but has no handler.
	ErrUnsupportedType = fmt.Errorf("documents: unsupported file type")
)

// Options bounds and configures extraction.
//
// The zero value is usable: every field falls back to a default via
// withDefaults, so callers can pass Options{} and only set what they care
// about. A negative value disables the corresponding limit; zero means
// "default", which is why disabling is explicit rather than accidental.
type Options struct {
	// MaxFileBytes caps the size of a file read from disk.
	MaxFileBytes int64
	// MaxTextBytes caps extracted text. Beyond this the result is truncated
	// and flagged, because context windows are the real constraint and a
	// silently enormous attachment just gets rejected downstream.
	MaxTextBytes int
	// MaxImageBytes caps an image attachment.
	MaxImageBytes int
	// MaxImagePixels caps width×height, which is the dimension a decompression
	// bomb actually attacks.
	MaxImagePixels int64
	// MaxArchiveEntries caps the member count of an OOXML container.
	MaxArchiveEntries int
	// MaxArchiveBytes caps total decompressed bytes across a container.
	MaxArchiveBytes int64
	// MaxPDFPages caps how many pages are read.
	MaxPDFPages int
	// MaxEmbeddedImages caps images lifted out of a container document.
	MaxEmbeddedImages int
	// ExtractEmbeddedImages lifts images out of DOCX/XLSX/PPTX containers as
	// additional vision attachments (§27 "images where useful"). Off by
	// default: it multiplies attachment size and most requests do not need it.
	ExtractEmbeddedImages bool
	// RawDocumentPart additionally emits a provider.PartDocument carrying the
	// original bytes, for the few providers that ingest PDFs natively. Off by
	// default, because most providers reject it and the extracted text is what
	// makes a text-only model work.
	RawDocumentPart bool
}

// Defaults for Options. They are deliberately generous for text and stingy for
// anything that decompresses.
const (
	defaultMaxFileBytes      = 32 << 20 // 32 MiB
	defaultMaxTextBytes      = 1 << 20  // 1 MiB
	defaultMaxImageBytes     = 8 << 20  // 8 MiB
	defaultMaxImagePixels    = 40_000_000
	defaultMaxArchiveEntries = 4096
	defaultMaxArchiveBytes   = 64 << 20 // 64 MiB
	defaultMaxPDFPages       = 500
	defaultMaxEmbeddedImages = 8
)

// DefaultOptions returns the standard limits.
func DefaultOptions() Options { return Options{}.withDefaults() }

func (o Options) withDefaults() Options {
	o.MaxFileBytes = defaultInt64(o.MaxFileBytes, defaultMaxFileBytes)
	o.MaxTextBytes = defaultInt(o.MaxTextBytes, defaultMaxTextBytes)
	o.MaxImageBytes = defaultInt(o.MaxImageBytes, defaultMaxImageBytes)
	o.MaxImagePixels = defaultInt64(o.MaxImagePixels, defaultMaxImagePixels)
	o.MaxArchiveEntries = defaultInt(o.MaxArchiveEntries, defaultMaxArchiveEntries)
	o.MaxArchiveBytes = defaultInt64(o.MaxArchiveBytes, defaultMaxArchiveBytes)
	o.MaxPDFPages = defaultInt(o.MaxPDFPages, defaultMaxPDFPages)
	o.MaxEmbeddedImages = defaultInt(o.MaxEmbeddedImages, defaultMaxEmbeddedImages)
	return o
}

// defaultInt applies def when v is zero and treats a negative value as "no
// limit", expressed as 0 so the enforcing code can test `limit > 0`.
func defaultInt(v, def int) int {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	default:
		return v
	}
}

func defaultInt64(v, def int64) int64 {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	default:
		return v
	}
}

// EmbeddedImage is a raster image lifted out of a container document.
type EmbeddedImage struct {
	// Name is the entry name inside the container.
	Name string    `json:"name"`
	Info ImageInfo `json:"info"`
	// Data is the original encoded bytes, not re-encoded.
	Data []byte `json:"-"`
}

// Document is a loaded, extracted attachment.
type Document struct {
	// Path is the source path, empty when loaded from bytes.
	Path string `json:"path,omitempty"`
	// Filename is the base name used for the attachment.
	Filename string `json:"filename"`
	// Size is the source byte length.
	Size int64 `json:"size"`
	// Type is the detection result.
	Type TypeInfo `json:"type"`

	// Text is the extracted text, empty for images.
	Text string `json:"text,omitempty"`
	// Truncated reports that Text was cut at a configured cap.
	Truncated bool `json:"truncated,omitempty"`
	// Encoding is the source text encoding, for text files only.
	Encoding Encoding `json:"encoding,omitempty"`
	// Lines counts lines in Text for text files.
	Lines int `json:"lines,omitempty"`

	// Image is set when the file is itself an image.
	Image *ImageInfo `json:"image,omitempty"`
	// PDF and Office carry format-specific metadata.
	PDF    *PDFResult    `json:"pdf,omitempty"`
	Office *OfficeResult `json:"office,omitempty"`
	// Images are attachments lifted out of a container document.
	Images []EmbeddedImage `json:"images,omitempty"`

	// Parts is the multimodal body, ready to attach.
	Parts []provider.ContentPart `json:"parts"`
	// RequiredCapabilities lists what a model must advertise to accept Parts.
	RequiredCapabilities []provider.Capability `json:"required_capabilities,omitempty"`
	// Diagnostics are human-readable notes about what was skipped.
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// Load reads and extracts a file from disk.
//
// The size check happens against the stat result before any bytes are read, so
// an oversized file costs one syscall rather than a full read.
func Load(path string, opts Options) (*Document, error) {
	opts = opts.withDefaults()

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("documents: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("documents: %s is a directory", path)
	}
	if opts.MaxFileBytes > 0 && info.Size() > opts.MaxFileBytes {
		return nil, fmt.Errorf("%w: %s is %s, over the %s limit",
			ErrFileTooLarge, path, humanBytes(info.Size()), humanBytes(opts.MaxFileBytes))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("documents: %w", err)
	}
	doc, err := LoadBytes(filepath.Base(path), data, opts)
	if doc != nil {
		doc.Path = path
	}
	return doc, err
}

// LoadBytes extracts an in-memory attachment.
//
// filename is used for detection and for the attachment label; it need not
// exist on disk. On a handler failure the *Document is still returned with the
// detection result filled in, so callers can report what the file was even
// when extraction did not work.
func LoadBytes(filename string, data []byte, opts Options) (*Document, error) {
	opts = opts.withDefaults()

	doc := &Document{
		Filename: filename,
		Size:     int64(len(data)),
		Type:     Detect(filename, data),
	}
	if doc.Type.Mismatch {
		doc.Diagnostics = append(doc.Diagnostics, fmt.Sprintf(
			"the extension claims %s but the bytes are %s; Boop is treating it as %s",
			doc.Type.FromExtension, doc.Type.Sniffed, doc.Type.MIMEType))
	}

	switch doc.Type.Handler {
	case HandlerText:
		return doc, doc.loadText(data, opts)
	case HandlerImage:
		return doc, doc.loadImage(data, opts)
	case HandlerPDF:
		return doc, doc.loadPDF(data, opts)
	case HandlerOffice:
		return doc, doc.loadOffice(data, opts)
	default:
		return doc, unsupportedError(filename, doc.Type)
	}
}

// unsupportedError explains an unusable file and points at what would work.
func unsupportedError(filename string, t TypeInfo) error {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".doc", ".xls", ".ppt":
		return LegacyOfficeError(ext)
	case ".bmp", ".tif", ".tiff":
		return fmt.Errorf("%w: %s is %s, which no provider accepts as an attachment. "+
			"Convert it to PNG or JPEG", ErrUnsupportedType, filename, t.MIMEType)
	}
	if t.Mismatch {
		return fmt.Errorf("%w: %s is named %q but its bytes are %s, which Boop has no handler for",
			ErrUnsupportedType, filename, ext, t.Sniffed)
	}
	return fmt.Errorf("%w: %s is %s. Supported: text and source files, PNG/JPEG/GIF/WebP images, "+
		"PDF, and DOCX/XLSX/PPTX", ErrUnsupportedType, filename, t.MIMEType)
}

func (d *Document) loadText(data []byte, opts Options) error {
	res, err := DecodeText(data, opts.MaxTextBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Filename, err)
	}
	d.Text, d.Encoding, d.Truncated, d.Lines = res.Text, res.Encoding, res.Truncated, res.Lines
	if res.Replacements > 0 {
		d.Diagnostics = append(d.Diagnostics, fmt.Sprintf(
			"%d undecodable byte(s) were replaced; the file is not cleanly %s",
			res.Replacements, res.Encoding))
	}
	d.Parts = []provider.ContentPart{d.textPart()}
	return nil
}

func (d *Document) loadImage(data []byte, opts Options) error {
	part, info, err := ImagePart(d.Filename, data, opts.MaxImageBytes, opts.MaxImagePixels)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Filename, err)
	}
	d.Image = &info
	d.Parts = []provider.ContentPart{part}
	d.RequiredCapabilities = []provider.Capability{provider.CapabilityVision}
	return nil
}

func (d *Document) loadPDF(data []byte, opts Options) error {
	res, err := ExtractPDF(data, opts)
	d.PDF = res
	if res != nil {
		d.Text, d.Truncated = res.Text, res.Truncated
		d.Lines = lineCount(res.Text)
		d.Diagnostics = append(d.Diagnostics, res.Diagnostics...)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", d.Filename, err)
	}
	d.Parts = []provider.ContentPart{d.textPart()}
	d.appendRawPart(data, opts)
	return nil
}

func (d *Document) loadOffice(data []byte, opts Options) error {
	res, err := ExtractOffice(data, d.Type.MIMEType, opts)
	d.Office = res
	if res != nil {
		d.Text, d.Truncated = res.Text, res.Truncated
		d.Lines = lineCount(res.Text)
		d.Images = res.Images
		d.Diagnostics = append(d.Diagnostics, res.Diagnostics...)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", d.Filename, err)
	}
	d.Parts = []provider.ContentPart{d.textPart()}
	for _, img := range d.Images {
		d.Parts = append(d.Parts, provider.ContentPart{
			Kind:     provider.PartImage,
			MIMEType: img.Info.MIMEType,
			Data:     img.Data,
			Filename: img.Name,
		})
	}
	if len(d.Images) > 0 {
		d.RequiredCapabilities = []provider.Capability{provider.CapabilityVision}
	}
	d.appendRawPart(data, opts)
	return nil
}

// appendRawPart attaches the untransformed bytes for providers that ingest
// documents natively.
func (d *Document) appendRawPart(data []byte, opts Options) {
	if !opts.RawDocumentPart {
		return
	}
	d.Parts = append(d.Parts, provider.ContentPart{
		Kind:     provider.PartDocument,
		MIMEType: d.Type.MIMEType,
		Data:     data,
		Filename: d.Filename,
	})
}

// textPart renders the extracted text as a labelled content part.
//
// The label matters: without it a model cannot tell where an attachment starts
// and the user's own words end, which is how prompt-injection-by-attachment
// gets its foothold.
func (d *Document) textPart() provider.ContentPart {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<attachment filename=%q type=%q", d.Filename, d.Type.MIMEType)
	switch {
	case d.PDF != nil:
		fmt.Fprintf(&sb, " pages=%d", d.PDF.Pages)
	case d.Office != nil && d.Office.Sheets > 0:
		fmt.Fprintf(&sb, " sheets=%d", d.Office.Sheets)
	case d.Office != nil && d.Office.Slides > 0:
		fmt.Fprintf(&sb, " slides=%d", d.Office.Slides)
	}
	if d.Truncated {
		sb.WriteString(" truncated=\"true\"")
	}
	sb.WriteString(">\n")
	sb.WriteString(d.Text)
	if d.Truncated {
		sb.WriteString(TruncationNotice(len(d.Text), int(d.Size)))
	}
	sb.WriteString("\n</attachment>")

	return provider.ContentPart{
		Kind:     provider.PartText,
		Text:     sb.String(),
		MIMEType: d.Type.MIMEType,
		Filename: d.Filename,
	}
}

// RequiresVision reports whether attaching this document needs a vision model.
func (d *Document) RequiresVision() bool {
	for _, c := range d.RequiredCapabilities {
		if c == provider.CapabilityVision {
			return true
		}
	}
	return false
}

// CheckCapabilities verifies a model can accept this document.
//
// It returns nil when the model is suitable and a *CapabilityError otherwise,
// carrying the §8 explanation plus the configured models that would work. Pass
// the full model list as available; the suggestion set is filtered here.
func (d *Document) CheckCapabilities(providerName, model string, caps provider.Capabilities, available []provider.Model) error {
	if len(d.RequiredCapabilities) == 0 {
		return nil
	}
	reason := "the attachment contains image content"
	if d.Image != nil {
		reason = "image attachments need a vision-capable model"
	}
	return RequireCapabilities(d.Filename, providerName, model, caps, available, reason, d.RequiredCapabilities...)
}

// PartsFor returns the parts safe to send to a model with the given
// capabilities.
//
// A document that is only an image is an error: there is nothing to fall back
// to. A document with extracted text plus embedded images degrades instead —
// the images are dropped, a diagnostic explains it, and the text still gets
// through, which is exactly what §27 requires of a text-only model.
func (d *Document) PartsFor(caps provider.Capabilities) ([]provider.ContentPart, []string, error) {
	if !d.RequiresVision() || SupportsImages(caps) {
		return d.Parts, nil, nil
	}
	var kept []provider.ContentPart
	dropped := 0
	for _, p := range d.Parts {
		if p.Kind == provider.PartImage {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return nil, nil, RequireCapabilities(d.Filename, "", "", caps, nil,
			"image attachments need a vision-capable model", provider.CapabilityVision)
	}
	notes := []string{fmt.Sprintf(
		"%d image(s) from %s were dropped because the selected model has no vision capability; "+
			"the extracted text was sent instead", dropped, d.Filename)}
	return kept, notes, nil
}

// Summary is a one-line description for the CLI and TUI attachment list.
func (d *Document) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s, %s)", d.Filename, d.Type.MIMEType, humanBytes(d.Size))
	switch {
	case d.Image != nil:
		fmt.Fprintf(&sb, " %dx%d, needs vision", d.Image.Width, d.Image.Height)
	case d.PDF != nil:
		fmt.Fprintf(&sb, " %d pages, %d with text", d.PDF.Pages, d.PDF.PagesWithText)
	case d.Office != nil:
		switch {
		case d.Office.Slides > 0:
			fmt.Fprintf(&sb, " %d slides", d.Office.Slides)
		case d.Office.Sheets > 0:
			fmt.Fprintf(&sb, " %d sheets", d.Office.Sheets)
		default:
			fmt.Fprintf(&sb, " %d paragraphs", d.Office.Paragraphs)
		}
	case d.Text != "":
		fmt.Fprintf(&sb, " %d lines, %s", d.Lines, d.Encoding)
	}
	if d.Truncated {
		sb.WriteString(", truncated")
	}
	return sb.String()
}

// lineCount counts lines in extracted text, reporting zero for no content
// rather than the one line a naive newline count would claim.
func lineCount(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// humanBytes renders a byte count for user-facing messages.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
