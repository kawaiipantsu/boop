package tools

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// Fixtures are generated in code rather than committed, so the repository
// carries no binary blobs and each test states exactly what shape of file it
// is exercising.

// attachPNG encodes a small PNG through the stdlib encoder.
func attachPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x30, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.String()
}

// attachDOCX builds a minimal OOXML word container with archive/zip.
func attachDOCX(t *testing.T, paragraphs ...string) string {
	t.Helper()
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		fmt.Fprintf(&body, `<w:p><w:r><w:t>%s</w:t></w:r></w:p>`, p)
	}
	body.WriteString(`</w:body></w:document>`)

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	entries := []struct{ name, data string }{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"word/document.xml", body.String()},
	}
	for _, e := range entries {
		f, err := w.Create(e.name)
		if err != nil {
			t.Fatalf("zip create %s: %v", e.name, err)
		}
		if _, err := f.Write([]byte(e.data)); err != nil {
			t.Fatalf("zip write %s: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.String()
}

// attachBuildPDF assembles a syntactically valid PDF with a correct xref
// table. objects[i] is the body of object i+1; object 1 must be the catalog.
func attachBuildPDF(header string, objects []string, trailerExtra string) string {
	var buf bytes.Buffer
	buf.WriteString(header + "\n")
	offsets := make([]int, len(objects)+1)
	for i, body := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R %s>>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, trailerExtra, xref)
	return buf.String()
}

func attachStream(dictExtra, content string) string {
	return fmt.Sprintf("<< /Length %d %s>>\nstream\n%s\nendstream", len(content), dictExtra, content)
}

// attachTextPDF builds a PDF whose pages each show one line of Helvetica text.
func attachTextPDF(pages ...string) string {
	objects := []string{
		"", // catalog
		"", // page tree
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var kids string
	for i, text := range pages {
		pageNum := 4 + i*2
		kids += fmt.Sprintf("%d 0 R ", pageNum)
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", pageNum+1))
		objects = append(objects, attachStream("",
			fmt.Sprintf("BT /F1 24 Tf 72 720 Td (%s) Tj ET", text)))
	}
	objects[0] = "<< /Type /Catalog /Pages 2 0 R >>"
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(pages))
	return attachBuildPDF("%PDF-1.4", objects, "")
}

// attachScannedPDF draws one image XObject and shows no text, which is what a
// scan looks like structurally.
func attachScannedPDF() string {
	jpegPayload := "\xFF\xD8\xFF\xE0fake-jpeg-bytes\xFF\xD9"
	return attachBuildPDF("%PDF-1.4", []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>",
		attachStream("", "q 612 0 0 792 0 0 cm /Im0 Do Q"),
		attachStream("/Type /XObject /Subtype /Image /Width 100 /Height 100 "+
			"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode ", jpegPayload),
	}, "")
}

// attachEncryptedPDF declares an /Encrypt dictionary the reader cannot handle.
func attachEncryptedPDF() string {
	return attachBuildPDF("%PDF-1.4", []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		attachStream("", "BT ET"),
		"<< /Filter /BoopCustomCrypt /V 4 /Length 128 >>",
	}, "/Encrypt 5 0 R /ID [<01020304> <01020304>] ")
}

// attachCorruptPDF is a real PDF with its tail cut off, the commonest form of
// damage: an interrupted download.
func attachCorruptPDF() string {
	full := attachTextPDF("Hello from page one")
	return full[:len(full)*2/3]
}

// attachLegacyDOC is the OLE compound-file magic that .doc/.xls/.ppt carry.
func attachLegacyDOC() string {
	return "\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1" + strings.Repeat("\x00", 512)
}

// --- extraction -----------------------------------------------------------

