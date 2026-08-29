package documents

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExtractPDFText(t *testing.T) {
	data := textPDF("Hello Boop")

	res, err := ExtractPDF(data, Options{})
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if !strings.Contains(res.Text, "Hello Boop") {
		t.Fatalf("text = %q, want it to contain %q", res.Text, "Hello Boop")
	}
	if res.Pages != 1 || res.PagesWithText != 1 {
		t.Errorf("pages = %d, with text = %d, want 1 and 1", res.Pages, res.PagesWithText)
	}
	if res.Version != "1.4" {
		t.Errorf("version = %q, want 1.4", res.Version)
	}
	if res.Truncated {
		t.Error("Truncated set for a document well under the cap")
	}
}

func TestExtractPDFMultiPage(t *testing.T) {
	data := textPDF("Page one text", "Page two text", "Page three text")

	res, err := ExtractPDF(data, Options{})
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if res.Pages != 3 || res.PagesWithText != 3 {
		t.Fatalf("pages = %d, with text = %d, want 3 and 3", res.Pages, res.PagesWithText)
	}
	for _, want := range []string{"Page one text", "Page two text", "Page three text"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text is missing %q:\n%s", want, res.Text)
		}
	}
	if i, j := strings.Index(res.Text, "Page one"), strings.Index(res.Text, "Page three"); i > j {
		t.Error("pages came out in the wrong order")
	}
}

func TestExtractPDFResourceCaps(t *testing.T) {
	data := textPDF("Alpha content", "Bravo content", "Charlie content", "Delta content")

	t.Run("page cap", func(t *testing.T) {
		res, err := ExtractPDF(data, Options{MaxPDFPages: 2})
		if err != nil {
			t.Fatalf("ExtractPDF: %v", err)
		}
		if !res.Truncated {
			t.Error("Truncated not set despite the page cap")
		}
		if res.PagesProcessed != 2 {
			t.Errorf("processed %d pages, want 2", res.PagesProcessed)
		}
		if strings.Contains(res.Text, "Charlie") {
			t.Error("content past the page cap leaked into the result")
		}
		if res.Pages != 4 {
			t.Errorf("Pages = %d, want the real count 4 even when capped", res.Pages)
		}
	})

	t.Run("text cap", func(t *testing.T) {
		res, err := ExtractPDF(data, Options{MaxTextBytes: 20})
		if err != nil {
			t.Fatalf("ExtractPDF: %v", err)
		}
		if !res.Truncated {
			t.Error("Truncated not set despite the byte cap")
		}
		if len(res.Text) > 40 {
			t.Errorf("text is %d bytes, far past the 20 byte cap: %q", len(res.Text), res.Text)
		}
	})
}

func TestExtractPDFFailures(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr error
		wantMsg string
	}{
		{
			name:    "not a pdf",
			data:    []byte("just some text, definitely not a document"),
			wantErr: ErrNotPDF,
		},
		{
			name:    "scanned page with no text layer",
			data:    scannedPDF(),
			wantErr: ErrPDFNoTextLayer,
			wantMsg: "scan",
		},
		{
			name:    "encrypted",
			data:    encryptedPDF(),
			wantErr: ErrPDFEncrypted,
			wantMsg: "unprotected",
		},
		{
			name:    "truncated file with no EOF marker",
			data:    []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n"),
			wantErr: ErrPDFDamaged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractPDF(tc.data, Options{})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message %q does not mention %q", err.Error(), tc.wantMsg)
			}
			// Every failure must be actionable, never a bare category.
			if len(err.Error()) < 40 {
				t.Errorf("message %q is too terse to act on", err.Error())
			}
		})
	}
}

// TestExtractPDFScannedReportsImages pins the signal that distinguishes a scan
// from a genuinely blank page.
func TestExtractPDFScannedReportsImages(t *testing.T) {
	res, err := ExtractPDF(scannedPDF(), Options{})
	if !errors.Is(err, ErrPDFNoTextLayer) {
		t.Fatalf("error = %v, want ErrPDFNoTextLayer", err)
	}
	if res == nil {
		t.Fatal("result is nil; callers need the metadata even on failure")
	}
	if res.ImagesFound == 0 {
		t.Error("ImagesFound = 0, so the scan heuristic had nothing to work with")
	}
}

