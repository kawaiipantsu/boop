package documents

import (
	"errors"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestInspectImage(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		maxBytes   int
		maxPixels  int64
		wantFormat string
		wantMIME   string
		wantW      int
		wantH      int
		wantErr    error
	}{
		{name: "png", data: pngBytes(t, 12, 7), wantFormat: "png", wantMIME: "image/png", wantW: 12, wantH: 7},
		{name: "jpeg", data: jpegBytes(t, 20, 10), wantFormat: "jpeg", wantMIME: "image/jpeg", wantW: 20, wantH: 10},
		{name: "gif", data: gifBytes(t, 5, 9), wantFormat: "gif", wantMIME: "image/gif", wantW: 5, wantH: 9},
		{name: "webp lossless", data: webpLosslessBytes(320, 240), wantFormat: "webp", wantMIME: "image/webp", wantW: 320, wantH: 240},
		{name: "empty", data: nil, wantErr: ErrNotAnImage},
		{name: "text pretending to be an image", data: []byte("this is not an image at all"), wantErr: ErrNotAnImage},
		{
			// A truncated PNG has the right magic but no usable header.
			name: "truncated png", data: pngBytes(t, 8, 8)[:12], wantErr: ErrNotAnImage,
		},
		{name: "over the byte cap", data: pngBytes(t, 40, 40), maxBytes: 32, wantErr: ErrImageTooLarge},
		{name: "over the pixel cap", data: pngBytes(t, 40, 40), maxPixels: 100, wantErr: ErrImageTooManyPixels},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := InspectImage(tc.data, tc.maxBytes, tc.maxPixels)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("InspectImage: %v", err)
			}
			if got.Format != tc.wantFormat || got.MIMEType != tc.wantMIME {
				t.Errorf("format/mime = %s/%s, want %s/%s", got.Format, got.MIMEType, tc.wantFormat, tc.wantMIME)
			}
			if got.Width != tc.wantW || got.Height != tc.wantH {
				t.Errorf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, tc.wantW, tc.wantH)
			}
			if got.Bytes != len(tc.data) {
				t.Errorf("Bytes = %d, want %d", got.Bytes, len(tc.data))
			}
			if got.String() == "" {
				t.Error("String() is empty")
			}
		})
	}
}

// TestInspectImageIgnoresClaimedType proves validation is by content: a GIF
// handed to the inspector is reported as a GIF regardless of what anyone said.
func TestInspectImageIgnoresClaimedType(t *testing.T) {
	got, err := InspectImage(gifBytes(t, 3, 4), 0, 0)
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if got.MIMEType != "image/gif" {
		t.Errorf("MIMEType = %q, want image/gif", got.MIMEType)
	}
}

func TestImagePart(t *testing.T) {
	data := pngBytes(t, 6, 6)
	part, info, err := ImagePart("shot.png", data, 0, 0)
	if err != nil {
		t.Fatalf("ImagePart: %v", err)
	}
	if part.Kind != provider.PartImage {
		t.Errorf("Kind = %q, want %q", part.Kind, provider.PartImage)
	}
	if part.MIMEType != "image/png" || part.Filename != "shot.png" {
		t.Errorf("part = %+v, want image/png named shot.png", part)
	}
	if len(part.Data) != len(data) {
		t.Errorf("Data is %d bytes, want the original %d", len(part.Data), len(data))
	}
	if info.Width != 6 {
		t.Errorf("info.Width = %d, want 6", info.Width)
	}

	if _, _, err := ImagePart("bad.png", []byte("nope"), 0, 0); !errors.Is(err, ErrNotAnImage) {
		t.Errorf("error = %v, want ErrNotAnImage", err)
	}
}

func TestSupportsImages(t *testing.T) {
	if !SupportsImages(provider.Capabilities{provider.CapabilityVision}) {
		t.Error("a vision capability set was reported as unsupported")
	}
	if SupportsImages(provider.Capabilities{provider.CapabilityTools}) {
		t.Error("a tools-only capability set was reported as vision-capable")
	}
}

