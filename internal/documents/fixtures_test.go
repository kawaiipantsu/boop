package documents

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// Fixtures are generated in code rather than committed, so the repository
// carries no binary blobs and every test states exactly what it is exercising.

func solidImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xFF})
		}
	}
	return img
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, solidImage(w, h)); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, solidImage(w, h), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func gifBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, solidImage(w, h), nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// webpLosslessBytes builds a minimal RIFF/WEBP VP8L container. The stdlib has
// no WebP encoder, and only the header is under test.
func webpLosslessBytes(w, h int) []byte {
	body := make([]byte, 13)
	copy(body[0:4], "VP8L")
	binary.LittleEndian.PutUint32(body[4:8], 5)
	body[8] = 0x2F
	binary.LittleEndian.PutUint32(body[9:13], uint32(w-1)|uint32(h-1)<<14)

	out := make([]byte, 0, 12+len(body))
	out = append(out, "RIFF"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(4+len(body)))
	out = append(out, "WEBP"...)
	return append(out, body...)
}

// --- PDF ------------------------------------------------------------------

// buildPDF assembles a syntactically valid PDF with a correct xref table.
// objects[i] is the body of object i+1; object 1 must be the catalog.
func buildPDF(objects []string, trailerExtra string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
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
	return buf.Bytes()
}

func streamObj(dictExtra, content string) string {
	return fmt.Sprintf("<< /Length %d %s>>\nstream\n%s\nendstream", len(content), dictExtra, content)
}

// textPDF builds a PDF whose pages each show one line of Helvetica text.
func textPDF(pages ...string) []byte {
	// Object layout: 1 catalog, 2 page tree, 3 font, then page/content pairs.
	objects := []string{
		"", // catalog, filled in below
		"", // page tree
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var kids string
	for i, text := range pages {
		pageNum := 4 + i*2
		contentNum := pageNum + 1
		kids += fmt.Sprintf("%d 0 R ", pageNum)
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentNum))
		objects = append(objects, streamObj("",
			fmt.Sprintf("BT /F1 24 Tf 72 720 Td (%s) Tj ET", text)))
	}
	objects[0] = "<< /Type /Catalog /Pages 2 0 R >>"
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(pages))
	return buildPDF(objects, "")
}

// scannedPDF builds a page that draws one image XObject and shows no text,
// which is what a scan looks like structurally.
func scannedPDF() []byte {
	jpegPayload := "\xFF\xD8\xFF\xE0fake-jpeg-bytes\xFF\xD9"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>",
		streamObj("", "q 612 0 0 792 0 0 cm /Im0 Do Q"),
		streamObj("/Type /XObject /Subtype /Image /Width 100 /Height 100 "+
			"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode ", jpegPayload),
	}
	return buildPDF(objects, "")
}

// encryptedPDF declares an /Encrypt dictionary the reader cannot handle.
func encryptedPDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		streamObj("", "BT ET"),
		"<< /Filter /BoopCustomCrypt /V 4 /Length 128 >>",
	}
	return buildPDF(objects, "/Encrypt 5 0 R /ID [<01020304> <01020304>] ")
}

// --- OOXML ----------------------------------------------------------------

type zipEntry struct {
	name string
	body []byte
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := w.Create(e.name)
		if err != nil {
			t.Fatalf("zip create %s: %v", e.name, err)
		}
		if _, err := f.Write(e.body); err != nil {
			t.Fatalf("zip write %s: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

const docxBody = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>First paragraph</w:t></w:r></w:p>
    <w:p><w:r><w:t xml:space="preserve">Second </w:t></w:r><w:r><w:t>paragraph</w:t></w:r></w:p>
    <w:tbl>
      <w:tr><w:tc><w:p><w:r><w:t>A1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>B1</w:t></w:r></w:p></w:tc></w:tr>
      <w:tr><w:tc><w:p><w:r><w:t>A2</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>B2</w:t></w:r></w:p></w:tc></w:tr>
    </w:tbl>
    <w:p><w:r><w:t>Fish &amp; chips</w:t></w:r><w:r><w:tab/><w:t>after tab</w:t></w:r></w:p>
  </w:body>
</w:document>`

func docxBytes(t *testing.T, extra ...zipEntry) []byte {
	t.Helper()
	entries := []zipEntry{
		{"[Content_Types].xml", []byte(`<?xml version="1.0"?><Types/>`)},
		{partWordDocument, []byte(docxBody)},
	}
	return buildZip(t, append(entries, extra...))
}

func xlsxBytes(t *testing.T) []byte {
	t.Helper()
	shared := `<?xml version="1.0"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="3" uniqueCount="3">
  <si><t>Name</t></si>
  <si><t>Widget</t></si>
  <si><r><t>Rich</t></r><r><t>Text</t></r></si>
</sst>`
	workbook := `<?xml version="1.0"?>
<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets><sheet name="Inventory" sheetId="1" r:id="rId1"/></sheets>
</workbook>`
	rels := `<?xml version="1.0"?>
<Relationships><Relationship Id="rId1" Type="worksheet" Target="worksheets/sheet1.xml"/></Relationships>`
	sheet := `<?xml version="1.0"?>
<worksheet><sheetData>
  <row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>2</v></c></row>
  <row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2"><v>42</v></c><c r="C2" t="b"><v>1</v></c></row>
  <row r="3"><c r="A3" t="inlineStr"><is><t>Inline value</t></is></c></row>
</sheetData></worksheet>`
	return buildZip(t, []zipEntry{
		{"[Content_Types].xml", []byte(`<?xml version="1.0"?><Types/>`)},
		{partSharedStrings, []byte(shared)},
		{partWorkbook, []byte(workbook)},
		{partWorkbookRels, []byte(rels)},
		{prefixWorksheets + "sheet1.xml", []byte(sheet)},
	})
}

func pptxBytes(t *testing.T, slideCount int) []byte {
	t.Helper()
	entries := []zipEntry{{"[Content_Types].xml", []byte(`<?xml version="1.0"?><Types/>`)}}
	for i := 1; i <= slideCount; i++ {
		body := fmt.Sprintf(`<?xml version="1.0"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld><p:spTree>
    <p:sp><p:txBody>
      <a:p><a:r><a:t>Title %d</a:t></a:r></a:p>
      <a:p><a:r><a:t>Body of slide %d</a:t></a:r></a:p>
    </p:txBody></p:sp>
  </p:spTree></p:cSld>
</p:sld>`, i, i)
		entries = append(entries, zipEntry{
			name: fmt.Sprintf("%sslide%d.xml", prefixSlides, i),
			body: []byte(body),
		})
	}
	return buildZip(t, entries)
}
