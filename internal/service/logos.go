package service

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/prismaport/prismaport/internal/logo"
	"github.com/prismaport/prismaport/internal/store"
)

// CodeLogoNotFound answers a logo path that names no stored logo.
const CodeLogoNotFound = "logo_not_found"

// logoFile is the last segment of a public logo path: the content id and ".png".
var logoFile = regexp.MustCompile(`^([0-9a-f]{32})\.png$`)

// LogoPath is where the checkout API serves a logo. It is relative: a page served apart from the
// API prefixes the API's origin, as it does for its fetches.
func LogoPath(id string) string { return "/v1/checkout/logos/" + id + ".png" }

func logoURL(id *string) *string {
	if id == nil {
		return nil
	}
	p := LogoPath(*id)
	return &p
}

// LogoURL is the merchant's logo path, or nil when it has none.
func (u DashUser) LogoURL() *string { return logoURL(u.LogoID) }

// PublicMerchant brands a checkout. It exists only once the merchant uploaded a logo: until then
// the checkout names no merchant, as before checkout branding.
type PublicMerchant struct {
	Name    string `json:"name"`
	LogoURL string `json:"logo_url"`
}

// PublicMerchant returns the merchant's branding for its checkout pages, nil without a logo.
func (s *Service) PublicMerchant(ctx context.Context, merchantID string) (*PublicMerchant, error) {
	m, err := s.store.GetMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	if m.LogoID == nil {
		return nil, nil
	}
	return &PublicMerchant{Name: strings.TrimSpace(m.Name), LogoURL: LogoPath(*m.LogoID)}, nil
}

// Logo returns the stored PNG a public logo path names.
func (s *Service) Logo(ctx context.Context, file string) ([]byte, error) {
	m := logoFile.FindStringSubmatch(file)
	if m == nil {
		return nil, logoNotFound()
	}
	b, err := s.store.GetLogo(ctx, m[1])
	if errors.Is(err, store.ErrNotFound) {
		return nil, logoNotFound()
	}
	return b, err
}

func logoNotFound() *Error {
	return newErr(KindNotFound, CodeLogoNotFound, "no such logo")
}

// DashSetLogo checks and re-encodes an upload (logo.Normalize) and makes it the merchant's
// checkout logo. It answers the new logo's path.
func (s *Service) DashSetLogo(ctx context.Context, u DashUser, raw []byte) (string, error) {
	l, err := logo.Normalize(raw)
	if e, ok := logo.IsError(err); ok {
		return "", &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Message: e.Message, Param: "image"}
	}
	if err != nil {
		return "", err
	}
	if err := s.store.SetMerchantLogo(ctx, u.MerchantID, l.ID, l.PNG, u.actor()); err != nil {
		return "", err
	}
	return LogoPath(l.ID), nil
}

// DashRemoveLogo clears the merchant's logo; removing a logo that is not there is not an error.
func (s *Service) DashRemoveLogo(ctx context.Context, u DashUser) error {
	_, err := s.store.RemoveMerchantLogo(ctx, u.MerchantID, u.actor())
	return err
}
