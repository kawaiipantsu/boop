package documents

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SniffLen is the number of leading bytes inspected by content sniffing.
//
// It matches the window http.DetectContentType uses; reading more buys nothing.
const SniffLen = 512

// Handler names the extraction path chosen for a file.
//
// It is deliberately coarser than the MIME type: several MIME types share one
// implementation (all OOXML containers are unzipped the same way), and callers
// switch on the handler rather than re-deriving it from the type string.
type Handler string

const (
	// HandlerText covers plain text, markup and source code.
	HandlerText Handler = "text"
	// HandlerImage covers raster images that can become a vision attachment.
	HandlerImage Handler = "image"
	// HandlerPDF covers PDF text-layer extraction.
	HandlerPDF Handler = "pdf"
	// HandlerOffice covers the OOXML zip containers (docx/xlsx/pptx).
	HandlerOffice Handler = "office"
	// HandlerUnsupported means Boop can identify the bytes but not use them.
	HandlerUnsupported Handler = "unsupported"
)

// TypeInfo is the outcome of MIME detection for a single file.
type TypeInfo struct {
	// MIMEType is the effective type Boop acts on.
	MIMEType string `json:"mime_type"`
	// Sniffed is what the leading bytes claim, via http.DetectContentType.
	Sniffed string `json:"sniffed,omitempty"`
	// FromExtension is what the filename claimed, empty when unknown.
	FromExtension string `json:"from_extension,omitempty"`
	// Extension is the lower-cased filename extension including the dot.
	Extension string `json:"extension,omitempty"`
	// IsText reports whether the effective type is human-readable text.
	IsText bool `json:"is_text"`
	// Handler is the extraction path for MIMEType.
	Handler Handler `json:"handler"`
	// Mismatch reports that the extension claimed something the bytes
	// contradict. Detection always resolves in favour of the bytes; the flag
	// exists so the UI can warn that a file is not what it is named.
	Mismatch bool `json:"mismatch,omitempty"`
}

// String renders the detection result for logs and the CLI.
func (t TypeInfo) String() string {
	if t.Mismatch {
		return fmt.Sprintf("%s (handler %s; extension claimed %s)", t.MIMEType, t.Handler, t.FromExtension)
	}
	return fmt.Sprintf("%s (handler %s)", t.MIMEType, t.Handler)
}

// Supported reports whether Boop has an extraction path for this type.
func (t TypeInfo) Supported() bool { return t.Handler != HandlerUnsupported }

// extensionTypes maps filename extensions to MIME types.
//
// Boop carries its own table rather than relying on mime.TypeByExtension,
// whose results depend on the host's /etc/mime.types and therefore differ
// between the machines Boop is meant to behave identically on.
var extensionTypes = map[string]string{
	// Plain text and markup.
	".txt": "text/plain", ".text": "text/plain", ".log": "text/plain",
	".md": "text/markdown", ".markdown": "text/markdown", ".mdx": "text/markdown",
	".rst": "text/x-rst", ".adoc": "text/plain", ".org": "text/plain",
	".csv": "text/csv", ".tsv": "text/tab-separated-values",
	".json": "application/json", ".jsonl": "application/json", ".ndjson": "application/json",
	".xml": "application/xml", ".xsd": "application/xml", ".xsl": "application/xml",
	".svg":  "image/svg+xml",
	".yaml": "application/yaml", ".yml": "application/yaml",
	".toml": "application/toml", ".ini": "text/plain", ".cfg": "text/plain",
	".conf": "text/plain", ".properties": "text/plain", ".env": "text/plain",
	".html": "text/html", ".htm": "text/html", ".xhtml": "application/xhtml+xml",
	".css": "text/css", ".scss": "text/css", ".less": "text/css",
	".sql": "application/sql", ".diff": "text/x-diff", ".patch": "text/x-diff",

	// Source code.
	".go": "text/x-go", ".mod": "text/x-go", ".sum": "text/plain",
	".c": "text/x-c", ".h": "text/x-c", ".cc": "text/x-c++", ".cpp": "text/x-c++",
	".cxx": "text/x-c++", ".hpp": "text/x-c++", ".hh": "text/x-c++",
	".cs": "text/x-csharp", ".java": "text/x-java", ".kt": "text/x-kotlin",
	".kts": "text/x-kotlin", ".scala": "text/x-scala", ".swift": "text/x-swift",
	".rs": "text/x-rust", ".rb": "text/x-ruby", ".php": "text/x-php",
	".py": "text/x-python", ".pyi": "text/x-python", ".pl": "text/x-perl",
	".pm": "text/x-perl", ".lua": "text/x-lua", ".r": "text/x-r",
	".js": "text/javascript", ".mjs": "text/javascript", ".cjs": "text/javascript",
	".jsx": "text/javascript", ".ts": "text/x-typescript", ".tsx": "text/x-typescript",
	".sh": "text/x-shellscript", ".bash": "text/x-shellscript",
	".zsh": "text/x-shellscript", ".fish": "text/x-shellscript",
	".ps1": "text/x-powershell", ".bat": "text/x-msdos-batch", ".cmd": "text/x-msdos-batch",
	".dockerfile": "text/plain", ".makefile": "text/x-makefile", ".mk": "text/x-makefile",
	".tf": "text/x-terraform", ".hcl": "text/x-terraform", ".proto": "text/x-protobuf",
	".vue": "text/plain", ".svelte": "text/plain", ".tmpl": "text/plain", ".gotmpl": "text/plain",

	// Images.
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".jpe": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff",

	// Documents.
	".pdf":  "application/pdf",
	".docx": mimeDOCX, ".xlsx": mimeXLSX, ".pptx": mimePPTX,
	".zip": "application/zip",
}