// TestExtractPDFCorruptDoesNotPanic covers the reader's habit of panicking on
// malformed input. A bad attachment must produce an error, never take the
// process down.
func TestExtractPDFCorruptDoesNotPanic(t *testing.T) {
	valid := textPDF("Hello Boop")

	corruptions := []struct {
		name string
		data []byte
	}{
		{"header only", []byte("%PDF-1.4\n%%EOF\n")},
		{"garbage after header", append([]byte("%PDF-1.4\n"), bytesOf(0xFF, 512)...)},
		{"startxref points into a stream", swapLast(valid, "startxref\n", "startxref\n120\n%%EOF\n")},
		{"xref table replaced with junk", []byte(strings.Replace(string(valid), "xref\n0 ", "xref\nZZ ", 1))},
		{"trailer removed", []byte(strings.Replace(string(valid), "trailer", "trailXX", 1))},
		{"nul bytes injected", injectNULs(valid)},
		// This one makes the reader panic rather than return an error: the
		// xref entry resolves to bytes that are not an object definition.
		{"xref entry points at non-object bytes", corruptXrefOffset(valid, 4, 30)},
	}

	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			// A panic escaping here fails the test by crashing it, which is
			// exactly the regression being guarded.
			res, err := ExtractPDF(tc.data, Options{})
			if err == nil {
				t.Fatalf("expected an error for corrupt input, got a result: %+v", res)
			}
			if strings.Contains(err.Error(), "panic") {
				t.Errorf("raw panic text leaked into the user-facing error: %v", err)
			}
		})
	}

	t.Run("truncation sweep", func(t *testing.T) {
		// Cutting a valid file at many offsets reaches parser states no
		// hand-written fixture would.
		for n := 1; n < len(valid); n += 7 {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("ExtractPDF panicked on a %d-byte prefix: %v", n, r)
					}
				}()
				_, _ = ExtractPDF(valid[:n], Options{})
			}()
		}
	})
}

// corruptXrefOffset rewrites one 20-byte cross-reference entry to point
// somewhere that holds no object definition.
func corruptXrefOffset(data []byte, objNum, offset int) []byte {
	out := append([]byte{}, data...)
	i := strings.LastIndex(string(out), "xref\n0 ")
	if i < 0 {
		return out
	}
	j := strings.Index(string(out[i:]), "\n0000000000 65535 f \n")
	if j < 0 {
		return out
	}
	start := i + j + 1 + objNum*20
	if start+10 > len(out) {
		return out
	}
	copy(out[start:start+10], []byte(fmt.Sprintf("%010d", offset)))
	return out
}

func TestGuardPanicConvertsPanicToError(t *testing.T) {
	err := guardPanic("doing something risky", func() error {
		panic("boom")
	})
	if err == nil {
		t.Fatal("guardPanic swallowed the panic and returned nil")
	}
	if !strings.Contains(err.Error(), "doing something risky") {
		t.Errorf("error %q does not say what was being attempted", err)
	}

	sentinel := errors.New("ordinary failure")
	if got := guardPanic("x", func() error { return sentinel }); !errors.Is(got, sentinel) {
		t.Errorf("guardPanic altered a normal error: %v", got)
	}
	if got := guardPanic("x", func() error { return nil }); got != nil {
		t.Errorf("guardPanic invented an error: %v", got)
	}
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func swapLast(data []byte, marker, replacement string) []byte {
	i := strings.LastIndex(string(data), marker)
	if i < 0 {
		return data
	}
	return append(append([]byte{}, data[:i]...), replacement...)
}

func injectNULs(data []byte) []byte {
	out := append([]byte{}, data...)
	for i := 20; i < len(out)-20; i += 11 {
		out[i] = 0
	}
	return out
}

func TestUnsupportedFilterName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"unknown filter LZWDecode", "LZWDecode"},
		{"unsupported filter /JPXDecode", "JPXDecode"},
		{"some other failure", ""},
	}
	for _, tc := range tests {
		if got := unsupportedFilterName(errors.New(tc.in)); got != tc.want {
			t.Errorf("unsupportedFilterName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
