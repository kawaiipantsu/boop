package documents

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Office container errors.
var (
	// ErrNotOffice means the bytes are not a readable OOXML zip container.
	ErrNotOffice = errors.New("documents: not an OOXML document")
	// ErrOfficeUnsupported covers the legacy binary formats (.doc, .xls, .ppt),
	// which are OLE compound files rather than zips and share no code with the
	// modern ones.
	ErrOfficeUnsupported = errors.New("documents: unsupported office format")
	// ErrArchiveTooLarge means the container exceeded a decompression bound.
	ErrArchiveTooLarge = errors.New("documents: archive exceeds decompression limits")
	// ErrUnsafeArchivePath means an entry name escaped the archive root.
	ErrUnsafeArchivePath = errors.New("documents: archive contains an unsafe path")
	// ErrOfficeEmpty means the document parsed but contained no text.
	ErrOfficeEmpty = errors.New("documents: document contains no extractable text")
)

// OfficeResult is the outcome of extracting an OOXML document.
type OfficeResult struct {
	// Text is the extracted content: paragraphs for Word, tab-separated rows
	// for Excel, per-slide blocks for PowerPoint.
	Text string `json:"text"`
	// Format is "docx", "xlsx" or "pptx".
	Format string `json:"format"`
	// Paragraphs, Tables, Sheets and Slides are populated where meaningful.
	Paragraphs int `json:"paragraphs,omitempty"`
	Tables     int `json:"tables,omitempty"`
	Sheets     int `json:"sheets,omitempty"`
	Slides     int `json:"slides,omitempty"`
	// Truncated reports that extraction stopped at a configured cap.
	Truncated bool `json:"truncated,omitempty"`
	// Images holds validated media when Options.ExtractEmbeddedImages is set.
	Images []EmbeddedImage `json:"images,omitempty"`
	// Diagnostics are notes about parts that were skipped.
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// OOXML part names Boop reads.
const (
	partWordDocument   = "word/document.xml"
	partSharedStrings  = "xl/sharedStrings.xml"
	partWorkbook       = "xl/workbook.xml"
	partWorkbookRels   = "xl/_rels/workbook.xml.rels"
	prefixWorksheets   = "xl/worksheets/"
	prefixSlides       = "ppt/slides/"
	prefixMediaWord    = "word/media/"
	prefixMediaSlides  = "ppt/media/"
	prefixMediaExcel   = "xl/media/"
	relationshipTarget = "Target"
)

// ExtractOffice reads text out of an OOXML container.
//
// DOCX, XLSX and PPTX are all zips of XML, which is why this needs no third
// party code: archive/zip plus encoding/xml is the whole dependency list.
// mimeType may be empty, in which case the format is inferred from the parts
// present in the container rather than from the filename.
func ExtractOffice(data []byte, mimeType string, opts Options) (*OfficeResult, error) {
	opts = opts.withDefaults()

	src, err := openZipSource(data, opts)
	if err != nil {
		return nil, err
	}

	format := officeFormat(mimeType, src)
	switch format {
	case "docx":
		return src.extractDocx(opts)
	case "xlsx":
		return src.extractXlsx(opts)
	case "pptx":
		return src.extractPptx(opts)
	default:
		return nil, fmt.Errorf("%w: the zip contains none of %s, %s or %s, so it is not a Word, "+
			"Excel or PowerPoint file", ErrNotOffice, partWordDocument, partWorkbook, prefixSlides+"slide1.xml")
	}
}

// LegacyOfficeError reports the pre-2007 binary formats with a usable
// suggestion, since those files are common and the failure is otherwise opaque.
func LegacyOfficeError(ext string) error {
	return fmt.Errorf("%w: %s is a pre-2007 OLE compound file, not an OOXML zip. "+
		"Save it as %sx and attach that", ErrOfficeUnsupported, ext, ext)
}

func officeFormat(mimeType string, src *zipSource) string {
	switch normalizeMIME(mimeType) {
	case mimeDOCX:
		if src.has(partWordDocument) {
			return "docx"
		}
	case mimeXLSX:
		if src.has(partWorkbook) {
			return "xlsx"
		}
	case mimePPTX:
		if len(src.withPrefix(prefixSlides)) > 0 {
			return "pptx"
		}
	}
	// The declared type was absent or contradicted by the container; believe
	// the container.
	switch {
	case src.has(partWordDocument):
		return "docx"
	case src.has(partWorkbook):
		return "xlsx"
	case len(src.withPrefix(prefixSlides)) > 0:
		return "pptx"
	}
	return ""
}

// --- bounded zip access ---------------------------------------------------

// zipSource is a size- and path-bounded view over an OOXML container.
//
// Every guard here exists because an attachment is untrusted input: a 40 KB
// zip can declare a 4 GB member, a container can hold a million entries, and
// an entry can be named "../../.ssh/authorized_keys". None of those may reach
// memory or the filesystem.
type zipSource struct {
	files map[string]*zip.File
	names []string
	// budget is the remaining decompressed bytes across the whole container.
	budget      int64
	diagnostics []string
}

func openZipSource(data []byte, opts Options) (*zipSource, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotOffice, err)
	}
	if opts.MaxArchiveEntries > 0 && len(zr.File) > opts.MaxArchiveEntries {
		return nil, fmt.Errorf("%w: %d entries exceeds the limit of %d",
			ErrArchiveTooLarge, len(zr.File), opts.MaxArchiveEntries)
	}

	// A disabled byte limit is modelled as an effectively infinite budget so
	// the per-read accounting needs no special case.
	budget := opts.MaxArchiveBytes
	if budget <= 0 {
		budget = math.MaxInt64
	}
	src := &zipSource{files: make(map[string]*zip.File, len(zr.File)), budget: budget}

	var declared int64
	for _, f := range zr.File {
		clean, err := safeArchivePath(f.Name)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(f.Name, "/") {
			continue // directory entry
		}
		declared += int64(f.UncompressedSize64)
		if opts.MaxArchiveBytes > 0 && declared > opts.MaxArchiveBytes {
			return nil, fmt.Errorf("%w: entries declare %s decompressed, over the %s limit",
				ErrArchiveTooLarge, humanBytes(declared), humanBytes(opts.MaxArchiveBytes))
		}
		src.files[clean] = f
		src.names = append(src.names, clean)
	}
	sort.Strings(src.names)
	return src, nil
}

