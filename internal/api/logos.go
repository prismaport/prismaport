package api

import (
	"encoding/base64"
	"net/http"

	"github.com/prismaport/prismaport/internal/logo"
	"github.com/prismaport/prismaport/internal/service"
)

// LogoCSP is sent with every logo file, in case one is opened as a page: it may run nothing.
const LogoCSP = "default-src 'none'; sandbox"

// maxLogoBodyBytes fits {"image": "<base64 of logo.MaxUploadBytes>"}.
const maxLogoBodyBytes = (logo.MaxUploadBytes+2)/3*4 + 1024

// mountLogoFile adds the public logo file the checkout pages show.
func (s *Server) mountLogoFile() {
	// Public and content-addressed (the path is a hash of the bytes, never the merchant), so it is
	// cached for good and may be shown by a page on any origin. A PNG, not JSON, so the contract
	// leaves it out (notInContract in openapi_test.go).
	s.public("GET /v1/checkout/logos/{file}", "", func(w http.ResponseWriter, r *http.Request) error {
		b, err := s.svc.Logo(r.Context(), r.PathValue("file"))
		if err != nil {
			return err
		}
		h := w.Header()
		h.Set("Content-Type", "image/png")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("Content-Security-Policy", LogoCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return nil
	})
}

// mountDashboardLogo adds the dashboard routes that set and remove the merchant's logo. The logo
// belongs to the merchant, not to a mode, so they take no mode.
func (s *Server) mountDashboardLogo() {
	s.dash("PUT", "/logo", func(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
		body, err := readBodyUpTo(r, maxLogoBodyBytes)
		if err != nil {
			return err
		}
		m, err := fields(body, "image")
		if err != nil {
			return err
		}
		enc, err := decodeField[string](m, "image", "a base64 string")
		if err != nil {
			return err
		}
		if enc == nil {
			return badParam("image", "image is required")
		}
		raw, err := base64.StdEncoding.DecodeString(*enc)
		if err != nil {
			return badParam("image", "image must be the file's bytes in standard base64")
		}
		url, err := s.svc.DashSetLogo(r.Context(), ds.User, raw)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{"logo_url": url})
	})
	s.dash("DELETE", "/logo", func(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
		if err := s.svc.DashRemoveLogo(r.Context(), ds.User); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{"logo_url": nil})
	})
}
