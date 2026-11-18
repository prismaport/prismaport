package api

import (
	"net/http"

	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/view"
)

// CodeRateLimited is the error code returned when a public request exceeds its rate limit.
const CodeRateLimited = service.CodeRateLimited

// MountCheckoutLinks adds the public payment-link endpoints: a link's public view, opening a
// session from it (rate limited, see ConfigurePublic), and the network registry. Like the
// checkout endpoints they take no API key.
func (s *Server) MountCheckoutLinks(c *checkout.Checkout) {
	s.public("GET /v1/checkout/networks", "", func(w http.ResponseWriter, r *http.Request) error {
		pauses, err := s.svc.ChainPauses(r.Context())
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, view.NetworkList{Data: view.Networks(s.svc.Registry(), pauses)})
	})
	s.public("GET /v1/checkout/links/{id}", "", func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.svc.PublicLink(r.Context(), r.PathValue("id"))
		return respond(w, p, err)
	})
	s.public("POST /v1/checkout/links/{id}/sessions", RouteLinkOpen, func(w http.ResponseWriter, r *http.Request) error {
		body, err := readBody(r)
		if err != nil {
			return err
		}
		m, err := fields(body, "amount", "client_reference_id")
		if err != nil {
			return err
		}
		amount, err := decodeField[string](m, "amount", "a decimal string such as \"25.00\"")
		if err != nil {
			return err
		}
		ref, err := decodeField[string](m, "client_reference_id", "a string")
		if err != nil {
			return err
		}
		id, replayed, err := s.svc.OpenLinkSession(r.Context(), r.PathValue("id"), r.Header.Get("Idempotency-Key"), body, amount, ref)
		if err != nil {
			return err
		}
		ses, err := c.Session(r.Context(), id, false)
		if err != nil {
			return err
		}
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		return writeJSON(w, http.StatusOK, ses)
	})
}