// safeArchivePath rejects entry names that escape the extraction root.
//
// Boop never writes these entries to disk, but the check runs anyway: the day
// someone adds an "unpack attachment" tool, the guard must already be here
// rather than be remembered.
func safeArchivePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty entry name", ErrUnsafeArchivePath)
	}
	// Windows separators and drive letters are not valid in zip names but are
	// accepted by careless extractors, so normalise before judging.
	normalized := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, ":") {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafeArchivePath, name)
	}
	clean := path.Clean(normalized)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q escapes the archive root", ErrUnsafeArchivePath, name)
	}
	return clean, nil
}

func (z *zipSource) has(name string) bool { _, ok := z.files[name]; return ok }

func (z *zipSource) withPrefix(prefix string) []string {
	var out []string
	for _, n := range z.names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

func (z *zipSource) note(format string, args ...any) {
	if len(z.diagnostics) < 16 {
		z.diagnostics = append(z.diagnostics, fmt.Sprintf(format, args...))
	}
}

// read decompresses one entry against the shared budget.
//
// The declared uncompressed size is not trusted: the limit is enforced on the
// bytes that actually arrive, which is what stops a lying header.
func (z *zipSource) read(name string) ([]byte, error) {
	f, ok := z.files[name]
	if !ok {
		return nil, fmt.Errorf("%w: missing part %s", ErrNotOffice, name)
	}
	if z.budget <= 0 {
		return nil, fmt.Errorf("%w: decompression budget exhausted before %s", ErrArchiveTooLarge, name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: opening %s: %v", ErrNotOffice, name, err)
	}
	defer func() { _ = rc.Close() }()

	var buf bytes.Buffer
	// Read one byte past the budget so an over-long entry is detected rather
	// than silently truncated into malformed XML. The clamp matters: budget is
	// MaxInt64 when the limit is disabled, and +1 there would wrap negative
	// and make LimitReader read nothing at all.
	limit := z.budget
	if limit < math.MaxInt64 {
		limit++
	}
	n, err := io.Copy(&buf, io.LimitReader(rc, limit))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrNotOffice, name, err)
	}
	if n > z.budget {
		return nil, fmt.Errorf("%w: %s alone exceeds the remaining %s budget",
			ErrArchiveTooLarge, name, humanBytes(z.budget))
	}
	z.budget -= n
	return buf.Bytes(), nil
}

