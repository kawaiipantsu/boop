package webclient

import (
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// cp1252High maps the Windows-1252 bytes 0x80–0x9F, the only region where it
// differs from ISO-8859-1. A page mislabelled as latin-1 that actually contains
// smart quotes is extremely common, so decoding this range properly is the
// difference between readable text and scattered replacement characters.
var cp1252High = [32]rune{
	0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178,
}

// metaCharsetRe finds a charset declaration inside an HTML meta tag, in either
// the HTML5 (<meta charset=...>) or legacy (http-equiv content=...) spelling.
var metaCharsetRe = regexp.MustCompile(`(?i)<meta[^>]+charset\s*=\s*["']?\s*([a-z0-9_:.+-]+)`)

// SupportedCharsets lists the encodings this package can decode. Boop pulls in
// no text-encoding dependency, so anything else is reported as unsupported
// rather than decoded wrongly: mojibake in a model's context is worse than an
// honest "cannot decode this".
func SupportedCharsets() []string {
	return []string{"utf-8", "us-ascii", "iso-8859-1", "windows-1252", "utf-16", "utf-16le", "utf-16be"}
}

// decodeBody converts a response body to UTF-8 text.
//
// declared is the charset from the Content-Type header (may be empty). For
// HTML, a meta charset declaration is consulted when the header is silent. A
// byte-order mark always wins, because it is evidence rather than a claim.
//
// It returns the decoded text, the charset actually used, and whether decoding
// succeeded. When ok is false the text is empty and the caller should say so
// rather than present garbage.
func decodeBody(body []byte, declared, contentType string) (text, charset string, ok bool) {
	if cs, rest, found := detectBOM(body); found {
		t, decoded := decodeCharset(rest, cs)
		return t, cs, decoded
	}
	cs := normalizeCharset(declared)
	if cs == "" && isHTMLType(contentType) {
		cs = normalizeCharset(sniffMetaCharset(body))
	}
	if cs == "" {
		// No declaration anywhere. Valid UTF-8 is overwhelmingly the right
		// guess today; otherwise fall back to the HTML5 default, which is
		// Windows-1252 and never fails to produce something.
		if utf8.Valid(body) {
			return string(body), "utf-8", true
		}
		return decodeSingleByte(body, true), "windows-1252", true
	}
	t, decoded := decodeCharset(body, cs)
	return t, cs, decoded
}

// decodeCharset decodes body using a normalized charset name.
func decodeCharset(body []byte, charset string) (string, bool) {
	switch charset {
	case "utf-8":
		if utf8.Valid(body) {
			return string(body), true
		}
		// Declared UTF-8 but not valid UTF-8: repair rather than refuse, so
		// one bad byte does not cost the whole page.
		return strings.ToValidUTF8(string(body), "�"), true
	case "us-ascii":
		return decodeSingleByte(body, false), true
	case "iso-8859-1":
		return decodeSingleByte(body, false), true
	case "windows-1252":
		return decodeSingleByte(body, true), true
	case "utf-16", "utf-16le":
		return decodeUTF16(body, false), true
	case "utf-16be":
		return decodeUTF16(body, true), true
	default:
		return "", false
	}
}

// detectBOM reports a byte-order mark and returns the body without it.
func detectBOM(b []byte) (charset string, rest []byte, found bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF:
		return "utf-8", b[3:], true
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		return "utf-16le", b[2:], true
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		return "utf-16be", b[2:], true
	}
	return "", b, false
}

// normalizeCharset canonicalises the many spellings of the encodings we know.
func normalizeCharset(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.Trim(n, `"'`)
	n = strings.ReplaceAll(n, "_", "-")
	switch n {
	case "":
		return ""
	case "utf8", "utf-8", "unicode-1-1-utf-8":
		return "utf-8"
	case "ascii", "us-ascii", "iso-ir-6", "ansi-x3.4-1968":
		return "us-ascii"
	case "latin1", "latin-1", "iso-8859-1", "iso8859-1", "iso-88591", "l1", "cp819", "iso-latin-1":
		return "iso-8859-1"
	case "windows-1252", "cp1252", "cp-1252", "win-1252", "x-cp1252":
		return "windows-1252"
	case "utf-16", "utf16":
		return "utf-16"
	case "utf-16le", "utf16le", "unicodefffe":
		return "utf-16le"
	case "utf-16be", "utf16be":
		return "utf-16be"
	}
	return n
}

// decodeSingleByte widens each byte to the matching rune. With cp1252 set, the
// 0x80–0x9F range uses the Windows-1252 table instead of C1 controls.
func decodeSingleByte(b []byte, cp1252 bool) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case cp1252 && c < 0xA0:
			sb.WriteRune(cp1252High[c-0x80])
		default:
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// decodeUTF16 decodes UTF-16 code units in the given byte order.
func decodeUTF16(b []byte, bigEndian bool) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
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

// sniffMetaCharset looks for a charset declaration in the first 4 KiB of an
// HTML document, which is where the spec requires it to be.
func sniffMetaCharset(b []byte) string {
	head := b
	if len(head) > 4096 {
		head = head[:4096]
	}
	if m := metaCharsetRe.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}

// isHTMLType reports whether a Content-Type looks like HTML or XHTML.
func isHTMLType(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}

// isTextualType reports whether a Content-Type is worth decoding to text at
// all. Images and archives are returned as bytes only.
func isTextualType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "" {
		return true // unlabelled: assume text and let decoding decide.
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	for _, s := range []string{"json", "xml", "javascript", "x-www-form-urlencoded", "yaml", "csv", "ecmascript"} {
		if strings.Contains(ct, s) {
			return true
		}
	}
	return false
}
