package documents

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExtractDOCX(t *testing.T) {
	res, err := ExtractOffice(docxBytes(t), mimeDOCX, Options{})
	if err != nil {
		t.Fatalf("ExtractOffice: %v", err)
	}
	if res.Format != "docx" {
		t.Errorf("Format = %q, want docx", res.Format)
	}

	want := "First paragraph\n" +
		"Second paragraph\n" +
		"A1\tB1\n" +
		"A2\tB2\n" +
		"Fish & chips\tafter tab"
	if res.Text != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", res.Text, want)
	}
	if res.Tables != 1 {
		t.Errorf("Tables = %d, want 1", res.Tables)
	}
	if res.Paragraphs != 7 {
		t.Errorf("Paragraphs = %d, want 7", res.Paragraphs)
	}
	if res.Truncated {
		t.Error("Truncated set for a tiny document")
	}
}

func TestExtractDOCXTruncates(t *testing.T) {
	var body strings.Builder
	body.WriteString(`<w:document xmlns:w="x"><w:body>`)
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&body, `<w:p><w:r><w:t>paragraph number %d</w:t></w:r></w:p>`, i)
	}
	body.WriteString(`</w:body></w:document>`)

	data := buildZip(t, []zipEntry{{partWordDocument, []byte(body.String())}})
	res, err := ExtractOffice(data, mimeDOCX, Options{MaxTextBytes: 200})
	if err != nil {
		t.Fatalf("ExtractOffice: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated not set despite the byte cap")
	}
	if len(res.Text) > 260 {
		t.Errorf("text is %d bytes, well past the 200 byte cap", len(res.Text))
	}
}

func TestExtractXLSX(t *testing.T) {
	res, err := ExtractOffice(xlsxBytes(t), mimeXLSX, Options{})
	if err != nil {
		t.Fatalf("ExtractOffice: %v", err)
	}
	if res.Format != "xlsx" || res.Sheets != 1 {
		t.Fatalf("Format/Sheets = %s/%d, want xlsx/1", res.Format, res.Sheets)
	}
	want := "# Inventory\n" +
		"Name\tRichText\n" +
		"Widget\t42\tTRUE\n" +
		"Inline value"
	if res.Text != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", res.Text, want)
	}
}

func TestExtractXLSXWithoutSharedStrings(t *testing.T) {
	sheet := `<worksheet><sheetData><row><c r="A1"><v>7</v></c></row></sheetData></worksheet>`
	data := buildZip(t, []zipEntry{
		{partWorkbook, []byte(`<workbook><sheets><sheet name="S" sheetId="1"/></sheets></workbook>`)},
		{prefixWorksheets + "sheet1.xml", []byte(sheet)},
	})
	res, err := ExtractOffice(data, mimeXLSX, Options{})
	if err != nil {
		t.Fatalf("ExtractOffice: %v", err)
	}
	if !strings.Contains(res.Text, "7") {
		t.Errorf("numeric cell missing from %q", res.Text)
	}
	// Without the rels part the tab name is unknown, so the file name is used.
	if !strings.Contains(res.Text, "# sheet1") {
		t.Errorf("expected a fallback sheet heading, got %q", res.Text)
	}
}

func TestExtractPPTX(t *testing.T) {
	res, err := ExtractOffice(pptxBytes(t, 11), mimePPTX, Options{})
	if err != nil {
		t.Fatalf("ExtractOffice: %v", err)
	}
	if res.Format != "pptx" || res.Slides != 11 {
		t.Fatalf("Format/Slides = %s/%d, want pptx/11", res.Format, res.Slides)
	}
	for i := 1; i <= 11; i++ {
		if !strings.Contains(res.Text, fmt.Sprintf("Title %d", i)) {
			t.Errorf("slide %d title missing", i)
		}
	}
	// slide10 must not sort before slide2.
	if strings.Index(res.Text, "Title 2") > strings.Index(res.Text, "Title 10") {
		t.Errorf("slides came out in lexical rather than numeric order:\n%s", res.Text)
	}
}

func TestExtractOfficeFormatFromContainer(t *testing.T) {
	// An empty or wrong MIME type must not stop extraction: the container is
	// the authority on what it holds.
	for _, mimeType := range []string{"", "application/zip", mimePPTX} {
		res, err := ExtractOffice(docxBytes(t), mimeType, Options{})
		if err != nil {
			t.Fatalf("mime %q: %v", mimeType, err)
		}
		if res.Format != "docx" {
			t.Errorf("mime %q: Format = %q, want docx", mimeType, res.Format)
		}
	}
}

func TestExtractOfficeFailures(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{"not a zip", []byte("plain text"), ErrNotOffice},
		{"zip with no office parts", buildZip(t, []zipEntry{{"readme.txt", []byte("hi")}}), ErrNotOffice},
		{
			"docx with no text runs",
			buildZip(t, []zipEntry{{partWordDocument, []byte(`<w:document><w:body/></w:document>`)}}),
			ErrOfficeEmpty,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractOffice(tc.data, "", Options{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
		})
	}
}

func TestLegacyOfficeError(t *testing.T) {
	err := LegacyOfficeError(".doc")
	if !errors.Is(err, ErrOfficeUnsupported) {
		t.Fatalf("error = %v, want ErrOfficeUnsupported", err)
	}
	if !strings.Contains(err.Error(), ".docx") {
		t.Errorf("message does not suggest the modern format: %v", err)
	}
}

