package documents

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDF extraction errors.
//
// Each names a specific, actionable failure. Returning empty text for a
// document the user can plainly read on screen is a bug, not a result: the
// model would conclude the file is blank. Every path that produces no text
// therefore produces one of these instead.
var (
	// ErrNotPDF means the bytes are not a PDF the reader will accept.
	ErrNotPDF = errors.New("documents: not a readable PDF")
	// ErrPDFEncrypted means the file is password-protected. Boop does not
	// prompt for PDF passwords; an empty user password is tried automatically.
	ErrPDFEncrypted = errors.New("documents: PDF is encrypted")
	// ErrPDFNoTextLayer means the PDF renders only images — a scan or an
	// image-only export. OCR is out of scope.
	ErrPDFNoTextLayer = errors.New("documents: PDF has no extractable text layer")
	// ErrPDFUnextractable means text objects exist but produced no characters,
	// typically an embedded subset font with no /ToUnicode map.
	ErrPDFUnextractable = errors.New("documents: PDF text could not be decoded")
	// ErrPDFDamaged means the structure could not be parsed at all.
	ErrPDFDamaged = errors.New("documents: PDF structure is unreadable")
)

// PDFResult is the outcome of a text-extraction attempt.
type PDFResult struct {
	// Text is the extracted text layer, pages separated by blank lines.
	Text string `json:"text"`
	// Pages is the page count reported by the document catalog.
	Pages int `json:"pages"`
	// PagesProcessed is how many pages were actually read; it is lower than
	// Pages when the page cap or the byte cap stopped extraction early.
	PagesProcessed int `json:"pages_processed"`
	// PagesWithText counts pages that yielded characters. A large gap between
	// this and Pages usually means a partly scanned document.
	PagesWithText int `json:"pages_with_text"`
	// Truncated reports that extraction stopped at a configured cap.
	Truncated bool `json:"truncated,omitempty"`
	// Producer, Title and Version come from the document metadata.
	Producer string `json:"producer,omitempty"`
	Title    string `json:"title,omitempty"`
	Version  string `json:"version,omitempty"`
	// ImagesFound counts image XObjects referenced by the processed pages. It
	// is what separates "scanned page" from "genuinely blank page".
	ImagesFound int `json:"images_found,omitempty"`
	// UnsupportedFilters lists stream filters the reader refused.
	UnsupportedFilters []string `json:"unsupported_filters,omitempty"`
	// FailedPages counts pages that errored or panicked during extraction.
	FailedPages int `json:"failed_pages,omitempty"`
	// Diagnostics are human-readable notes about what was skipped and why.
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// IsPDF reports whether data begins with a PDF header.
//
// A small leading offset is tolerated because some producers prepend bytes and
// conforming readers are required to cope.
func IsPDF(data []byte) bool {
	window := data
	if len(window) > 1024 {
		window = window[:1024]
	}
	return bytes.Contains(window, []byte("%PDF-"))
}

// ExtractPDF pulls the text layer out of a PDF.
//
// What this handles: unencrypted PDF 1.0–1.7 with a valid cross-reference
// table or xref stream; FlateDecode and ASCII85Decode content streams; simple
// and composite fonts mapped through /ToUnicode CMaps, /Differences arrays and
// the built-in encodings; encrypted files whose user password is empty.
//
// What this does not handle, and reports instead of guessing: password-
// protected files, scanned or image-only pages (no OCR), LZWDecode and other
// filters the reader rejects, embedded subset fonts with no /ToUnicode map,
// and PDF 2.0 headers. Layout fidelity is approximate — text comes out in
// content-stream order, so multi-column pages interleave.
//
// Resource use is bounded: at most opts.MaxPDFPages pages are read and at most
// opts.MaxTextBytes of text is kept, with Truncated set when either bites.
func ExtractPDF(data []byte, opts Options) (*PDFResult, error) {
	opts = opts.withDefaults()
	if !IsPDF(data) {
		return nil, fmt.Errorf("%w: no %%PDF- header in the first 1 KiB", ErrNotPDF)
	}

	res := &PDFResult{Version: pdfVersion(data)}

	var reader *pdf.Reader
	err := guardPanic("opening the PDF", func() error {
		r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
		reader = r
		return err
	})
	if err != nil {
		return nil, openError(err, res.Version)
	}

	if err := guardPanic("reading the page count", func() error {
		res.Pages = reader.NumPage()
		return nil
	}); err != nil {
		res.Pages = 0
		res.Diagnostics = append(res.Diagnostics, "the page count could not be read")
	}
	readPDFMetadata(reader, res)

	if res.Pages <= 0 {
		return nil, fmt.Errorf("%w: the catalog reports no pages", ErrPDFDamaged)
	}

	limit := res.Pages
	if opts.MaxPDFPages > 0 && limit > opts.MaxPDFPages {
		limit = opts.MaxPDFPages
		res.Truncated = true
		res.Diagnostics = append(res.Diagnostics,
			fmt.Sprintf("stopped after %d of %d pages (page cap)", limit, res.Pages))
	}

	tb := &pdfTextBuilder{max: opts.MaxTextBytes}
	filters := map[string]bool{}
	// Fonts are cached across pages: charmap parsing dominates the cost, and
	// a document typically reuses a handful of fonts on every page.
	fonts := map[string]*pdf.Font{}

	for i := 1; i <= limit; i++ {
		if tb.full() {
			res.Truncated = true
			res.Diagnostics = append(res.Diagnostics,
				fmt.Sprintf("stopped after %d of %d pages (text size cap)", i-1, res.Pages))
			break
		}
		res.PagesProcessed = i
		text, images, perr := extractPage(reader, i, fonts)
		res.ImagesFound += images
		if perr != nil {
			res.FailedPages++
			if f := unsupportedFilterName(perr); f != "" {
				filters[f] = true
			}
			if len(res.Diagnostics) < 16 {
				res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("page %d: %v", i, perr))
			}
			continue
		}
		if strings.TrimSpace(text) != "" {
			res.PagesWithText++
			tb.writePage(text)
		}
	}

	res.Truncated = res.Truncated || tb.truncated
	for f := range filters {
		res.UnsupportedFilters = append(res.UnsupportedFilters, f)
	}
	sort.Strings(res.UnsupportedFilters)
	res.Text = strings.TrimSpace(NormalizeNewlines(tb.String()))

	if res.Text == "" {
		return res, explainEmptyPDF(res)
	}
	return res, nil
}