func TestRequireVision(t *testing.T) {
	available := []provider.Model{
		{ID: "qwen2.5:7b", Provider: "ollama", Capabilities: provider.Capabilities{provider.CapabilityTools}},
		{ID: "llava:13b", Provider: "ollama", Capabilities: provider.Capabilities{provider.CapabilityVision}},
		{ID: "gpt-4o", Provider: "openai", Capabilities: provider.Capabilities{provider.CapabilityVision, provider.CapabilityTools}},
	}

	t.Run("capable model passes", func(t *testing.T) {
		err := RequireVision("a.png", "openai", "gpt-4o",
			provider.Capabilities{provider.CapabilityVision}, available)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("incapable model explains and suggests", func(t *testing.T) {
		err := RequireVision("diagram.png", "ollama", "qwen2.5:7b",
			provider.Capabilities{provider.CapabilityTools}, available)
		if err == nil {
			t.Fatal("expected an error")
		}

		var capErr *CapabilityError
		if !errors.As(err, &capErr) {
			t.Fatalf("error is %T, want *CapabilityError", err)
		}
		// §8 requires unwrapping to the normalized provider error so routing
		// and UI code can match on one type.
		var provErr *provider.UnsupportedCapabilityError
		if !errors.As(err, &provErr) {
			t.Fatalf("error does not unwrap to *provider.UnsupportedCapabilityError")
		}
		if len(provErr.Missing) != 1 || provErr.Missing[0] != provider.CapabilityVision {
			t.Errorf("Missing = %v, want [vision]", provErr.Missing)
		}

		msg := err.Error()
		for _, want := range []string{"diagram.png", "qwen2.5:7b", "vision", "ollama/llava:13b", "openai/gpt-4o"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message is missing %q:\n%s", want, msg)
			}
		}
		if strings.Contains(msg, "ollama/qwen2.5:7b,") {
			t.Errorf("the incapable model was suggested as an alternative:\n%s", msg)
		}
		if gotP, gotM := capErr.Model(); gotP != "ollama" || gotM != "qwen2.5:7b" {
			t.Errorf("Model() = %s/%s, want ollama/qwen2.5:7b", gotP, gotM)
		}
	})

	t.Run("no alternatives says so", func(t *testing.T) {
		err := RequireVision("a.png", "ollama", "qwen2.5:7b", nil, available[:1])
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "No configured model") {
			t.Errorf("message does not admit there is no alternative:\n%s", err)
		}
	})
}

func TestModelsWith(t *testing.T) {
	models := []provider.Model{
		{ID: "z", Provider: "openai", Capabilities: provider.Capabilities{provider.CapabilityVision}},
		{ID: "a", Provider: "openai", Capabilities: provider.Capabilities{provider.CapabilityVision, provider.CapabilityTools}},
		{ID: "b", Provider: "anthropic", Capabilities: provider.Capabilities{provider.CapabilityVision}},
		{ID: "c", Provider: "ollama", Capabilities: nil},
	}

	got := VisionModels(models)
	want := []string{"anthropic/b", "openai/a", "openai/z"}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d", len(got), len(want))
	}
	for i, m := range got {
		if m.Provider+"/"+m.ID != want[i] {
			t.Errorf("model %d = %s/%s, want %s", i, m.Provider, m.ID, want[i])
		}
	}

	both := ModelsWith(models, provider.CapabilityVision, provider.CapabilityTools)
	if len(both) != 1 || both[0].ID != "a" {
		t.Errorf("ModelsWith(vision, tools) = %+v, want just openai/a", both)
	}
}

func TestWebPConfig(t *testing.T) {
	if _, _, ok := webpConfig([]byte("not a riff container at all")); ok {
		t.Error("webpConfig accepted non-WebP bytes")
	}
	w, h, ok := webpConfig(webpLosslessBytes(1024, 768))
	if !ok || w != 1024 || h != 768 {
		t.Errorf("webpConfig = %d x %d (ok=%v), want 1024x768", w, h, ok)
	}
}
