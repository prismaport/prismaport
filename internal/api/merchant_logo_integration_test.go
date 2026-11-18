//go:build integration

package api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
)

func logoUpload(t *testing.T, w, h int, c color.NRGBA) map[string]any {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"image": base64.StdEncoding.EncodeToString(b.Bytes())}
}

func logoCount(t *testing.T, c *dashClient) int {
	t.Helper()
	var n int
	if err := c.app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM logos`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMerchantLogo(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	me := c.get("/v1/dashboard/me")["merchant"].(map[string]any)
	if me["logo_url"] != nil {
		t.Fatalf("a new merchant has a logo: %v", me)
	}
	name := me["name"].(string)

	ses := app.Session(app.TestKey, map[string]any{"amount": "5.00"}, "Idempotency-Key", "logo-1").JSON(t)["id"].(string)
	link := app.PaymentLink(app.TestKey, app.Link(app.TestKey), nil)
	if m := app.Do("GET", "/v1/checkout/sessions/"+ses, "", nil).JSON(t); m["merchant"] != nil {
		t.Fatalf("the checkout names a merchant without a logo: %v", m["merchant"])
	}

	// Without the CSRF token the upload is refused.
	c.csrf, c.header = "", http.Header{}
	if r := c.do("PUT", "/v1/dashboard/logo", logoUpload(t, 600, 300, color.NRGBA{R: 200, A: 255})); r.Status != http.StatusForbidden {
		t.Fatalf("upload without CSRF: %d %s", r.Status, r.Body)
	}
	c.get("/v1/dashboard/me")

	url := c.send("PUT", "/v1/dashboard/logo", logoUpload(t, 600, 300, color.NRGBA{R: 200, A: 255}))["logo_url"].(string)
	if !strings.HasPrefix(url, "/v1/checkout/logos/") || !strings.HasSuffix(url, ".png") {
		t.Fatalf("logo_url %q", url)
	}
	if got := c.get("/v1/dashboard/me")["merchant"].(map[string]any)["logo_url"]; got != url {
		t.Fatalf("/me logo_url %v, want %s", got, url)
	}
	want := map[string]any{"name": name, "logo_url": url}
	for _, path := range []string{"/v1/checkout/sessions/" + ses, "/v1/checkout/links/" + link} {
		m := app.Do("GET", path, "", nil).JSON(t)["merchant"].(map[string]any)
		if m["name"] != want["name"] || m["logo_url"] != want["logo_url"] {
			t.Fatalf("%s merchant %v, want %v", path, m, want)
		}
	}

	// A link that is no longer live still says whose it was.
	if r := app.Do("PATCH", "/v1/payment_links/"+link, app.TestKey, map[string]any{"active": false}); r.Status != http.StatusOK {
		t.Fatalf("deactivate link: %d %s", r.Status, r.Body)
	}
	if m := app.Do("GET", "/v1/checkout/links/"+link, "", nil).JSON(t); m["live"] != false ||
		m["merchant"].(map[string]any)["logo_url"] != url {
		t.Fatalf("inactive link: %v", m)
	}

	r := app.Do("GET", url, "", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("GET logo: %d %s", r.Status, r.Body)
	}
	for k, v := range map[string]string{"Content-Type": "image/png", "Cache-Control": "public, max-age=31536000, immutable",
		"Cross-Origin-Resource-Policy": "cross-origin", "Content-Security-Policy": api.LogoCSP, "X-Content-Type-Options": "nosniff",
		"Access-Control-Allow-Origin": "*"} {
		t.Run(k, func(t *testing.T) {
			if got := r.Header.Get(k); got != v {
				t.Errorf("logo %s = %q, want %q", k, got, v)
			}
		})
	}
	img, err := png.Decode(bytes.NewReader(r.Body))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 128 {
		t.Fatalf("stored logo is %dx%d, want 256x128", b.Dx(), b.Dy())
	}

	// The same image again is the same file; another one replaces it and the old file goes.
	if again := c.send("PUT", "/v1/dashboard/logo", logoUpload(t, 600, 300, color.NRGBA{R: 200, A: 255}))["logo_url"]; again != url {
		t.Fatalf("the same upload moved the logo: %v", again)
	}
	next := c.send("PUT", "/v1/dashboard/logo", logoUpload(t, 64, 64, color.NRGBA{G: 200, A: 255}))["logo_url"].(string)
	if next == url || logoCount(t, c) != 1 {
		t.Fatalf("replace: %s (was %s), %d logos stored", next, url, logoCount(t, c))
	}
	if r := app.Do("GET", url, "", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "logo_not_found" ||
		r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("replaced logo: %d %s %q", r.Status, r.Body, r.Header.Get("Cache-Control"))
	}

	// A second merchant with the same image shares the file; removing one keeps it for the other.
	secondMerchant(t, app, "other@example.com")
	o := login(t, app, "other@example.com")
	if shared := o.send("PUT", "/v1/dashboard/logo", logoUpload(t, 64, 64, color.NRGBA{G: 200, A: 255}))["logo_url"]; shared != next {
		t.Fatalf("same image, other merchant: %v, want %s", shared, next)
	}
	if m := c.send("DELETE", "/v1/dashboard/logo", nil); m["logo_url"] != nil || logoCount(t, c) != 1 {
		t.Fatalf("remove: %v, %d logos stored", m, logoCount(t, c))
	}
	if r := app.Do("GET", next, "", nil); r.Status != http.StatusOK {
		t.Fatalf("the other merchant's logo went with the first: %d", r.Status)
	}
	if m := app.Do("GET", "/v1/checkout/sessions/"+ses, "", nil).JSON(t); m["merchant"] != nil {
		t.Fatalf("a removed logo still brands the checkout: %v", m["merchant"])
	}
	if m := c.send("DELETE", "/v1/dashboard/logo", nil); m["logo_url"] != nil {
		t.Fatalf("removing twice: %v", m)
	}
	o.send("DELETE", "/v1/dashboard/logo", nil)
	if n := logoCount(t, c); n != 0 {
		t.Fatalf("%d logos left with no merchant", n)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "merchant.logo_set", "merchant.logo_removed") {
		t.Fatal("logo changes are not audited")
	}
}

func TestMerchantLogoRefusals(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	svg := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	for _, tc := range []struct {
		body any
		want string
	}{
		{map[string]any{}, "image is required"},
		{map[string]any{"image": "not base64!"}, "standard base64"},
		{map[string]any{"image": svg}, "PNG, JPEG or WebP"},
		{logoUpload(t, 16, 16, color.NRGBA{A: 255}), "at least 32 x 32"},
		{map[string]any{"image": "x", "url": "https://evil.example/logo.png"}, "url"},
		{`{"image": "` + strings.Repeat("A", 800<<10) + `"}`, "request body exceeds"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			r := c.do("PUT", "/v1/dashboard/logo", tc.body)
			if r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" || !strings.Contains(string(r.Body), tc.want) {
				t.Errorf("want 400 %q, got %d %s", tc.want, r.Status, r.Body)
			}
		})
	}
	if r := c.do("PUT", "/v1/dashboard/logo", "image=x", "Content-Type", "application/x-www-form-urlencoded"); r.Status != http.StatusUnsupportedMediaType {
		t.Errorf("form body: %d %s", r.Status, r.Body)
	}
	for _, path := range []string{"/v1/checkout/logos/nothing.png", "/v1/checkout/logos/" + strings.Repeat("0", 32) + ".png",
		"/v1/checkout/logos/" + strings.Repeat("A", 32) + ".png", "/v1/checkout/logos/" + strings.Repeat("0", 32)} {
		if r := app.Do("GET", path, "", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "logo_not_found" {
			t.Errorf("GET %s: %d %s", path, r.Status, r.Body)
		}
	}
	if r := newClient(t, app).do("PUT", "/v1/dashboard/logo", logoUpload(t, 64, 64, color.NRGBA{A: 255})); r.Status != http.StatusUnauthorized {
		t.Errorf("signed out: %d %s", r.Status, r.Body)
	}
}

// A 4K image (3840 x 2160, 8.3 MP) is refused whatever its file size: a flat one fits in 512 KB
// and is refused on its header before any pixel is decoded, a photo is refused on its size.
func TestMerchantLogo4K(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	const w, h = 3840, 2160
	flat := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 3; i < len(flat.Pix); i += 4 {
		flat.Pix[i] = 255
	}
	noisy := image.NewNRGBA(image.Rect(0, 0, w, h))
	_, _ = rand.Read(noisy.Pix)
	encode := func(m image.Image, jpg bool) []byte {
		t.Helper()
		var b bytes.Buffer
		var err error
		if jpg {
			err = jpeg.Encode(&b, m, &jpeg.Options{Quality: 90})
		} else {
			err = png.Encode(&b, m)
		}
		if err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	// The header of an 8K PNG and nothing else: the pixels it claims are never allocated.
	ihdr := binary.BigEndian.AppendUint32([]byte("IHDR"), 7680)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 4320)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	bomb := binary.BigEndian.AppendUint32([]byte("\x89PNG\r\n\x1a\n"), 13)
	bomb = binary.BigEndian.AppendUint32(append(bomb, ihdr...), crc32.ChecksumIEEE(ihdr))

	for _, tc := range []struct {
		name  string
		raw   []byte
		small bool
		want  string
	}{
		{"flat 4K PNG", encode(flat, false), true, "at most 4096 pixels on a side and 4 megapixels"},
		{"flat 4K JPEG", encode(flat, true), true, "at most 4096 pixels on a side and 4 megapixels"},
		{"8K PNG header", bomb, true, "at most 4096 pixels on a side and 4 megapixels"},
		{"4K photo JPEG", encode(noisy, true), false, "request body exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if small := len(tc.raw) <= 512<<10; small != tc.small {
				t.Fatalf("%s: %d bytes, the case expects it under 512 KB: %v", tc.name, len(tc.raw), tc.small)
			}
			r := c.do("PUT", "/v1/dashboard/logo", map[string]any{"image": base64.StdEncoding.EncodeToString(tc.raw)})
			if r.Status != http.StatusBadRequest || !strings.Contains(string(r.Body), tc.want) {
				t.Errorf("%s (%d KB): want 400 %q, got %d %s", tc.name, len(tc.raw)>>10, tc.want, r.Status, r.Body)
			}
		})
	}
	if n := logoCount(t, c); n != 0 {
		t.Fatalf("%d logos stored", n)
	}
	if me := c.get("/v1/dashboard/me")["merchant"].(map[string]any); me["logo_url"] != nil {
		t.Fatalf("a refused upload set a logo: %v", me)
	}
}