// --- DOCX -----------------------------------------------------------------

func (z *zipSource) extractDocx(opts Options) (*OfficeResult, error) {
	doc, err := z.read(partWordDocument)
	if err != nil {
		return nil, err
	}
	res := &OfficeResult{Format: "docx"}
	tb := &boundedText{max: opts.MaxTextBytes}

	if err := parseDocxBody(doc, tb, res); err != nil {
		return nil, fmt.Errorf("%w: %s is malformed: %v", ErrNotOffice, partWordDocument, err)
	}
	res.Text = strings.TrimSpace(tb.String())
	res.Truncated = tb.truncated
	z.finish(res, opts, prefixMediaWord)

	if res.Text == "" && len(res.Images) == 0 {
		return res, fmt.Errorf("%w: %s has no text runs — the document may be empty or contain "+
			"only images and drawings", ErrOfficeEmpty, partWordDocument)
	}
	return res, nil
}

// parseDocxBody streams WordprocessingML, preserving paragraph and table
// structure.
//
// Streaming rather than unmarshalling into a struct is deliberate: the schema
// is enormous, deeply nested and extended by every Word version, so matching a
// handful of element names is both smaller and more robust than modelling it.
func parseDocxBody(data []byte, tb *boundedText, res *OfficeResult) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	inText := 0
	tableDepth := 0

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if tb.full() {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t", "delText":
				inText++
			case "tab":
				tb.write("\t")
			case "br", "cr":
				tb.sep("\n")
			case "tbl":
				tableDepth++
				res.Tables++
				tb.sep("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t", "delText":
				if inText > 0 {
					inText--
				}
			case "p":
				res.Paragraphs++
				// Inside a table a paragraph is a line within a cell; breaking
				// there would scatter one row across many lines.
				if tableDepth > 0 {
					tb.sep(" ")
				} else {
					tb.sep("\n")
				}
			case "tc":
				tb.sep("\t")
			case "tr":
				tb.sep("\n")
			case "tbl":
				if tableDepth > 0 {
					tableDepth--
				}
				tb.sep("\n")
			}
		case xml.CharData:
			if inText > 0 {
				tb.write(string(t))
			}
		}
	}
}

// --- XLSX -----------------------------------------------------------------

func (z *zipSource) extractXlsx(opts Options) (*OfficeResult, error) {
	res := &OfficeResult{Format: "xlsx"}
	tb := &boundedText{max: opts.MaxTextBytes}

	shared := z.sharedStrings()
	names := z.sheetNames()

	sheets := z.withPrefix(prefixWorksheets)
	sort.Slice(sheets, func(i, j int) bool { return numericLess(sheets[i], sheets[j]) })

	for _, part := range sheets {
		if !strings.HasSuffix(part, ".xml") || tb.full() {
			continue
		}
		data, err := z.read(part)
		if err != nil {
			z.note("sheet %s could not be read: %v", part, err)
			continue
		}
		res.Sheets++
		title := names[path.Base(part)]
		if title == "" {
			title = strings.TrimSuffix(path.Base(part), ".xml")
		}
		tb.sep("\n\n")
		tb.write("# " + title)
		tb.sep("\n")
		if err := parseSheet(data, shared, tb); err != nil {
			z.note("sheet %s is malformed: %v", part, err)
		}
	}

	res.Text = strings.TrimSpace(tb.String())
	res.Truncated = tb.truncated
	z.finish(res, opts, prefixMediaExcel)

	if res.Sheets == 0 {
		return nil, fmt.Errorf("%w: no worksheets under %s", ErrNotOffice, prefixWorksheets)
	}
	if strings.TrimSpace(stripSheetHeadings(res.Text)) == "" && len(res.Images) == 0 {
		return res, fmt.Errorf("%w: the %d worksheet(s) contain no cell values", ErrOfficeEmpty, res.Sheets)
	}
	return res, nil
}

