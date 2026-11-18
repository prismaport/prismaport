package api

import (
	"net/http"

	"github.com/prismaport/prismaport/internal/checkout"
)

// MountCheckoutAddresses adds the public endpoints of the customer address page /a/{id}
// : the address's public view, and opening
// or resuming its session (rate limited per client IP, see ConfigurePublic). Like the other
// checkout endpoints they take no API key: the caddr_ id is the only scope.
func (s *Server) MountCheckoutAddresses(c *checkout.Checkout) {
	// ?active=1 says the page is open and visible: it records the payer's presence on the session
	// the page resumes, as on the session endpoint. Any other value is ignored.
	s.public("GET /v1/checkout/addresses/{id}", "", func(w http.ResponseWriter, r *http.Request) error {
		a, err := s.svc.PublicAddress(r.Context(), r.PathValue("id"), r.URL.Query().Get("active") == "1")
		return respond(w, a, err)
	})
	s.public("POST /v1/checkout/addresses/{id}/sessions", RouteAddressOpen, func(w http.ResponseWriter, r *http.Request) error {
		if _, err := readFields(r); err != nil {
			return err
		}
		id, err := s.svc.OpenAddressSessionPublic(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		ses, err := c.Session(r.Context(), id, false)
		return respond(w, ses, err)
	})
}
