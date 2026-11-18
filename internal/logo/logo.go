// Package logo turns a merchant's uploaded logo into the one file the checkout serves: the image
// decoded from PNG, JPEG or WebP, scaled down to fit Max x Max and encoded again as PNG. Nothing
// of the upload but its pixels survives (no metadata, no trailing bytes, no second format), so
// the public file is always a plain PNG whatever was sent.
package logo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	// MaxUploadBytes is the largest upload accepted.
	MaxUploadBytes = 512 << 10
	// MinSide is the smallest width and height accepted.
	MinSide = 32
	// MaxSide is the largest width or height accepted, checked before the pixels are decoded.
	MaxSide = 4096
	// MaxPixels bounds width x height, also before decoding (4 MP is 16 MB of RGBA).
	MaxPixels = 4 << 20
	// Max is the largest width and height of the stored PNG; smaller images keep their size.
	Max = 256
	// MaxStoredBytes is the largest stored PNG (the logos table checks the same bound).
	MaxStoredBytes = 512 << 10
)

// Error is a problem with the upload itself, worded for the merchant.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func invalid(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }

// Logo is a normalized logo: its PNG and its content id.
type Logo struct {
	ID     string
	PNG    []byte
	Width  int
	Height int
}

// ID is the content id of a stored PNG: the first 128 bits of its SHA-256, lowercase hex.
func ID(pngBytes []byte) string {
	sum := sha256.Sum256(pngBytes)
	return hex.EncodeToString(sum[:16])
}

// Normalize checks an upload and re-encodes it. Every refusal is an *Error.
func Normalize(raw []byte) (Logo, error) {
	if len(raw) == 0 {
		return Logo{}, invalid("image is empty")
	}
	if len(raw) > MaxUploadBytes {
		return Logo{}, invalid("image is larger than %d KB", MaxUploadBytes>>10)
	}
	decodeConfig, decode, err := codec(raw)
	if err != nil {
		return Logo{}, err
	}
	cfg, err := decodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Logo{}, invalid("image could not be read")
	}
	if cfg.Width < MinSide || cfg.Height < MinSide {
		return Logo{}, invalid("image must be at least %d x %d pixels", MinSide, MinSide)
	}
	if cfg.Width > MaxSide || cfg.Height > MaxSide || cfg.Width*cfg.Height > MaxPixels {
		return Logo{}, invalid("image must be at most %d pixels on a side and %d megapixels in all", MaxSide, MaxPixels>>20)
	}
	src, err := decode(bytes.NewReader(raw))
	if err != nil {
		return Logo{}, invalid("image could not be read")
	}
	dst := scale(src)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return Logo{}, fmt.Errorf("logo: encode: %w", err)
	}
	if buf.Len() > MaxStoredBytes {
		return Logo{}, invalid("image is too detailed to store; try a simpler or smaller logo")
	}
	b := dst.Bounds()
	return Logo{ID: ID(buf.Bytes()), PNG: buf.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}

type configFunc func(r *bytes.Reader) (image.Config, error)
type decodeFunc func(r *bytes.Reader) (image.Image, error)

// codec picks the decoder from the file's signature, never from a name or a content type the
// client sent. SVG, GIF and anything else are refused.
func codec(raw []byte) (configFunc, decodeFunc, error) {
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		return func(r *bytes.Reader) (image.Config, error) { return png.DecodeConfig(r) },
			func(r *bytes.Reader) (image.Image, error) { return png.Decode(r) }, nil
	case bytes.HasPrefix(raw, []byte{0xff, 0xd8, 0xff}):
		return func(r *bytes.Reader) (image.Config, error) { return jpeg.DecodeConfig(r) },
			func(r *bytes.Reader) (image.Image, error) { return jpeg.Decode(r) }, nil
	case len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return func(r *bytes.Reader) (image.Config, error) { return webp.DecodeConfig(r) },
			func(r *bytes.Reader) (image.Image, error) { return webp.Decode(r) }, nil
	}
	return nil, nil, invalid("image must be a PNG, JPEG or WebP file")
}

// scale fits src inside Max x Max, keeping its aspect ratio; it never enlarges. The result is
// always a fresh NRGBA image, so the encoder sees only pixels.
func scale(src image.Image) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > Max || h > Max {
		if w >= h {
			h = max(1, (h*Max+w/2)/w)
			w = Max
		} else {
			w = max(1, (w*Max+h/2)/h)
			h = Max
		}
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	}
	return dst
}

// IsError reports whether err is a refusal of the upload.
func IsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
