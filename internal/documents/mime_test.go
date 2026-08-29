package documents

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDetect(t *testing.T) {
	pngData := pngBytes(t, 4, 4)
	zipData := docxBytes(t)
	pdfData := textPDF("hi")

	tests := []struct {
		name         string
		filename     string
		data         []byte
		wantMIME     string
		wantHandler  Handler
		wantText     bool
		wantMismatch bool
	}{
		{
			name: "plain text", filename: "notes.txt", data: []byte("hello world\n"),
			wantMIME: "text/plain", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "markdown refines a plain sniff", filename: "README.md", data: []byte("# Title\n\nBody\n"),
			wantMIME: "text/markdown", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "go source refines a plain sniff", filename: "main.go", data: []byte("package main\n"),
			wantMIME: "text/x-go", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "json", filename: "data.json", data: []byte(`{"a":1}`),
			wantMIME: "application/json", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "yaml", filename: "config.yaml", data: []byte("a: 1\n"),
			wantMIME: "application/yaml", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "png", filename: "logo.png", data: pngData,
			wantMIME: "image/png", wantHandler: HandlerImage,
		},
		{
			name: "webp", filename: "photo.webp", data: webpLosslessBytes(8, 6),
			wantMIME: "image/webp", wantHandler: HandlerImage,
		},
		{
			name: "pdf", filename: "report.pdf", data: pdfData,
			wantMIME: "application/pdf", wantHandler: HandlerPDF,
		},
		{
			name: "docx is a zip refined by extension", filename: "letter.docx", data: zipData,
			wantMIME: mimeDOCX, wantHandler: HandlerOffice,
		},
		{
			// The bytes win: a PNG named .txt is still handled as an image.
			name: "image mislabelled as text", filename: "payload.txt", data: pngData,
			wantMIME: "image/png", wantHandler: HandlerImage, wantMismatch: true,
		},
		{
			// The dangerous direction: a zip named .txt must not be treated as
			// text, and must not be silently unpacked either.
			name: "zip mislabelled as text", filename: "payload.txt", data: zipData,
			wantMIME: "application/zip", wantHandler: HandlerUnsupported, wantMismatch: true,
		},
		{
			name: "opaque binary claiming png", filename: "fake.png", data: []byte{0x00, 0x01, 0x02, 0x03, 0x7F, 0x00, 0xFE},
			wantMIME: "application/octet-stream", wantHandler: HandlerUnsupported, wantMismatch: true,
		},
		{
			name: "unknown extension with text bytes", filename: "notes.weird", data: []byte("plain content"),
			wantMIME: "text/plain", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "empty file", filename: "empty.txt", data: nil,
			wantMIME: "text/plain", wantHandler: HandlerText, wantText: true,
		},
		{
			name: "legacy word binary", filename: "old.doc", data: []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1rest of an OLE file"),
			wantMIME: "application/octet-stream", wantHandler: HandlerUnsupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(tc.filename, tc.data)
			if got.MIMEType != tc.wantMIME {
				t.Errorf("MIMEType = %q, want %q", got.MIMEType, tc.wantMIME)
			}
			if got.Handler != tc.wantHandler {
				t.Errorf("Handler = %q, want %q", got.Handler, tc.wantHandler)
			}
			if got.IsText != tc.wantText {
				t.Errorf("IsText = %v, want %v", got.IsText, tc.wantText)
			}
			if got.Mismatch != tc.wantMismatch {
				t.Errorf("Mismatch = %v, want %v (sniffed %q, extension %q)",
					got.Mismatch, tc.wantMismatch, got.Sniffed, got.FromExtension)
			}
		})
	}
}

// TestDetectPrefersBytesOverExtension states the security property directly:
// no extension may promote bytes into a handler the sniff did not support.
func TestDetectPrefersBytesOverExtension(t *testing.T) {
	pngData := pngBytes(t, 2, 2)
	for _, name := range []string{"a.txt", "a.md", "a.docx", "a.pdf", "a.json", "a.go"} {
		got := Detect(name, pngData)
		if got.MIMEType != "image/png" {
			t.Errorf("Detect(%q, pngBytes).MIMEType = %q, want image/png", name, got.MIMEType)
		}
	}
	zipData := docxBytes(t)
	for _, name := range []string{"a.txt", "a.pdf", "a.png"} {
		got := Detect(name, zipData)
		if got.Handler == HandlerText || got.Handler == HandlerImage || got.Handler == HandlerPDF {
			t.Errorf("Detect(%q, zipBytes).Handler = %q, want the zip not to be promoted", name, got.Handler)
		}
	}
}

func TestDetectFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.md")
	if err := os.WriteFile(path, []byte("# Heading\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DetectFile(path)
	if err != nil {
		t.Fatalf("DetectFile: %v", err)
	}
	if got.MIMEType != "text/markdown" || got.Handler != HandlerText {
		t.Errorf("got %+v, want text/markdown handled as text", got)
	}

	if _, err := DetectFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("DetectFile on a missing path returned no error")
	}
}

func TestIsTextMIME(t *testing.T) {
	tests := map[string]bool{
		"text/plain":                true,
		"TEXT/PLAIN; charset=utf-8": true,
		"application/json":          true,
		"application/ld+json":       true,
		"image/svg+xml":             true,
		"application/pdf":           false,
		"image/png":                 false,
		"application/octet-stream":  false,
	}
	for in, want := range tests {
		if got := IsTextMIME(in); got != want {
			t.Errorf("IsTextMIME(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSupportedExtensions(t *testing.T) {
	got := SupportedExtensions()
	if len(got) == 0 {
		t.Fatal("SupportedExtensions returned nothing")
	}
	if !sort.StringsAreSorted(got) {
		t.Error("SupportedExtensions is not sorted, so CLI output would be unstable")
	}
	for _, ext := range []string{".pdf", ".docx", ".png", ".go", ".md"} {
		if !contains(got, ext) {
			t.Errorf("SupportedExtensions is missing %q", ext)
		}
	}
	// Types with no handler must not be advertised.
	for _, ext := range []string{".zip", ".bmp", ".tiff"} {
		if contains(got, ext) {
			t.Errorf("SupportedExtensions advertises %q, which has no handler", ext)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