// sharedStrings reads the workbook string pool. Cells reference it by index,
// so without it a spreadsheet extracts as a grid of integers.
func (z *zipSource) sharedStrings() []string {
	if !z.has(partSharedStrings) {
		return nil
	}
	data, err := z.read(partSharedStrings)
	if err != nil {
		z.note("shared strings could not be read: %v", err)
		return nil
	}
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	var current strings.Builder
	inSI, inT := false, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI, current = true, strings.Builder{}
			case "t":
				if inSI {
					inT++
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				out = append(out, current.String())
				inSI = false
			case "t":
				if inT > 0 {
					inT--
				}
			}
		case xml.CharData:
			if inSI && inT > 0 {
				current.Write(t)
			}
		}
	}
	return out
}

// sheetNames maps worksheet part basenames to their user-visible tab names by
// joining workbook.xml to its relationships part.
func (z *zipSource) sheetNames() map[string]string {
	out := map[string]string{}
	if !z.has(partWorkbook) || !z.has(partWorkbookRels) {
		return out
	}
	rels := map[string]string{}
	if data, err := z.read(partWorkbookRels); err == nil {
		dec := xml.NewDecoder(bytes.NewReader(data))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Relationship" {
				var id, target string
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "Id":
						id = a.Value
					case relationshipTarget:
						target = a.Value
					}
				}
				if id != "" && target != "" {
					rels[id] = path.Base(target)
				}
			}
		}
	}
	data, err := z.read(partWorkbook)
	if err != nil {
		return out
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			var name, rid string
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "name":
					name = a.Value
				case "id":
					rid = a.Value
				}
			}
			if base, ok := rels[rid]; ok && name != "" {
				out[base] = name
			}
		}
	}
	return out
}

// parseSheet renders one worksheet as tab-separated rows.
func parseSheet(data []byte, shared []string, tb *boundedText) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var cellType, value string
	var inV, inIS int
	first := true

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if tb.full() {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				tb.sep("\n")
				first = true
			case "c":
				cellType, value = "", ""
				for _, a := range t.Attr {
					if a.Name.Local == "t" {
						cellType = a.Value
					}
				}
			case "v":
				inV++
			case "is":
				inIS++
			}
		case xml.CharData:
			if inV > 0 || inIS > 0 {
				value += string(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				if inV > 0 {
					inV--
				}
			case "is":
				if inIS > 0 {
					inIS--
				}
			case "c":
				if !first {
					tb.write("\t")
				}
				first = false
				tb.write(resolveCell(cellType, value, shared))
			}
		}
	}
}

// resolveCell turns a raw cell payload into display text, following the shared
// string table for t="s" cells.
func resolveCell(cellType, value string, shared []string) string {
	switch cellType {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || i < 0 || i >= len(shared) {
			return ""
		}
		return strings.TrimSpace(shared[i])
	case "b":
		if strings.TrimSpace(value) == "1" {
			return "TRUE"
		}
		return "FALSE"
	default:
		return strings.TrimSpace(value)
	}
}

func stripSheetHeadings(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "# ") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}

// --- PPTX -----------------------------------------------------------------

func (z *zipSource) extractPptx(opts Options) (*OfficeResult, error) {
	res := &OfficeResult{Format: "pptx"}
	tb := &boundedText{max: opts.MaxTextBytes}

	slides := z.withPrefix(prefixSlides)
	var parts []string
	for _, s := range slides {
		if strings.HasSuffix(s, ".xml") && strings.HasPrefix(path.Base(s), "slide") {
			parts = append(parts, s)
		}
	}
	// "slide10.xml" must sort after "slide9.xml"; plain string order does not.
	sort.Slice(parts, func(i, j int) bool { return numericLess(parts[i], parts[j]) })

	for i, part := range parts {
		if tb.full() {
			break
		}
		data, err := z.read(part)
		if err != nil {
			z.note("slide %s could not be read: %v", part, err)
			continue
		}
		res.Slides++
		tb.sep("\n\n")
		tb.write(fmt.Sprintf("# Slide %d", i+1))
		tb.sep("\n")
		if err := parseSlide(data, tb); err != nil {
			z.note("slide %s is malformed: %v", part, err)
		}
	}

	res.Text = strings.TrimSpace(tb.String())
	res.Truncated = tb.truncated
	z.finish(res, opts, prefixMediaSlides)

	if res.Slides == 0 {
		return nil, fmt.Errorf("%w: no slides under %s", ErrNotOffice, prefixSlides)
	}
	return res, nil
}

