package documents

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		data        []byte
		wantHandler Handler
		wantKindOf  provider.PartKind
		wantVision  bool
		wantInText  string
	}{
		{
			name: "text file", filename: "notes.md", data: []byte("# Title\n\nSome body text.\n"),
			wantHandler: HandlerText, wantKindOf: provider.PartText, wantInText: "Some body text",
		},
		{
			name: "source file", filename: "main.go", data: []byte("package main\n\nfunc main() {}\n"),
			wantHandler: HandlerText, wantKindOf: provider.PartText, wantInText: "func main",
		},
		{
			name: "png", filename: "shot.png", data: pngBytes(t, 16, 9),
			wantHandler: HandlerImage, wantKindOf: provider.PartImage, wantVision: true,
		},
		{
			name: "pdf", filename: "report.pdf", data: textPDF("Quarterly numbers"),
			wantHandler: HandlerPDF, wantKindOf: provider.PartText, wantInText: "Quarterly numbers",
		},
		{
			name: "docx", filename: "letter.docx", data: docxBytes(t),
			wantHandler: HandlerOffice, wantKindOf: provider.PartText, wantInText: "First paragraph",
		},
		{
			name: "xlsx", filename: "book.xlsx", data: xlsxBytes(t),
			wantHandler: HandlerOffice, wantKindOf: provider.PartText, wantInText: "Inventory",
		},
		{
			name: "pptx", filename: "deck.pptx", data: pptxBytes(t, 2),
			wantHandler: HandlerOffice, wantKindOf: provider.PartText, wantInText: "Title 2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.filename, tc.data)

			doc, err := Load(path, Options{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if doc.Type.Handler != tc.wantHandler {
				t.Errorf("Handler = %q, want %q", doc.Type.Handler, tc.wantHandler)
			}
			if doc.Path != path || doc.Filename != tc.filename {
				t.Errorf("path/filename = %s/%s, want %s/%s", doc.Path, doc.Filename, path, tc.filename)
			}
			if doc.Size != int64(len(tc.data)) {
				t.Errorf("Size = %d, want %d", doc.Size, len(tc.data))
			}
			if len(doc.Parts) == 0 {
				t.Fatal("no content parts produced")
			}
			if doc.Parts[0].Kind != tc.wantKindOf {
				t.Errorf("first part kind = %q, want %q", doc.Parts[0].Kind, tc.wantKindOf)
			}
			if got := doc.RequiresVision(); got != tc.wantVision {
				t.Errorf("RequiresVision = %v, want %v", got, tc.wantVision)
			}
			if tc.wantInText != "" && !strings.Contains(doc.Text, tc.wantInText) {
				t.Errorf("Text is missing %q:\n%s", tc.wantInText, doc.Text)
			}
			if doc.Summary() == "" {
				t.Error("Summary() is empty")
			}
		})
	}
}