func TestAttachToolExtractsText(t *testing.T) {
	tests := []struct {
		name         string
		file         string
		body         string
		wantInText   []string
		wantDisplay  []string
		wantKind     string
		wantVision   bool
		wantMinParts int
	}{
		{
			name:         "plain text",
			file:         "notes.txt",
			body:         "alpha\nbeta\ngamma\n",
			wantInText:   []string{"alpha", "gamma", `filename="notes.txt"`},
			wantDisplay:  []string{"text", "3 lines"},
			wantKind:     "text",
			wantMinParts: 1,
		},
		{
			name:         "docx paragraphs",
			file:         "report.docx",
			body:         attachDOCX(t, "First paragraph", "Second paragraph"),
			wantInText:   []string{"First paragraph", "Second paragraph"},
			wantDisplay:  []string{"DOCX"},
			wantKind:     "DOCX",
			wantMinParts: 1,
		},
		{
			name:         "pdf text layer",
			file:         "guide.pdf",
			body:         attachTextPDF("Hello from page one", "And page two"),
			wantInText:   []string{"Hello from page one", "And page two", "pages=2"},
			wantDisplay:  []string{"PDF", "2 pages", "text"},
			wantKind:     "PDF",
			wantMinParts: 1,
		},
		{
			name:         "png image",
			file:         "diagram.png",
			body:         attachPNG(t, 40, 20),
			wantInText:   []string{"is an image", "vision", "nothing in it to read as text"},
			wantDisplay:  []string{"PNG", "40×20", "needs vision"},
			wantKind:     "PNG",
			wantVision:   true,
			wantMinParts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := fsTestWorkspace(t, map[string]string{tc.file: tc.body})
			res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": tc.file})
			if res.IsError {
				t.Fatalf("unexpected error result: %s", res.Content)
			}
			for _, want := range tc.wantInText {
				if !strings.Contains(res.Content, want) {
					t.Errorf("Content is missing %q:\n%s", want, res.Content)
				}
			}
			if res.Display == "" {
				t.Error("Display is empty; a watching user would see nothing about the outcome")
			}
			for _, want := range tc.wantDisplay {
				if !strings.Contains(res.Display, want) {
					t.Errorf("Display = %q, missing %q", res.Display, want)
				}
			}
			data, ok := res.Data.(AttachData)
			if !ok {
				t.Fatalf("Data is %T, want AttachData", res.Data)
			}
			if data.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", data.Kind, tc.wantKind)
			}
			if data.RequiresVision != tc.wantVision {
				t.Errorf("RequiresVision = %v, want %v", data.RequiresVision, tc.wantVision)
			}
			if len(data.Parts) < tc.wantMinParts {
				t.Fatalf("Parts = %d, want at least %d; the caller has nothing to attach",
					len(data.Parts), tc.wantMinParts)
			}
			if data.Path != tc.file {
				t.Errorf("Path = %q, want %q", data.Path, tc.file)
			}
			if data.Reason != "" {
				t.Errorf("Reason = %q on a successful attach", data.Reason)
			}
		})
	}
}

// TestAttachToolImageCarriesPart pins the one thing an image result is for:
// there is no text, so the content part is the whole payload.
func TestAttachToolImageCarriesPart(t *testing.T) {
	body := attachPNG(t, 24, 16)
	ws := fsTestWorkspace(t, map[string]string{"shot.png": body})
	res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": "shot.png"})

	data := res.Data.(AttachData)
	if len(data.Parts) != 1 || data.Parts[0].Kind != provider.PartImage {
		t.Fatalf("Parts = %+v, want a single image part", data.Parts)
	}
	if !bytes.Equal(data.Parts[0].Data, []byte(body)) {
		t.Error("the image part does not carry the original bytes")
	}
	if data.Parts[0].MIMEType != "image/png" {
		t.Errorf("part MIME type = %q, want image/png", data.Parts[0].MIMEType)
	}
	if data.Image == nil || data.Image.Width != 24 || data.Image.Height != 16 {
		t.Errorf("Image = %+v, want 24x16", data.Image)
	}
	if len(data.RequiredCapabilities) != 1 || data.RequiredCapabilities[0] != provider.CapabilityVision {
		t.Errorf("RequiredCapabilities = %v, want [%s]",
			data.RequiredCapabilities, provider.CapabilityVision)
	}
	if strings.Contains(res.Content, "<attachment") {
		t.Errorf("an image result should not pretend to carry text:\n%s", res.Content)
	}
}

// --- failures -------------------------------------------------------------

