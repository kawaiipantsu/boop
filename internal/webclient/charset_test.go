package webclient

import "testing"

func TestNormalizeCharset(t *testing.T) {
	tests := []struct{ in, want string }{
		{"UTF-8", "utf-8"},
		{"utf8", "utf-8"},
		{" \"utf-8\" ", "utf-8"},
		{"ISO-8859-1", "iso-8859-1"},
		{"latin1", "iso-8859-1"},
		{"Windows-1252", "windows-1252"},
		{"cp1252", "windows-1252"},
		{"US-ASCII", "us-ascii"},
		{"utf_16le", "utf-16le"},
		{"", ""},
		{"shift_jis", "shift-jis"},
	}
	for _, tc := range tests {
		if got := normalizeCharset(tc.in); got != tc.want {
			t.Errorf("normalizeCharset(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeBody(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		declared    string
		contentType string
		wantText    string
		wantCharset string
		wantOK      bool
	}{
		{"plain utf-8", []byte("hello"), "utf-8", "text/plain", "hello", "utf-8", true},
		{"undeclared valid utf-8", []byte("héllo"), "", "text/plain", "héllo", "utf-8", true},
		{"undeclared invalid utf-8 falls back", []byte{0x93, 'x'}, "", "text/plain", "“x", "windows-1252", true},
		{"declared utf-8 but invalid is repaired", []byte{'a', 0xFF, 'b'}, "utf-8", "text/plain", "a�b", "utf-8", true},
		{"utf-16le bom", []byte{0xFF, 0xFE, 'h', 0, 'i', 0}, "", "text/plain", "hi", "utf-16le", true},
		{"utf-16be bom", []byte{0xFE, 0xFF, 0, 'h', 0, 'i'}, "", "text/plain", "hi", "utf-16be", true},
		{"utf-16be declared", []byte{0, 'h', 0, 'i'}, "utf-16be", "text/plain", "hi", "utf-16be", true},
		{"odd length utf-16 tolerated", []byte{'h', 0, 'i'}, "utf-16le", "text/plain", "h", "utf-16le", true},
		{"meta charset in html", []byte(`<meta charset="windows-1252">` + "\x93q\x94"), "", "text/html", `<meta charset="windows-1252">“q”`, "windows-1252", true},
		{"header beats meta", append([]byte(`<meta charset="utf-8">`), 0xE9), "iso-8859-1", "text/html", `<meta charset="utf-8">é`, "iso-8859-1", true},
		{"unsupported charset", []byte{0x82, 0xA0}, "euc-jp", "text/plain", "", "euc-jp", false},
		{"empty body", nil, "", "text/plain", "", "utf-8", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, charset, ok := decodeBody(tc.body, tc.declared, tc.contentType)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if charset != tc.wantCharset {
				t.Fatalf("charset = %q, want %q", charset, tc.wantCharset)
			}
			if text != tc.wantText {
				t.Fatalf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestIsTextualType(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"text/html", true},
		{"text/plain", true},
		{"application/json", true},
		{"application/xhtml+xml", true},
		{"application/javascript", true},
		{"image/png", false},
		{"application/pdf", false},
		{"application/octet-stream", false},
		{"video/mp4", false},
	}
	for _, tc := range tests {
		if got := isTextualType(tc.in); got != tc.want {
			t.Errorf("isTextualType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsHTMLType(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"text/html", true},
		{"TEXT/HTML", true},
		{"application/xhtml+xml", true},
		{"text/plain", false},
		{"", false},
	} {
		if got := isHTMLType(tc.in); got != tc.want {
			t.Errorf("isHTMLType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSupportedCharsetsAreDecodable(t *testing.T) {
	for _, cs := range SupportedCharsets() {
		if _, ok := decodeCharset([]byte("a"), cs); !ok {
			t.Errorf("SupportedCharsets lists %q but decodeCharset refuses it", cs)
		}
	}
}