// extractPage reads one page under panic protection.
//
// The reader panics rather than returning an error on several classes of
// malformed input (bad xref offsets, unknown stream filters, short encrypted
// blocks). Page.GetPlainText recovers internally, but Reader.Page, Page.Fonts
// and Page.Resources do not, so every call is wrapped here. Without this a
// single corrupt attachment would take the whole Boop process down.
func extractPage(reader *pdf.Reader, num int, fonts map[string]*pdf.Font) (text string, images int, err error) {
	err = guardPanic(fmt.Sprintf("reading page %d", num), func() error {
		page := reader.Page(num)
		if page.V.IsNull() {
			return nil
		}
		for _, name := range page.Fonts() {
			if _, ok := fonts[name]; !ok {
				f := page.Font(name)
				fonts[name] = &f
			}
		}
		images = countPageImages(page)
		t, terr := page.GetPlainText(fonts)
		text = t
		return terr
	})
	return text, images, err
}

// countPageImages counts image XObjects on a page.
//
// This is the signal that turns "extraction produced nothing" into "this looks
// like a scan": a blank page has no images, a scanned page has exactly one
// full-bleed one.
func countPageImages(page pdf.Page) int {
	res := page.Resources()
	if res.IsNull() {
		return 0
	}
	xobjects := res.Key("XObject")
	if xobjects.Kind() != pdf.Dict {
		return 0
	}
	n := 0
	for _, key := range xobjects.Keys() {
		if xobjects.Key(key).Key("Subtype").Name() == "Image" {
			n++
		}
	}
	return n
}

func readPDFMetadata(reader *pdf.Reader, res *PDFResult) {
	_ = guardPanic("reading document metadata", func() error {
		info := reader.Trailer().Key("Info")
		if info.IsNull() {
			return nil
		}
		res.Producer = strings.TrimSpace(info.Key("Producer").Text())
		res.Title = strings.TrimSpace(info.Key("Title").Text())
		return nil
	})
}

// guardPanic runs fn, converting a panic into an error.
func guardPanic(what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s failed on malformed input: %v", what, r)
		}
	}()
	return fn()
}

