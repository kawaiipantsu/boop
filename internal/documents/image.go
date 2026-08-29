package documents

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"sort"
	"strings"

	// Register the stdlib decoders so image.DecodeConfig can validate headers.
	// Only the config is decoded, never the pixels, so this costs a few bytes
	// of parsing rather than a full raster in memory.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// Image validation errors.
var (
	// ErrNotAnImage means the bytes do not parse as the image type claimed.
	ErrNotAnImage = errors.New("documents: not a valid image")
	// ErrImageTooLarge means the payload exceeds the configured byte cap.
	ErrImageTooLarge = errors.New("documents: image too large")
	// ErrImageTooManyPixels means the declared dimensions exceed the pixel cap.
	ErrImageTooManyPixels = errors.New("documents: image dimensions too large")
	// ErrUnsupportedImageFormat covers formats with no stdlib decoder.
	ErrUnsupportedImageFormat = errors.New("documents: unsupported image format")
)

// ImageInfo describes a validated image attachment.
type ImageInfo struct {
	// MIMEType is derived from the decoded bytes, not the filename.
	MIMEType string `json:"mime_type"`
	// Format is the stdlib decoder name ("png", "jpeg", "gif", "webp").
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int    `json:"bytes"`
}

// String renders the image for logs and CLI summaries.
func (i ImageInfo) String() string {
	return fmt.Sprintf("%s %dx%d, %s", i.Format, i.Width, i.Height, humanBytes(int64(i.Bytes)))
}

// InspectImage validates image bytes and reports their real type and size.
//
// Validation decodes only the header via image.DecodeConfig, which both proves
// the bytes are the format they claim and yields dimensions without allocating
// a pixel buffer — important, because a hostile 64000x64000 PNG is a few KB on
// disk and 16 GB decoded.
//
// maxBytes and maxPixels bound the payload. Note that these are Boop's caps;
// every provider additionally enforces its own, usually stricter, limits on
// attachment size and resolution (and several downscale silently), so passing
// this check does not guarantee the provider will accept the image.
func InspectImage(data []byte, maxBytes int, maxPixels int64) (ImageInfo, error) {
	if len(data) == 0 {
		return ImageInfo{}, fmt.Errorf("%w: empty file", ErrNotAnImage)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return ImageInfo{}, fmt.Errorf("%w: %s exceeds the %s limit",
			ErrImageTooLarge, humanBytes(int64(len(data))), humanBytes(int64(maxBytes)))
	}

	info := ImageInfo{Bytes: len(data)}

	// WebP has no stdlib decoder; its header is simple enough to validate and
	// measure directly, which is better than refusing a format §27 lists as a
	// support target.
	if w, h, ok := webpConfig(data); ok {
		info.Format, info.MIMEType, info.Width, info.Height = "webp", "image/webp", w, h
	} else {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return ImageInfo{}, fmt.Errorf("%w: header does not parse as png, jpeg, gif or webp: %v",
				ErrNotAnImage, err)
		}
		mimeType, ok := formatMIME[format]
		if !ok {
			return ImageInfo{}, fmt.Errorf("%w: %s has no attachment mapping", ErrUnsupportedImageFormat, format)
		}
		info.Format, info.MIMEType, info.Width, info.Height = format, mimeType, cfg.Width, cfg.Height
	}

	if info.Width <= 0 || info.Height <= 0 {
		return ImageInfo{}, fmt.Errorf("%w: header reports %dx%d", ErrNotAnImage, info.Width, info.Height)
	}
	if maxPixels > 0 && int64(info.Width)*int64(info.Height) > maxPixels {
		return ImageInfo{}, fmt.Errorf("%w: %dx%d is %d pixels, over the %d limit",
			ErrImageTooManyPixels, info.Width, info.Height, int64(info.Width)*int64(info.Height), maxPixels)
	}
	return info, nil
}

var formatMIME = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// webpConfig parses a RIFF/WEBP header, returning the canvas dimensions.
//
// It reports ok=false for anything that is not a WebP so the caller can fall
// through to the registered stdlib decoders.
func webpConfig(b []byte) (width, height int, ok bool) {
	if len(b) < 16 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ": // Lossy: keyframe header carries 14-bit dimensions.
		if len(b) < 30 || b[23] != 0x9D || b[24] != 0x01 || b[25] != 0x2A {
			return 0, 0, false
		}
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3FFF)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3FFF)
		return w, h, true
	case "VP8L": // Lossless: 14-bit width-1 and height-1 packed after a 0x2F signature.
		if len(b) < 25 || b[20] != 0x2F {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3FFF) + 1, int((bits>>14)&0x3FFF) + 1, true
	case "VP8X": // Extended: 24-bit canvas width-1 and height-1.
		if len(b) < 30 {
			return 0, 0, false
		}
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	}
	return 0, 0, false
}