// TestLoadTextPartIsLabelled pins the attachment framing. Without a boundary a
// model cannot tell attachment content from the user's own instructions.
func TestLoadTextPartIsLabelled(t *testing.T) {
	doc, err := LoadBytes("notes.txt", []byte("body text"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	part := doc.Parts[0]
	if !strings.HasPrefix(part.Text, `<attachment filename="notes.txt"`) {
		t.Errorf("part does not open with an attachment tag:\n%s", part.Text)
	}
	if !strings.HasSuffix(part.Text, "</attachment>") {
		t.Errorf("part does not close the attachment tag:\n%s", part.Text)
	}
	if !strings.Contains(part.Text, "body text") {
		t.Error("the file content is missing from the part")
	}
}

func TestLoadTruncationIsAnnounced(t *testing.T) {
	doc, err := LoadBytes("big.txt", []byte(strings.Repeat("line of text\n", 500)), Options{MaxTextBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Truncated {
		t.Fatal("Truncated not set")
	}
	if !strings.Contains(doc.Parts[0].Text, "truncated") {
		t.Errorf("the model is not told the attachment is partial:\n%s", doc.Parts[0].Text)
	}
}

func TestLoadFailures(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope.txt"), Options{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("directory", func(t *testing.T) {
		if _, err := Load(t.TempDir(), Options{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("too large", func(t *testing.T) {
		path := writeTemp(t, "big.txt", []byte(strings.Repeat("x", 4096)))
		_, err := Load(path, Options{MaxFileBytes: 1024})
		if !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("error = %v, want ErrFileTooLarge", err)
		}
	})

	t.Run("unsupported type", func(t *testing.T) {
		doc, err := LoadBytes("archive.zip", buildZip(t, []zipEntry{{"a.txt", []byte("hi")}}), Options{})
		if !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("error = %v, want ErrUnsupportedType", err)
		}
		// The detection result must survive the failure so the UI can still
		// say what the file was.
		if doc == nil || doc.Type.MIMEType != "application/zip" {
			t.Errorf("doc = %+v, want the detection result to be populated", doc)
		}
		if !strings.Contains(err.Error(), "Supported:") {
			t.Errorf("message does not list what would work: %v", err)
		}
	})

	t.Run("legacy office", func(t *testing.T) {
		_, err := LoadBytes("old.doc", []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1padding padding"), Options{})
		if !errors.Is(err, ErrOfficeUnsupported) {
			t.Fatalf("error = %v, want ErrOfficeUnsupported", err)
		}
	})

	t.Run("binary named as text", func(t *testing.T) {
		_, err := LoadBytes("notes.txt", []byte{0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE, 0x7F}, Options{})
		if !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("error = %v, want ErrUnsupportedType", err)
		}
	})

	t.Run("scanned pdf keeps metadata", func(t *testing.T) {
		doc, err := LoadBytes("scan.pdf", scannedPDF(), Options{})
		if !errors.Is(err, ErrPDFNoTextLayer) {
			t.Fatalf("error = %v, want ErrPDFNoTextLayer", err)
		}
		if doc.PDF == nil || doc.PDF.Pages != 1 {
			t.Errorf("PDF metadata lost on failure: %+v", doc.PDF)
		}
	})
}

func TestDocumentMismatchDiagnostic(t *testing.T) {
	doc, err := LoadBytes("innocent.txt", pngBytes(t, 4, 4), Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(doc.Diagnostics) == 0 {
		t.Fatal("a mislabelled file produced no diagnostic")
	}
	if !strings.Contains(doc.Diagnostics[0], "image/png") {
		t.Errorf("diagnostic does not say what the bytes really are: %q", doc.Diagnostics[0])
	}
}

func TestDocumentCheckCapabilities(t *testing.T) {
	available := []provider.Model{
		{ID: "llava", Provider: "ollama", Capabilities: provider.Capabilities{provider.CapabilityVision}},
	}

	image, err := LoadBytes("shot.png", pngBytes(t, 8, 8), Options{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := LoadBytes("notes.txt", []byte("hello"), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := text.CheckCapabilities("ollama", "qwen", nil, available); err != nil {
		t.Errorf("a text attachment demanded a capability: %v", err)
	}
	if err := image.CheckCapabilities("ollama", "llava", provider.Capabilities{provider.CapabilityVision}, available); err != nil {
		t.Errorf("a vision model was rejected: %v", err)
	}

	err = image.CheckCapabilities("ollama", "qwen", provider.Capabilities{provider.CapabilityTools}, available)
	if err == nil {
		t.Fatal("an image was allowed through to a text-only model")
	}
	if !strings.Contains(err.Error(), "ollama/llava") {
		t.Errorf("message does not suggest the capable model: %v", err)
	}
}

func TestDocumentPartsFor(t *testing.T) {
	vision := provider.Capabilities{provider.CapabilityVision}
	textOnly := provider.Capabilities{provider.CapabilityTools}

	t.Run("image only cannot degrade", func(t *testing.T) {
		doc, err := LoadBytes("shot.png", pngBytes(t, 8, 8), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if parts, _, err := doc.PartsFor(vision); err != nil || len(parts) != 1 {
			t.Fatalf("vision model got %d parts, err %v", len(parts), err)
		}
		if _, _, err := doc.PartsFor(textOnly); err == nil {
			t.Fatal("an image-only document was accepted by a text-only model")
		}
	})

	t.Run("document with images degrades to text", func(t *testing.T) {
		data := docxBytes(t, zipEntry{prefixMediaWord + "image1.png", pngBytes(t, 8, 8)})
		doc, err := LoadBytes("letter.docx", data, Options{ExtractEmbeddedImages: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Parts) != 2 || !doc.RequiresVision() {
			t.Fatalf("expected a text part plus an image part, got %d parts (vision=%v)",
				len(doc.Parts), doc.RequiresVision())
		}

		parts, notes, err := doc.PartsFor(textOnly)
		if err != nil {
			t.Fatalf("a text-only model was refused a document it can read: %v", err)
		}
		if len(parts) != 1 || parts[0].Kind != provider.PartText {
			t.Fatalf("degraded parts = %+v, want just the text part", parts)
		}
		if len(notes) == 0 || !strings.Contains(notes[0], "dropped") {
			t.Errorf("dropping the image was not reported: %v", notes)
		}
	})
}

func TestRawDocumentPart(t *testing.T) {
	data := textPDF("Raw bytes test")

	doc, err := LoadBytes("r.pdf", data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Parts {
		if p.Kind == provider.PartDocument {
			t.Fatal("a raw document part was emitted without opting in")
		}
	}

	doc, err = LoadBytes("r.pdf", data, Options{RawDocumentPart: true})
	if err != nil {
		t.Fatal(err)
	}
	var raw *provider.ContentPart
	for i := range doc.Parts {
		if doc.Parts[i].Kind == provider.PartDocument {
			raw = &doc.Parts[i]
		}
	}
	if raw == nil {
		t.Fatal("no raw document part despite opting in")
	}
	if raw.MIMEType != "application/pdf" || len(raw.Data) != len(data) {
		t.Errorf("raw part = %s / %d bytes, want application/pdf / %d", raw.MIMEType, len(raw.Data), len(data))
	}
}

func TestOptionsDefaults(t *testing.T) {
	got := DefaultOptions()
	if got.MaxFileBytes != defaultMaxFileBytes || got.MaxTextBytes != defaultMaxTextBytes {
		t.Errorf("defaults not applied: %+v", got)
	}

	// A negative value disables a limit; it must not be replaced by the default.
	disabled := Options{MaxFileBytes: -1, MaxTextBytes: -1, MaxPDFPages: -1}.withDefaults()
	if disabled.MaxFileBytes != 0 || disabled.MaxTextBytes != 0 || disabled.MaxPDFPages != 0 {
		t.Errorf("negative values did not disable the limits: %+v", disabled)
	}

	explicit := Options{MaxTextBytes: 77}.withDefaults()
	if explicit.MaxTextBytes != 77 {
		t.Errorf("MaxTextBytes = %d, want the caller's 77", explicit.MaxTextBytes)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		2048:    "2.0 KiB",
		5 << 20: "5.0 MiB",
		3 << 30: "3.0 GiB",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
