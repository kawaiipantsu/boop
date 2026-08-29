package documents

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrUnsupportedEncoding reports text whose encoding Boop cannot decode.
//
// Boop refuses rather than guessing, because a wrong guess produces mojibake
// that looks like real content: the model would happily reason about garbage
// and the user would never know the file was misread.
var ErrUnsupportedEncoding = errors.New("documents: unsupported text encoding")

// Encoding is a character encoding Boop can decode.
type Encoding string

const (
	EncodingUTF8    Encoding = "utf-8"
	EncodingUTF16LE Encoding = "utf-16le"
	EncodingUTF16BE Encoding = "utf-16be"
	// EncodingLatin1 is ISO-8859-1, the fallback for byte-oriented files that
	// are not valid UTF-8 but contain no control bytes.
	EncodingLatin1 Encoding = "iso-8859-1"
	EncodingBinary Encoding = "binary"
)

// Byte-order marks Boop recognises.
var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
	bomUTF32LE = []byte{0xFF, 0xFE, 0x00, 0x00}
	bomUTF32BE = []byte{0x00, 0x00, 0xFE, 0xFF}
)

// TextResult is decoded, normalised text plus how it was obtained.
type TextResult struct {
	// Text is UTF-8 with LF line endings.
	Text string `json:"text"`
	// Encoding is the source encoding that was decoded.
	Encoding Encoding `json:"encoding"`
	// HadBOM reports that a byte-order mark was present and stripped.
	HadBOM bool `json:"had_bom,omitempty"`
	// Truncated reports that Text was cut at the configured cap.
	Truncated bool `json:"truncated,omitempty"`
	// SourceBytes is the length of the input before decoding.
	SourceBytes int `json:"source_bytes"`
	// Lines counts the lines kept in Text.
	Lines int `json:"lines"`
	// Replacements counts undecodable bytes replaced with U+FFFD. Non-zero
	// means the source was not cleanly one encoding.
	Replacements int `json:"replacements,omitempty"`
}

// DecodeText converts arbitrary text bytes into normalised UTF-8.
//
// It recognises a UTF-8 BOM, UTF-16 with either BOM, and ISO-8859-1; UTF-16
// without a BOM and UTF-32 are reported as unsupported rather than mangled,
// since without a BOM they are indistinguishable from binary. Line endings are
// normalised to LF and the result is capped at maxBytes.
func DecodeText(data []byte, maxBytes int) (TextResult, error) {
	res := TextResult{SourceBytes: len(data)}

	switch {
	case hasPrefix(data, bomUTF32LE), hasPrefix(data, bomUTF32BE):
		return res, fmt.Errorf("%w: UTF-32 (convert the file to UTF-8 first)", ErrUnsupportedEncoding)

	case hasPrefix(data, bomUTF8):
		res.Encoding, res.HadBOM = EncodingUTF8, true
		res.Text = string(data[len(bomUTF8):])

	case hasPrefix(data, bomUTF16LE):
		res.Encoding, res.HadBOM = EncodingUTF16LE, true
		res.Text = decodeUTF16(data[2:], false)

	case hasPrefix(data, bomUTF16BE):
		res.Encoding, res.HadBOM = EncodingUTF16BE, true
		res.Text = decodeUTF16(data[2:], true)

	case utf8.Valid(data):
		res.Encoding = EncodingUTF8
		res.Text = string(data)

	case looksLatin1(data):
		res.Encoding = EncodingLatin1
		res.Text = decodeLatin1(data)

	default:
		return res, fmt.Errorf("%w: bytes are not UTF-8, not UTF-16 with a BOM, and contain "+
			"control characters that rule out ISO-8859-1 (the file looks binary)", ErrUnsupportedEncoding)
	}

	res.Text, res.Replacements = sanitize(res.Text)
	res.Text = NormalizeNewlines(res.Text)
	res.Text, res.Truncated = capText(res.Text, maxBytes)
	res.Lines = countLines(res.Text)
	return res, nil
}

// NormalizeNewlines converts CRLF and lone CR to LF.
//
// Mixed line endings otherwise reach the model as stray \r characters, which
// waste tokens and break diff-style tool output that assumes LF.
func NormalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// capText truncates s to at most max bytes on a rune boundary, preferring to
// cut at the last newline so the model never sees a half-written line.
func capText(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl > max/2 {
		cut = nl + 1
	}
	return s[:cut], true
}

// TruncationNotice renders a marker appended to truncated extractions so the
// model knows the content is partial rather than complete.
func TruncationNotice(kept, total int) string {
	return fmt.Sprintf("\n\n[boop: truncated — showing %d of %d bytes; ask for a specific "+
		"range or section to see more]", kept, total)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

// decodeUTF16 converts UTF-16 code units to UTF-8, tolerating an odd trailing
// byte rather than failing the whole file.
func decodeUTF16(b []byte, bigEndian bool) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(units))
}

// looksLatin1 reports whether bytes are plausibly ISO-8859-1 text.
//
// Every byte is a valid Latin-1 character, so the only usable discriminator is
// the presence of C0/C1 control codes that no text file contains.
func looksLatin1(b []byte) bool {
	for _, c := range b {
		switch {
		case c == '\t', c == '\n', c == '\r', c == '\f', c == 0x1B:
			// Tabs, newlines, form feeds and ESC (ANSI colour in logs) are fine.
		case c < 0x20, c == 0x7F, c >= 0x80 && c <= 0x9F:
			return false
		}
	}
	return true
}

func decodeLatin1(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// sanitize replaces invalid UTF-8 sequences and NUL bytes, returning the count
// of replacements so callers can report a partially-decodable file.
func sanitize(s string) (string, int) {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s, 0
	}
	var sb strings.Builder
	sb.Grow(len(s))
	n := 0
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				sb.WriteRune('�')
				n++
				continue
			}
		}
		if r == 0 {
			n++
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String(), n
}