// ImagePart validates image bytes and wraps them as a multimodal content part.
//
// The returned part is ready to attach to a provider.Message, but only to a
// model with provider.CapabilityVision — see RequireVision.
func ImagePart(filename string, data []byte, maxBytes int, maxPixels int64) (provider.ContentPart, ImageInfo, error) {
	info, err := InspectImage(data, maxBytes, maxPixels)
	if err != nil {
		return provider.ContentPart{}, ImageInfo{}, err
	}
	part := provider.ContentPart{
		Kind:     provider.PartImage,
		MIMEType: info.MIMEType,
		Data:     data,
		Filename: filename,
	}
	return part, info, nil
}

// SupportsImages reports whether a capability set permits image attachments.
//
// Callers should use this before attaching rather than after a provider
// rejection, so the user gets Boop's explanation instead of a vendor HTTP 400.
func SupportsImages(caps provider.Capabilities) bool {
	return caps.Has(provider.CapabilityVision)
}

// CapabilityError explains why an attachment cannot be sent to the selected
// model, and what to switch to.
//
// It satisfies §8's contract: name the missing capability, then list the
// configured models that have it. It unwraps to *provider.UnsupportedCapabilityError
// so router and UI code can match on the normalized type.
type CapabilityError struct {
	// Attachment is the filename or description that triggered the check.
	Attachment string
	// Reason names what the attachment needs in human terms.
	Reason string
	// Alternatives are configured models that do have the capability.
	Alternatives []provider.Model

	inner *provider.UnsupportedCapabilityError
}

// Unwrap exposes the normalized provider error for errors.As.
func (e *CapabilityError) Unwrap() error { return e.inner }

// Missing returns the capabilities the selected model lacks.
func (e *CapabilityError) Missing() []provider.Capability { return e.inner.Missing }

// Model returns the provider/model pair that was rejected.
func (e *CapabilityError) Model() (providerName, model string) {
	return e.inner.Provider, e.inner.Model
}

func (e *CapabilityError) Error() string {
	var sb strings.Builder
	if e.Attachment != "" {
		fmt.Fprintf(&sb, "cannot attach %s: ", e.Attachment)
	}
	fmt.Fprintf(&sb, "model %s/%s does not support %s",
		e.inner.Provider, e.inner.Model, joinCapabilities(e.inner.Missing))
	if e.Reason != "" {
		fmt.Fprintf(&sb, " (%s)", e.Reason)
	}
	switch len(e.Alternatives) {
	case 0:
		sb.WriteString(". No configured model advertises it — add a vision-capable model, " +
			"or extract the content as text instead")
	default:
		sb.WriteString(". Switch to one of: ")
		names := make([]string, 0, len(e.Alternatives))
		for _, m := range e.Alternatives {
			names = append(names, m.Provider+"/"+m.ID)
		}
		sb.WriteString(strings.Join(names, ", "))
	}
	return sb.String()
}

func joinCapabilities(caps []provider.Capability) string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return strings.Join(out, ", ")
}

// RequireVision checks a model before an image is attached to it.
//
// It returns nil when caps includes vision, and otherwise a *CapabilityError
// carrying the §8 explanation plus the vision-capable entries from available.
// Pass the full configured model list as available; filtering happens here so
// every caller produces the same suggestion set.
func RequireVision(attachment, providerName, model string, caps provider.Capabilities, available []provider.Model) error {
	return RequireCapabilities(attachment, providerName, model, caps, available,
		"image attachments need a vision-capable model", provider.CapabilityVision)
}

// RequireCapabilities is the general form of RequireVision.
func RequireCapabilities(attachment, providerName, model string, caps provider.Capabilities, available []provider.Model, reason string, want ...provider.Capability) error {
	missing := caps.Missing(want...)
	if len(missing) == 0 {
		return nil
	}
	return &CapabilityError{
		Attachment:   attachment,
		Reason:       reason,
		Alternatives: ModelsWith(available, want...),
		inner: &provider.UnsupportedCapabilityError{
			Provider: providerName,
			Model:    model,
			Missing:  missing,
		},
	}
}

// ModelsWith filters a model list down to those advertising every capability,
// sorted for stable presentation.
func ModelsWith(models []provider.Model, want ...provider.Capability) []provider.Model {
	var out []provider.Model
	for _, m := range models {
		if m.Capabilities.HasAll(want...) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// VisionModels lists the configured models that can accept images.
func VisionModels(models []provider.Model) []provider.Model {
	return ModelsWith(models, provider.CapabilityVision)
}