// TestArchiveResourceBounds covers the zip-bomb guards. All three limits are
// enforced before any large allocation happens.
func TestArchiveResourceBounds(t *testing.T) {
	t.Run("entry count", func(t *testing.T) {
		entries := []zipEntry{{partWordDocument, []byte(docxBody)}}
		for i := 0; i < 50; i++ {
			entries = append(entries, zipEntry{fmt.Sprintf("word/filler%d.xml", i), []byte("<x/>")})
		}
		_, err := ExtractOffice(buildZip(t, entries), mimeDOCX, Options{MaxArchiveEntries: 10})
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("error = %v, want ErrArchiveTooLarge", err)
		}
	})

	t.Run("declared decompressed size", func(t *testing.T) {
		// Highly compressible padding: small on disk, large inflated.
		big := strings.Repeat("A", 2<<20)
		data := buildZip(t, []zipEntry{
			{partWordDocument, []byte(docxBody)},
			{"word/padding.xml", []byte(big)},
		})
		if len(data) > 100<<10 {
			t.Fatalf("fixture did not compress; it is %d bytes", len(data))
		}
		_, err := ExtractOffice(data, mimeDOCX, Options{MaxArchiveBytes: 64 << 10})
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("error = %v, want ErrArchiveTooLarge", err)
		}
	})

	t.Run("per-read budget", func(t *testing.T) {
		body := `<w:document><w:body>` + strings.Repeat(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`, 2000) + `</w:body></w:document>`
		data := buildZip(t, []zipEntry{{partWordDocument, []byte(body)}})
		_, err := ExtractOffice(data, mimeDOCX, Options{MaxArchiveBytes: 512})
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("error = %v, want ErrArchiveTooLarge", err)
		}
	})

	t.Run("limits can be disabled explicitly", func(t *testing.T) {
		if _, err := ExtractOffice(docxBytes(t), mimeDOCX, Options{MaxArchiveBytes: -1, MaxArchiveEntries: -1}); err != nil {
			t.Fatalf("negative limits should disable the guard, got %v", err)
		}
	})
}

func TestSafeArchivePath(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "normal", in: "word/document.xml", want: "word/document.xml"},
		{name: "redundant segments", in: "word/./sub/../document.xml", want: "word/document.xml"},
		{name: "traversal", in: "../../etc/passwd", wantErr: true},
		{name: "traversal after a prefix", in: "word/../../etc/passwd", wantErr: true},
		{name: "absolute", in: "/etc/passwd", wantErr: true},
		{name: "windows separators", in: `..\..\Windows\System32\evil.dll`, wantErr: true},
		{name: "drive letter", in: `C:/Windows/evil.dll`, wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeArchivePath(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsafeArchivePath) {
					t.Fatalf("error = %v, want ErrUnsafeArchivePath", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("safeArchivePath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("safeArchivePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractOfficeRejectsEscapingEntry(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{partWordDocument, []byte(docxBody)},
		{"../../../etc/cron.d/pwn", []byte("* * * * * root sh")},
	})
	_, err := ExtractOffice(data, mimeDOCX, Options{})
	if !errors.Is(err, ErrUnsafeArchivePath) {
		t.Fatalf("error = %v, want ErrUnsafeArchivePath", err)
	}
}

func TestExtractOfficeEmbeddedImages(t *testing.T) {
	png := pngBytes(t, 10, 10)
	data := docxBytes(t,
		zipEntry{prefixMediaWord + "image1.png", png},
		zipEntry{prefixMediaWord + "image2.bin", []byte("definitely not an image")},
	)

	t.Run("off by default", func(t *testing.T) {
		res, err := ExtractOffice(data, mimeDOCX, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Images) != 0 {
			t.Errorf("got %d images without opting in", len(res.Images))
		}
	})

	t.Run("opted in", func(t *testing.T) {
		res, err := ExtractOffice(data, mimeDOCX, Options{ExtractEmbeddedImages: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Images) != 1 {
			t.Fatalf("got %d images, want 1 (the non-image entry must be rejected)", len(res.Images))
		}
		if res.Images[0].Name != "image1.png" || res.Images[0].Info.Width != 10 {
			t.Errorf("image = %+v, want the validated 10px png", res.Images[0])
		}
		if len(res.Diagnostics) == 0 {
			t.Error("the rejected media entry produced no diagnostic")
		}
	})

	t.Run("respects the image count cap", func(t *testing.T) {
		entries := []zipEntry{}
		for i := 0; i < 6; i++ {
			entries = append(entries, zipEntry{fmt.Sprintf("%simage%d.png", prefixMediaWord, i), png})
		}
		res, err := ExtractOffice(docxBytes(t, entries...), mimeDOCX,
			Options{ExtractEmbeddedImages: true, MaxEmbeddedImages: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Images) != 2 {
			t.Errorf("got %d images, want the cap of 2", len(res.Images))
		}
	})
}

func TestNumericLess(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"ppt/slides/slide2.xml", "ppt/slides/slide10.xml", true},
		{"ppt/slides/slide10.xml", "ppt/slides/slide2.xml", false},
		{"ppt/slides/slide1.xml", "ppt/slides/slide1.xml", false},
		{"a.xml", "b.xml", true},
	}
	for _, tc := range tests {
		if got := numericLess(tc.a, tc.b); got != tc.want {
			t.Errorf("numericLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestBoundedTextSeparatorPriority(t *testing.T) {
	tb := &boundedText{}
	tb.write("A1")
	tb.sep(" ")
	tb.sep("\t") // a stronger separator replaces a weaker pending one
	tb.write("B1")
	tb.sep("\t")
	tb.sep("\n")
	tb.write("A2")
	tb.sep("\n") // trailing separators never materialise
	if got, want := tb.String(), "A1\tB1\nA2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	empty := &boundedText{}
	empty.sep("\n")
	empty.write("first")
	if got := empty.String(); got != "first" {
		t.Errorf("leading separator leaked: %q", got)
	}
}