// TestAttachToolNamedFailures is the central claim of this tool: every known
// way a document can fail comes back with the specific reason and the remedy,
// never a bare "failed".
func TestAttachToolNamedFailures(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		body       string
		wantReason string
		wantPhrase []string
		notPhrase  []string
	}{
		{
			name:       "encrypted pdf names the password problem",
			file:       "secret.pdf",
			body:       attachEncryptedPDF(),
			wantReason: "pdf_encrypted",
			wantPhrase: []string{"encrypted", "password", "unprotected"},
		},
		{
			name:       "scanned pdf points at OCR and vision",
			file:       "scan.pdf",
			body:       attachScannedPDF(),
			wantReason: "pdf_no_text_layer",
			wantPhrase: []string{"no extractable text layer", "scan", "OCR", "vision-capable"},
		},
		{
			name:       "corrupt pdf says it is damaged",
			file:       "broken.pdf",
			body:       attachCorruptPDF(),
			wantReason: "pdf_damaged",
			wantPhrase: []string{"unreadable"},
		},
		{
			name:       "legacy doc says to save as docx",
			file:       "old.doc",
			body:       attachLegacyDOC(),
			wantReason: "legacy_office",
			wantPhrase: []string{"pre-2007", ".docx"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := fsTestWorkspace(t, map[string]string{tc.file: tc.body})
			res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": tc.file})
			if !res.IsError {
				t.Fatalf("expected a failed Result, got success:\n%s", res.Content)
			}
			data, ok := res.Data.(AttachData)
			if !ok {
				t.Fatalf("Data is %T, want AttachData", res.Data)
			}
			if data.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q (message: %s)", data.Reason, tc.wantReason, res.Content)
			}
			for _, want := range tc.wantPhrase {
				if !strings.Contains(res.Content, want) {
					t.Errorf("message is missing %q, which is what makes it actionable:\n%s",
						want, res.Content)
				}
			}
			for _, unwanted := range tc.notPhrase {
				if strings.Contains(res.Content, unwanted) {
					t.Errorf("message contains %q:\n%s", unwanted, res.Content)
				}
			}
			if !strings.Contains(res.Content, tc.file) {
				t.Errorf("message does not name the file:\n%s", res.Content)
			}
			if strings.Contains(res.Content, "documents:") {
				t.Errorf("the package prefix leaked into a user-facing message:\n%s", res.Content)
			}
			if res.Display == "" || res.Display == "attach failed" {
				t.Errorf("Display = %q, want a specific short outcome", res.Display)
			}
			// A generic message is the failure mode this test exists to catch.
			if len(res.Content) < 60 {
				t.Errorf("message is too short to be actionable: %q", res.Content)
			}
		})
	}
}

func TestAttachToolRejectsBadPaths(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"docs/notes.txt": "body",
		"empty.txt":      "",
	})
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"escaping path", map[string]any{"path": "../../etc/passwd"}, "escapes the workspace"},
		{"absolute escape", map[string]any{"path": "/etc/passwd"}, "escapes the workspace"},
		{"missing file", map[string]any{"path": "nope.pdf"}, "file not found"},
		{"directory", map[string]any{"path": "docs"}, "is a directory"},
		{"empty file", map[string]any{"path": "empty.txt"}, "is empty"},
		{"no path", map[string]any{}, `"path" argument is required`},
		{"negative max_chars", map[string]any{"path": "docs/notes.txt", "max_chars": -5}, "max_chars must be"},
		{"negative pages", map[string]any{"path": "docs/notes.txt", "pages": -1}, "pages must be"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := fsTestExec(t, NewAttachTool(ws), tc.args)
			if !res.IsError {
				t.Fatalf("expected a failed Result, got:\n%s", res.Content)
			}
			if !strings.Contains(res.Content, tc.want) {
				t.Errorf("message = %q, want it to contain %q", res.Content, tc.want)
			}
		})
	}
}

// --- caps -----------------------------------------------------------------

