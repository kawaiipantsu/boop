package documents

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

func utf16Bytes(s string, bigEndian, bom bool) []byte {
	units := utf16.Encode([]rune(s))
	var out []byte
	if bom {
		if bigEndian {
			out = append(out, 0xFE, 0xFF)
		} else {
			out = append(out, 0xFF, 0xFE)
		}
	}
	for _, u := range units {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}

func TestDecodeText(t *testing.T) {
	tests := []struct {
		name         string
		data         []byte
		max          int
		wantText     string
		wantEncoding Encoding
		wantBOM      bool
		wantTrunc    bool
		wantErr      error
	}{
		{
			name: "utf-8", data: []byte("hello wörld\n"),
			wantText: "hello wörld\n", wantEncoding: EncodingUTF8,
		},
		{
			name: "utf-8 with BOM", data: append([]byte{0xEF, 0xBB, 0xBF}, "hello"...),
			wantText: "hello", wantEncoding: EncodingUTF8, wantBOM: true,
		},
		{
			name: "utf-16le with BOM", data: utf16Bytes("naïve café", false, true),
			wantText: "naïve café", wantEncoding: EncodingUTF16LE, wantBOM: true,
		},
		{
			name: "utf-16be with BOM", data: utf16Bytes("naïve café", true, true),
			wantText: "naïve café", wantEncoding: EncodingUTF16BE, wantBOM: true,
		},
		{
			name: "utf-16 surrogate pair", data: utf16Bytes("emoji \U0001F600 here", false, true),
			wantText: "emoji \U0001F600 here", wantEncoding: EncodingUTF16LE, wantBOM: true,
		},
		{
			// 0xE9 is é in Latin-1 but an invalid UTF-8 lead byte.
			name: "latin-1", data: []byte{'c', 'a', 'f', 0xE9, '\n'},
			wantText: "café\n", wantEncoding: EncodingLatin1,
		},
		{
			name: "crlf normalised", data: []byte("one\r\ntwo\rthree\n"),
			wantText: "one\ntwo\nthree\n", wantEncoding: EncodingUTF8,
		},
		{
			name: "truncated at a line boundary", data: []byte("aaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\n"), max: 22,
			wantText: "aaaaaaaaaa\nbbbbbbbbbb\n", wantEncoding: EncodingUTF8, wantTrunc: true,
		},
		{
			name: "truncation never splits a rune", data: []byte(strings.Repeat("é", 20)), max: 9,
			wantText: strings.Repeat("é", 4), wantEncoding: EncodingUTF8, wantTrunc: true,
		},
		{
			name: "binary rejected", data: []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x03},
			wantErr: ErrUnsupportedEncoding,
		},
		{
			name: "utf-32 rejected by name", data: []byte{0xFF, 0xFE, 0x00, 0x00, 'a', 0, 0, 0},
			wantErr: ErrUnsupportedEncoding,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeText(tc.data, tc.max)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErr)
				}
				if len(err.Error()) < 30 {
					t.Errorf("error %q does not explain what went wrong", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeText: %v", err)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tc.wantText)
			}
			if got.Encoding != tc.wantEncoding {
				t.Errorf("Encoding = %q, want %q", got.Encoding, tc.wantEncoding)
			}
			if got.HadBOM != tc.wantBOM {
				t.Errorf("HadBOM = %v, want %v", got.HadBOM, tc.wantBOM)
			}
			if got.Truncated != tc.wantTrunc {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tc.wantTrunc)
			}
			if got.SourceBytes != len(tc.data) {
				t.Errorf("SourceBytes = %d, want %d", got.SourceBytes, len(tc.data))
			}
		})
	}
}

func TestDecodeTextUTF32BigEndianRejected(t *testing.T) {
	data := []byte{0x00, 0x00, 0xFE, 0xFF, 0, 0, 0, 'a'}
	if _, err := DecodeText(data, 0); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("error = %v, want ErrUnsupportedEncoding", err)
	}
}

func TestDecodeTextCountsLines(t *testing.T) {
	tests := map[string]int{
		"":             0,
		"one":          1,
		"one\n":        1,
		"one\ntwo":     2,
		"one\ntwo\n":   2,
		"one\n\nthree": 3,
	}
	for in, want := range tests {
		got, err := DecodeText([]byte(in), 0)
		if err != nil {
			t.Fatalf("DecodeText(%q): %v", in, err)
		}
		if got.Lines != want {
			t.Errorf("Lines for %q = %d, want %d", in, got.Lines, want)
		}
	}
}

func TestNormalizeNewlines(t *testing.T) {
	tests := map[string]string{
		"plain":              "plain",
		"a\r\nb":             "a\nb",
		"a\rb":               "a\nb",
		"a\r\n\r\nb":         "a\n\nb",
		"mixed\r\nand\rlf\n": "mixed\nand\nlf\n",
	}
	for in, want := range tests {
		if got := NormalizeNewlines(in); got != want {
			t.Errorf("NormalizeNewlines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncationNotice(t *testing.T) {
	got := TruncationNotice(100, 5000)
	for _, want := range []string{"truncated", "100", "5000"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q is missing %q", got, want)
		}
	}
}