// OOXML container MIME types.
const (
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// textTypes are non-"text/" MIME types whose payload is still readable text.
var textTypes = map[string]bool{
	"application/json":         true,
	"application/xml":          true,
	"application/yaml":         true,
	"application/x-yaml":       true,
	"application/toml":         true,
	"application/sql":          true,
	"application/javascript":   true,
	"application/x-javascript": true,
	"application/xhtml+xml":    true,
	"image/svg+xml":            true,
}

// imageHandlers are the raster types Boop can validate and attach. Formats
// outside this set (BMP, TIFF) are recognised but rejected: there is no stdlib
// decoder for them, and no provider accepts them either.
var imageHandlers = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

var officeHandlers = map[string]bool{
	mimeDOCX: true,
	mimeXLSX: true,
	mimePPTX: true,
}

// IsTextMIME reports whether a MIME type carries readable text.
func IsTextMIME(mimeType string) bool {
	mimeType = normalizeMIME(mimeType)
	switch {
	case strings.HasPrefix(mimeType, "text/"):
		return true
	case textTypes[mimeType]:
		return true
	case strings.HasSuffix(mimeType, "+json"), strings.HasSuffix(mimeType, "+xml"):
		return true
	default:
		return false
	}
}

// normalizeMIME lower-cases a media type and drops its parameters, so that
// "text/plain; charset=utf-16le" and "TEXT/PLAIN" compare equal.
func normalizeMIME(mimeType string) string {
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	return strings.ToLower(strings.TrimSpace(mimeType))
}

// genericSniff reports whether a sniffed type is a "no idea" answer that a
// specific extension may legitimately refine.
func genericSniff(mimeType string) bool {
	return mimeType == "text/plain" || mimeType == "application/octet-stream"
}

// Detect classifies a file from its name and leading bytes.
//
// Both signals are consulted, and the bytes win whenever they disagree. An
// extension is attacker-controlled metadata: honouring ".txt" on a file whose
// bytes are a zip, or ".png" on something that is not an image, would let a
// caller choose Boop's handler for it. The extension is used only to *refine*
// an inconclusive sniff into a compatible, more specific type — never to
// override a confident one, and never to move a file into a different handler.
//
// head may be shorter than SniffLen; it may also be the whole file.
func Detect(filename string, head []byte) TypeInfo {
	if len(head) > SniffLen {
		head = head[:SniffLen]
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		// Extension-less well-known names still carry intent.
		ext = "." + strings.ToLower(filepath.Base(filename))
	}
	fromExt := extensionTypes[ext]
	sniffed := normalizeMIME(http.DetectContentType(head))

	effective, mismatch := reconcile(sniffed, fromExt)

	info := TypeInfo{
		MIMEType:      effective,
		Sniffed:       sniffed,
		FromExtension: fromExt,
		Extension:     filepath.Ext(strings.ToLower(filename)),
		IsText:        IsTextMIME(effective),
		Mismatch:      mismatch,
	}
	info.Handler = handlerFor(effective, info.IsText)
	return info
}

// reconcile resolves the sniffed and extension-claimed types into the one Boop
// acts on, reporting whether they materially disagreed.
func reconcile(sniffed, fromExt string) (effective string, mismatch bool) {
	switch {
	case fromExt == "", sniffed == fromExt:
		return sniffed, false

	// A plain-text sniff plus a text extension is a refinement, not a
	// conflict: ".md" and ".go" both sniff as text/plain and stay text.
	case sniffed == "text/plain" && IsTextMIME(fromExt):
		return fromExt, false

	// OOXML files are zips; the sniff can only ever say "zip". Trusting the
	// extension here does not change the handler class, and office.go
	// re-verifies the container contents before believing it.
	case sniffed == "application/zip" && officeHandlers[fromExt]:
		return fromExt, false

	// Go's sniffer does not know every text-ish binary-adjacent format; when
	// it gives up entirely we keep octet-stream rather than let the name
	// promote the file into a real handler.
	case genericSniff(sniffed):
		return sniffed, true

	default:
		return sniffed, true
	}
}

func handlerFor(mimeType string, isText bool) Handler {
	switch {
	case imageHandlers[mimeType]:
		return HandlerImage
	case mimeType == "application/pdf":
		return HandlerPDF
	case officeHandlers[mimeType]:
		return HandlerOffice
	case isText:
		return HandlerText
	default:
		return HandlerUnsupported
	}
}

// DetectFile classifies a file on disk, reading only its head.
func DetectFile(path string) (TypeInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return TypeInfo{}, fmt.Errorf("documents: open %s: %w", path, err)
	}
	defer f.Close()

	head := make([]byte, SniffLen)
	n, err := f.Read(head)
	if err != nil && n == 0 && !errors.Is(err, io.EOF) {
		return TypeInfo{}, fmt.Errorf("documents: read %s: %w", path, err)
	}
	return Detect(filepath.Base(path), head[:n]), nil
}

// SupportedExtensions lists the extensions Boop has a handler for, sorted.
//
// The CLI uses it for help text and completion, so it is derived from the same
// table detection uses instead of being duplicated by hand.
func SupportedExtensions() []string {
	out := make([]string, 0, len(extensionTypes))
	for ext, mimeType := range extensionTypes {
		if handlerFor(mimeType, IsTextMIME(mimeType)) == HandlerUnsupported {
			continue
		}
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}