// parseSlide extracts DrawingML text runs, one line per paragraph.
func parseSlide(data []byte, tb *boundedText) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	inText := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if tb.full() {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText++
			case "br":
				tb.sep("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				if inText > 0 {
					inText--
				}
			case "p":
				tb.sep("\n")
			}
		case xml.CharData:
			if inText > 0 {
				tb.write(string(t))
			}
		}
	}
}

// --- shared -------------------------------------------------------------

// finish attaches diagnostics and, when requested, validated embedded media.
func (z *zipSource) finish(res *OfficeResult, opts Options, mediaPrefix string) {
	res.Text = NormalizeNewlines(res.Text)
	res.Text = collapseBlankLines(res.Text)
	if opts.ExtractEmbeddedImages {
		res.Images = z.media(mediaPrefix, opts)
	}
	res.Diagnostics = append(res.Diagnostics, z.diagnostics...)
}

// media returns the container's image parts, validated as real images.
//
// Validation matters here as much as anywhere: a media entry is just a file
// someone dropped into a zip, and its name says nothing about its contents.
func (z *zipSource) media(prefix string, opts Options) []EmbeddedImage {
	var out []EmbeddedImage
	for _, name := range z.withPrefix(prefix) {
		if len(out) >= opts.MaxEmbeddedImages {
			break
		}
		if f, ok := z.files[name]; ok && opts.MaxImageBytes > 0 &&
			int64(f.UncompressedSize64) > int64(opts.MaxImageBytes) {
			z.note("media %s is larger than the %s image cap and was skipped",
				name, humanBytes(int64(opts.MaxImageBytes)))
			continue
		}
		data, err := z.read(name)
		if err != nil {
			z.note("media %s could not be read: %v", name, err)
			continue
		}
		info, err := InspectImage(data, opts.MaxImageBytes, opts.MaxImagePixels)
		if err != nil {
			z.note("media %s is not a supported image: %v", name, err)
			continue
		}
		out = append(out, EmbeddedImage{Name: path.Base(name), Info: info, Data: data})
	}
	return out
}

// numericLess orders "slide2.xml" before "slide10.xml" by comparing embedded
// digit runs numerically instead of lexically.
func numericLess(a, b string) bool {
	na, sa := splitTrailingNumber(path.Base(a))
	nb, sb := splitTrailingNumber(path.Base(b))
	if sa == sb && na >= 0 && nb >= 0 {
		return na < nb
	}
	return a < b
}

func splitTrailingNumber(s string) (int, string) {
	s = strings.TrimSuffix(s, path.Ext(s))
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return -1, s
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return -1, s
	}
	return n, s[:i]
}

func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// boundedText accumulates extracted text under a byte cap.
//
// Structural boundaries (paragraph, cell, row, table, slide) request a
// *separator* rather than writing one. Separators are deferred until real text
// follows and the strongest pending one wins, which is what keeps a table cell
// from ending in " \t" and a document from ending in a run of blank lines.
type boundedText struct {
	sb        strings.Builder
	max       int
	truncated bool
	pending   string
}

// sepRank orders separators weakest to strongest.
func sepRank(s string) int {
	switch s {
	case " ":
		return 1
	case "\t":
		return 2
	case "\n":
		return 3
	case "\n\n":
		return 4
	default:
		return 0
	}
}

// sep requests a deferred separator, keeping the strongest one pending.
func (t *boundedText) sep(s string) {
	if t.sb.Len() == 0 {
		return
	}
	if sepRank(s) > sepRank(t.pending) {
		t.pending = s
	}
}

func (t *boundedText) full() bool {
	if t.max > 0 && t.sb.Len() >= t.max {
		t.truncated = true
		return true
	}
	return false
}

func (t *boundedText) write(s string) {
	if s == "" || t.full() {
		return
	}
	if t.pending != "" && t.sb.Len() > 0 {
		t.sb.WriteString(t.pending)
	}
	t.pending = ""
	if t.max > 0 && t.sb.Len()+len(s) > t.max {
		kept, _ := capText(s, t.max-t.sb.Len())
		t.sb.WriteString(kept)
		t.truncated = true
		return
	}
	t.sb.WriteString(s)
}

func (t *boundedText) String() string { return t.sb.String() }