func TestAttachToolTruncates(t *testing.T) {
	body := strings.Repeat("a line of perfectly ordinary text\n", 4000)
	ws := fsTestWorkspace(t, map[string]string{"big.txt": body})

	t.Run("max_chars caps the result", func(t *testing.T) {
		res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": "big.txt", "max_chars": 500})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		data := res.Data.(AttachData)
		if !data.Truncated {
			t.Error("Truncated is false for a capped extraction")
		}
		if data.TextBytes > 500 {
			t.Errorf("TextBytes = %d, want at most 500", data.TextBytes)
		}
		if !strings.Contains(res.Content, "truncated") {
			t.Errorf("the model is not told the content is partial:\n%s", res.Content)
		}
		if !strings.Contains(res.Display, "truncated") {
			t.Errorf("Display = %q, want it to report truncation", res.Display)
		}
	})

	t.Run("max_chars cannot raise the ceiling", func(t *testing.T) {
		opts := attachOptions(attachArgs{MaxChars: attachMaxTextBytes * 10})
		if opts.MaxTextBytes != attachMaxTextBytes {
			t.Errorf("MaxTextBytes = %d, want it clamped to %d", opts.MaxTextBytes, attachMaxTextBytes)
		}
	})

	t.Run("oversized source is capped by default", func(t *testing.T) {
		huge := strings.Repeat("x", attachMaxTextBytes+4096)
		ws := fsTestWorkspace(t, map[string]string{"huge.txt": huge})
		res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": "huge.txt"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		data := res.Data.(AttachData)
		if !data.Truncated || data.TextBytes > attachMaxTextBytes {
			t.Errorf("Truncated=%v TextBytes=%d, want truncation at %d",
				data.Truncated, data.TextBytes, attachMaxTextBytes)
		}
	})
}

func TestAttachToolPageLimit(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"long.pdf": attachTextPDF("Page one text", "Page two text", "Page three text"),
	})
	res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": "long.pdf", "pages": 1})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Page one text") {
		t.Errorf("first page missing:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "Page two text") {
		t.Errorf("pages=1 did not stop extraction:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "raise pages") {
		t.Errorf("the model is not told more pages exist:\n%s", res.Content)
	}
}

// --- permission -----------------------------------------------------------

func TestAttachToolPermission(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"report.pdf": attachTextPDF("content"),
		"logo.png":   attachPNG(t, 8, 8),
		".env":       "TOKEN=redacted-in-this-fixture",
	})
	tool := NewAttachTool(ws)

	tests := []struct {
		name        string
		path        string
		wantRisk    permissions.Risk
		wantSummary []string
	}{
		{"pdf reads well in a prompt", "report.pdf", permissions.RiskLow,
			[]string{"Attach report.pdf (", "PDF)"}},
		{"image names its format", "logo.png", permissions.RiskLow,
			[]string{"Attach logo.png (", "PNG)"}},
		{"credential file is raised", ".env", permissions.RiskMedium,
			[]string{"Attach .env"}},
		{"escaping path is critical", "../../etc/passwd", permissions.RiskCritical,
			[]string{"Attach "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action, err := tool.Permission(fsTestCall(t, tool.Name(), map[string]any{"path": tc.path}))
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if action.Category != permissions.CatFilesystemRead {
				t.Errorf("Category = %q, want %q", action.Category, permissions.CatFilesystemRead)
			}
			if action.Risk != tc.wantRisk {
				t.Errorf("Risk = %q, want %q", action.Risk, tc.wantRisk)
			}
			for _, want := range tc.wantSummary {
				if !strings.Contains(action.Summary, want) {
					t.Errorf("Summary = %q, missing %q", action.Summary, want)
				}
			}
			if action.Tool != "attach" {
				t.Errorf("Tool = %q, want attach", action.Tool)
			}
		})
	}
}

func TestAttachToolSchema(t *testing.T) {
	tool := NewAttachTool(fsTestWorkspace(t, nil))
	var _ Tool = tool

	schema := tool.Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object: %#v", schema)
	}
	for _, name := range []string{"path", "max_chars", "pages"} {
		if _, ok := props[name]; !ok {
			t.Errorf("schema is missing the %q property", name)
		}
	}
	req, ok := schema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v, want [path]", schema["required"])
	}
	if tool.Description() == "" {
		t.Error("Description is empty")
	}
}
