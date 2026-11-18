package logo

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func solid(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	return m
}

func encodePNG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func refusal(t *testing.T, raw []byte, want string) {
	t.Helper()
	_, err := Normalize(raw)
	e, ok := IsError(err)
	if !ok {
		t.Fatalf("want a refusal containing %q, got %v", want, err)
	}
	if !strings.Contains(e.Message, want) {
		t.Fatalf("refusal %q does not contain %q", e.Message, want)
	}
}

func decoded(t *testing.T, l Logo) image.Image {
	t.Helper()
	if !bytes.HasPrefix(l.PNG, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("stored logo is not a PNG")
	}
	m, err := png.Decode(bytes.NewReader(l.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if b := m.Bounds(); b.Dx() != l.Width || b.Dy() != l.Height {
		t.Fatalf("size %dx%d, reported %dx%d", b.Dx(), b.Dy(), l.Width, l.Height)
	}
	if l.ID != ID(l.PNG) || len(l.ID) != 32 {
		t.Fatalf("id %q is not the content id", l.ID)
	}
	return m
}

func TestNormalizeKeepsSmallImages(t *testing.T) {
	l, err := Normalize(encodePNG(t, solid(120, 40)))
	if err != nil {
		t.Fatal(err)
	}
	m := decoded(t, l)
	if l.Width != 120 || l.Height != 40 {
		t.Fatalf("got %dx%d, want 120x40", l.Width, l.Height)
	}
	if got := color.NRGBAModel.Convert(m.At(7, 9)).(color.NRGBA); got != (color.NRGBA{R: 7, G: 9, B: 200, A: 255}) {
		t.Fatalf("pixel changed: %v", got)
	}
}

func TestNormalizeScalesDownKeepingAspect(t *testing.T) {
	for _, c := range []struct{ w, h, ww, wh int }{
		{1024, 512, 256, 128},
		{300, 900, 85, 256},
		{256, 256, 256, 256},
		{257, 257, 256, 256},
	} {
		l, err := Normalize(encodePNG(t, solid(c.w, c.h)))
		if err != nil {
			t.Fatal(err)
		}
		decoded(t, l)
		if l.Width != c.ww || l.Height != c.wh {
			t.Errorf("%dx%d: got %dx%d, want %dx%d", c.w, c.h, l.Width, l.Height, c.ww, c.wh)
		}
	}
}

func TestNormalizeAcceptsJPEG(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, solid(64, 64), nil); err != nil {
		t.Fatal(err)
	}
	l, err := Normalize(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	decoded(t, l)
}

// A minimal lossless WebP (VP8L) of 1x1 cannot pass MinSide, so the test checks the codec is
// picked from the RIFF signature and the decoder runs: a WebP header with a bad body is "could
// not be read", not "must be a PNG".
func TestNormalizeDetectsWebP(t *testing.T) {
	raw := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8L"), make([]byte, 8)...)
	refusal(t, raw, "could not be read")
}

func TestNormalizeStripsTrailingBytesAndMetadata(t *testing.T) {
	src := encodePNG(t, solid(40, 40))
	// A text chunk before IEND and a script after it: neither may reach the stored file.
	text := []byte("tEXtComment\x00<script>alert(1)</script>")
	chunk := make([]byte, 4, 12+len(text))
	binary.BigEndian.PutUint32(chunk, uint32(len(text)-4))
	chunk = append(chunk, text...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(text))
	iend := bytes.LastIndex(src, []byte("IEND")) - 4
	raw := append(append(append([]byte{}, src[:iend]...), chunk...), src[iend:]...)
	raw = append(raw, []byte("<html><script>alert(2)</script>")...)
	l, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	decoded(t, l)
	if bytes.Contains(l.PNG, []byte("script")) || bytes.Contains(l.PNG, []byte("tEXt")) {
		t.Fatal("metadata or trailing bytes survived")
	}
}

func TestNormalizeIsDeterministic(t *testing.T) {
	raw := encodePNG(t, solid(500, 200))
	a, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || !bytes.Equal(a.PNG, b.PNG) {
		t.Fatal("the same upload gave two files")
	}
}

func TestNormalizeRefusals(t *testing.T) {
	refusal(t, nil, "empty")
	refusal(t, make([]byte, MaxUploadBytes+1), "larger than 512 KB")
	refusal(t, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "PNG, JPEG or WebP")
	refusal(t, []byte("GIF89a"), "PNG, JPEG or WebP")
	var g bytes.Buffer
	if err := gif.Encode(&g, solid(64, 64), nil); err != nil {
		t.Fatal(err)
	}
	refusal(t, g.Bytes(), "PNG, JPEG or WebP")
	refusal(t, encodePNG(t, solid(31, 200)), "at least 32 x 32")
	refusal(t, encodePNG(t, solid(200, 31)), "at least 32 x 32")
	refusal(t, []byte("\x89PNG\r\n\x1a\nnot really"), "could not be read")
	// A truncated PNG with a valid header: the size passes, the pixels do not.
	full := encodePNG(t, solid(64, 64))
	refusal(t, full[:len(full)/2], "could not be read")
}

// The size limits are read from the header, before any pixel is decoded: a header claiming
// 4097 x 32 or 2049 x 2049 is refused without allocating the image.
func TestNormalizeRefusesHugeHeadersBeforeDecoding(t *testing.T) {
	for _, c := range []struct{ w, h uint32 }{{MaxSide + 1, 32}, {32, MaxSide + 1}, {2049, 2049}} {
		refusal(t, pngHeaderOnly(c.w, c.h), "at most 4096 pixels on a side and 4 megapixels")
	}
}

func pngHeaderOnly(w, h uint32) []byte {
	ihdr := []byte("IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, 13)
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}