// openError turns a reader construction failure into one of Boop's categories.
func openError(err error, version string) error {
	msg := err.Error()
	switch {
	case errors.Is(err, pdf.ErrInvalidPassword):
		return fmt.Errorf("%w: it needs a password. Boop does not prompt for PDF passwords — "+
			"open it in a viewer and re-save or print an unprotected copy, then attach that", ErrPDFEncrypted)

	case strings.Contains(msg, "encryption"):
		return fmt.Errorf("%w: %s. Boop only opens PDFs whose user password is empty and whose "+
			"encryption is the standard RC4/AES-128 handler; re-save an unprotected copy and attach that",
			ErrPDFEncrypted, msg)

	case strings.Contains(msg, "invalid header"):
		detail := "the header is not %PDF-1.0 through %PDF-1.7"
		if version != "" {
			detail = fmt.Sprintf("the header declares PDF %s, and the reader accepts 1.0 through 1.7", version)
		}
		return fmt.Errorf("%w: %s. Re-save it as PDF 1.7 or earlier", ErrNotPDF, detail)

	case strings.Contains(msg, "missing %%EOF"), strings.Contains(msg, "startxref"):
		return fmt.Errorf("%w: %s — the file looks truncated or was damaged in transfer. "+
			"Re-download or re-export it", ErrPDFDamaged, msg)

	default:
		return fmt.Errorf("%w: %s", ErrPDFDamaged, msg)
	}
}

// explainEmptyPDF converts "no text came out" into a specific reason.
func explainEmptyPDF(res *PDFResult) error {
	switch {
	case len(res.UnsupportedFilters) > 0:
		return fmt.Errorf("%w: the content streams use the unsupported filter(s) %s. "+
			"Re-save the PDF with a modern writer, which will re-encode them as FlateDecode",
			ErrPDFUnextractable, strings.Join(res.UnsupportedFilters, ", "))

	case res.ImagesFound > 0:
		return fmt.Errorf("%w: %d page(s) contain %d image(s) and no text objects, so this looks "+
			"like a scan or an image-only export. Boop does not OCR — run the file through an OCR "+
			"tool first, or export the pages as images and attach them to a vision-capable model",
			ErrPDFNoTextLayer, res.Pages, res.ImagesFound)

	case res.FailedPages > 0 && res.FailedPages >= res.PagesProcessed:
		return fmt.Errorf("%w: every one of the %d page(s) read failed to parse: %s",
			ErrPDFDamaged, res.PagesProcessed, strings.Join(res.Diagnostics, "; "))

	default:
		return fmt.Errorf("%w: the %d page(s) contain no text-showing operators, or the fonts are "+
			"embedded subsets with no /ToUnicode map so their character codes cannot be turned back "+
			"into text. Copying the text out of a viewer and pasting it will work where this does not",
			ErrPDFNoTextLayer, res.Pages)
	}
}

var unsupportedFilterRe = regexp.MustCompile(`(?:unsupported|unknown) filter[:\s]*/?([A-Za-z0-9]*)`)

// unsupportedFilterName pulls the filter name out of the reader's panic text,
// so the user is told "LZWDecode" rather than "extraction failed".
func unsupportedFilterName(err error) string {
	m := unsupportedFilterRe.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	if m[1] == "" {
		return "unnamed"
	}
	return m[1]
}

func pdfVersion(data []byte) string {
	i := bytes.Index(data, []byte("%PDF-"))
	if i < 0 || i+8 > len(data) {
		return ""
	}
	return strings.TrimSpace(string(data[i+5 : i+8]))
}

// pdfTextBuilder accumulates page text under a byte cap.
type pdfTextBuilder struct {
	sb        strings.Builder
	max       int
	truncated bool
}

func (t *pdfTextBuilder) full() bool { return t.max > 0 && t.sb.Len() >= t.max }

func (t *pdfTextBuilder) writePage(s string) {
	s = strings.TrimSpace(NormalizeNewlines(s))
	if s == "" {
		return
	}
	if t.sb.Len() > 0 {
		t.sb.WriteString("\n\n")
	}
	if t.max > 0 && t.sb.Len()+len(s) > t.max {
		kept, _ := capText(s, t.max-t.sb.Len())
		t.sb.WriteString(kept)
		t.truncated = true
		return
	}
	t.sb.WriteString(s)
}

func (t *pdfTextBuilder) String() string { return t.sb.String() }
