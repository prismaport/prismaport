package api

import (
	"encoding/json"
	"net/http"

	"github.com/prismaport/prismaport/internal/service"
)

var productFields = []string{"name", "description", "price_type", "amount", "min_amount", "max_amount", "suggested_amounts", "success_url",
	"metadata", "active", "payout", "networks"}

// productPatchFields are the members a product patch takes: a create's, and links.
var productPatchFields = append(append([]string(nil), productFields...), "links")

// productCreateInput decodes a product create body.
func productCreateInput(body []byte) (service.ProductInput, error) {
	return productInput(body, productFields)
}

// productPatchInput decodes a product patch body; only a patch takes links.
func productPatchInput(body []byte) (service.ProductInput, error) {
	return productInput(body, productPatchFields)
}

// productInput decodes a product body that may hold the members allowed. What null means is the
// service's (service.ProductInput).
func productInput(body []byte, allowed []string) (service.ProductInput, error) {
	m, err := fields(body, allowed...)
	if err != nil {
		return service.ProductInput{}, err
	}
	d := &members{m: m}
	in := service.ProductInput{
		Name:             member[string](d, "name", "a string"),
		Description:      member[string](d, "description", "a string or null"),
		PriceType:        member[string](d, "price_type", "a string"),
		Amount:           member[string](d, "amount", "a decimal string such as \"25.00\""),
		MinAmount:        member[string](d, "min_amount", "a decimal string"),
		MaxAmount:        member[string](d, "max_amount", "a decimal string"),
		SuggestedAmounts: member[[]string](d, "suggested_amounts", "an array of decimal strings"),
		SuccessURL:       member[string](d, "success_url", "a string"),
		Metadata:         member[map[string]string](d, "metadata", "an object of string values"),
		Active:           member[bool](d, "active", "a boolean"),
	}
	if d.err != nil {
		return in, d.err
	}
	if raw, ok := m["payout"]; ok {
		if in.Payout, err = productPayout(raw); err != nil {
			return in, err
		}
	}
	if raw, ok := m["networks"]; ok {
		if in.Networks, err = productNetworks(raw); err != nil {
			return in, err
		}
	}
	if in.Links, err = field[string](m, "links", "keep or deactivate"); err != nil {
		return in, badParam("links", "links must be keep or deactivate")
	}
	return in, nil
}

var linkFields = []string{"product_id", "name", "amount", "active", "expires_at", "max_payments", "success_url", "metadata"}

// linkInput decodes a payment link create or patch body (merchant API and dashboard alike); null
// clears name, amount, expires_at, max_payments, success_url and metadata.
func linkInput(body []byte) (service.LinkInput, error) {
	m, err := fields(body, linkFields...)
	if err != nil {
		return service.LinkInput{}, err
	}
	d := &members{m: m}
	in := service.LinkInput{
		ProductID:   member[string](d, "product_id", "a string"),
		Name:        member[string](d, "name", "a string or null"),
		Amount:      member[string](d, "amount", "a decimal string such as \"25.00\" or null"),
		Active:      member[bool](d, "active", "a boolean"),
		ExpiresAt:   member[string](d, "expires_at", "an RFC 3339 time or null"),
		MaxPayments: member[int64](d, "max_payments", "a positive integer or null"),
		SuccessURL:  member[string](d, "success_url", "a string or null"),
	}
	if d.err != nil {
		return in, d.err
	}
	if in.Metadata, err = field[map[string]string](m, "metadata", "an object of string values"); err != nil {
		return in, badParam("metadata", "metadata must be an object of string values")
	}
	return in, nil
}

func (s *Server) createPaymentLink(r *http.Request, a service.Auth, key string, body []byte, in service.LinkInput) (any, bool, error) {
	return s.svc.CreatePaymentLink(r.Context(), a, key, body, in)
}

func (s *Server) listPaymentLinks(r *http.Request, a service.Auth, p listPage) (any, error) {
	q := r.URL.Query()
	return s.svc.ListPaymentLinks(r.Context(), a, q.Get("product_id"), q.Get("status"), p.cursor, p.limit)
}

func (s *Server) getPaymentLink(r *http.Request, a service.Auth) (any, error) {
	return s.svc.GetPaymentLink(r.Context(), a, r.PathValue("id"))
}

func (s *Server) updatePaymentLink(r *http.Request, a service.Auth, in service.LinkInput) (any, error) {
	return s.svc.UpdatePaymentLink(r.Context(), a, r.PathValue("id"), in)
}

func (s *Server) createProduct(r *http.Request, a service.Auth, in service.ProductInput) (any, error) {
	return s.svc.CreateProduct(r.Context(), a, in)
}

func (s *Server) getProduct(r *http.Request, a service.Auth) (any, error) {
	return s.svc.GetProduct(r.Context(), a, r.PathValue("id"))
}

// listProducts pages with starting_after, not cursor.
func (s *Server) listProducts(r *http.Request, a service.Auth, p listPage) (any, error) {
	return s.svc.ListProducts(r.Context(), a, p.limit, r.URL.Query().Get("starting_after"))
}

func (s *Server) updateProduct(r *http.Request, a service.Auth, in service.ProductInput) (any, error) {
	return s.svc.UpdateProduct(r.Context(), a, r.PathValue("id"), in)
}

var sessionFields = []string{"product_id", "customer_address_id", "amount", "metadata", "success_url", "expires_in", "client_reference_id"}

// createSession answers with the session as the service stored it, so a replay answers the same
// bytes.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request, a service.Auth) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	m, err := fields(body, sessionFields...)
	if err != nil {
		return err
	}
	d := &members{m: m}
	in := service.SessionInput{
		ProductID:         member[string](d, "product_id", "a string").Ptr(),
		CustomerAddressID: member[string](d, "customer_address_id", "a string").Ptr(),
		Amount:            member[string](d, "amount", "a decimal string such as \"25.00\"").Ptr(),
		SuccessURL:        member[string](d, "success_url", "a string").Ptr(),
		ExpiresIn:         member[int64](d, "expires_in", "an integer number of seconds").Ptr(),
		ClientReferenceID: member[string](d, "client_reference_id", "a string").Ptr(),
		Metadata:          member[map[string]string](d, "metadata", "an object of string values").Value,
	}
	if d.err != nil {
		return d.err
	}
	resp, replayed, err := s.svc.CreateSession(r.Context(), a, r.Header.Get("Idempotency-Key"), body, in)
	if err != nil {
		return err
	}
	markReplayed(w, replayed)
	writeRaw(w, resp.Status, resp.Body)
	return nil
}

// createCustomerAddress answers 201 for a new address and 200 for the customer's existing one.
func (s *Server) createCustomerAddress(w http.ResponseWriter, r *http.Request, a service.Auth) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	m, err := fields(body, "customer_id", "product_id", "metadata")
	if err != nil {
		return err
	}
	d := &members{m: m}
	in := service.CustomerAddressInput{
		CustomerID: member[string](d, "customer_id", "a string").Ptr(),
		ProductID:  member[string](d, "product_id", "a string").Ptr(),
		Metadata:   member[map[string]string](d, "metadata", "an object of string values").Value,
	}
	if d.err != nil {
		return d.err
	}
	ca, status, replayed, err := s.svc.CreateCustomerAddress(r.Context(), a, r.Header.Get("Idempotency-Key"), body, in)
	if err != nil {
		return err
	}
	markReplayed(w, replayed)
	return writeJSON(w, status, ca)
}

func (s *Server) listCustomerAddresses(r *http.Request, a service.Auth, p listPage) (any, error) {
	return s.svc.ListCustomerAddresses(r.Context(), a, r.URL.Query().Get("customer_id"), p.cursor, p.limit)
}

func (s *Server) getCustomerAddress(r *http.Request, a service.Auth) (any, error) {
	return s.svc.GetCustomerAddress(r.Context(), a, r.PathValue("id"))
}

func (s *Server) rotateCustomerAddress(r *http.Request, a service.Auth) (any, error) {
	return s.svc.RotateCustomerAddress(r.Context(), a, r.PathValue("id"))
}

func (s *Server) listAddressSessions(r *http.Request, a service.Auth, p listPage) (any, error) {
	return s.svc.ListAddressSessions(r.Context(), a, r.PathValue("id"), p.cursor, p.limit)
}

func (s *Server) getSession(r *http.Request, a service.Auth) (any, error) {
	return s.svc.GetSession(r.Context(), a, r.PathValue("id"))
}

// revealSession reveals one network of a session for a merchant's own payment page (D48): 200
// with the session, its deposit details on that network included.
func (s *Server) revealSession(r *http.Request, a service.Auth) (any, error) {
	chainID, err := revealChain(r)
	if err != nil {
		return nil, err
	}
	ses, err := s.svc.RevealSession(r.Context(), a, r.PathValue("id"), chainID)
	if err != nil {
		return nil, err
	}
	return s.svc.View().Session(ses), nil
}

// recheckSession queues "Check again" for a session past its watch window (202 with the
// session; a check already queued answers the same).
func (s *Server) recheckSession(w http.ResponseWriter, r *http.Request, a service.Auth) error {
	ses, err := s.svc.RecheckSession(r.Context(), a, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, s.svc.View().Session(ses))
}

// The events list holds defaultEventsLimit events unless limit asks for 1 to maxEventsLimit.
const (
	defaultEventsLimit = 50
	maxEventsLimit     = 200
)

// listEvents lists the events after the after query parameter (an event sequence), or with
// after=latest only the cursor to start from.
func (s *Server) listEvents(r *http.Request, a service.Auth) (any, error) {
	if r.URL.Query().Get("after") == "latest" {
		return s.svc.LatestEventCursor(r.Context(), a)
	}
	after, err := queryInt(r, "after", 0, 0, 1<<62)
	if err != nil {
		return nil, err
	}
	limit, err := queryInt(r, "limit", defaultEventsLimit, 1, maxEventsLimit)
	if err != nil {
		return nil, err
	}
	return s.svc.ListEvents(r.Context(), a, after, int(limit))
}

// endpointFields are the members a webhook endpoint create takes; an edit also takes status.
var endpointFields = []string{"url", "enabled_events", "description"}

// endpointEditFields are the members a webhook endpoint edit takes.
var endpointEditFields = append(append([]string(nil), endpointFields...), "status")

// endpointCreateInput decodes a webhook endpoint create.
func endpointCreateInput(m map[string]json.RawMessage) (service.EndpointInput, error) {
	var in service.EndpointInput
	err := endpointMembers(m, &in)
	return in, err
}

// endpointEditInput decodes a webhook endpoint edit: every member is optional, and only
// description takes null (it clears it, like "").
func endpointEditInput(m map[string]json.RawMessage) (service.EndpointInput, error) {
	var in service.EndpointInput
	for _, name := range []string{"url", "enabled_events", "status"} {
		if raw, ok := m[name]; ok && isNull(raw) {
			return in, badParam(name, "%s cannot be null: leave it out to keep it", name)
		}
	}
	if err := endpointMembers(m, &in); err != nil {
		return in, err
	}
	var err error
	in.Status, err = decodeField[string](m, "status", "enabled or disabled")
	return in, err
}

// endpointMembers decodes the members a create and an edit share.
func endpointMembers(m map[string]json.RawMessage, in *service.EndpointInput) error {
	d := &members{m: m}
	in.URL = member[string](d, "url", "a string").Ptr()
	in.EnabledEvents = member[[]string](d, "enabled_events", "an array of event types").Ptr()
	desc := member[string](d, "description", "a string or null")
	in.SetDescription, in.Description = desc.Set, desc.Ptr()
	return d.err
}

// bodyFields returns a body decoder that takes the members allowed and decodes them with decode.
func bodyFields[In any](decode func(map[string]json.RawMessage) (In, error), allowed ...string) func([]byte) (In, error) {
	return func(body []byte) (In, error) {
		m, err := fields(body, allowed...)
		if err != nil {
			var zero In
			return zero, err
		}
		return decode(m)
	}
}

func (s *Server) createWebhookEndpoint(r *http.Request, a service.Auth, in service.EndpointInput) (any, error) {
	return s.svc.CreateWebhookEndpoint(r.Context(), a, in)
}

func (s *Server) updateWebhookEndpoint(r *http.Request, a service.Auth, in service.EndpointInput) (any, error) {
	return s.svc.UpdateWebhookEndpoint(r.Context(), a, r.PathValue("id"), in)
}

func (s *Server) listWebhookEndpoints(r *http.Request, a service.Auth) (any, error) {
	list, err := s.svc.ListWebhookEndpoints(r.Context(), a)
	return WebhookEndpointList{Data: list}, err
}

func (s *Server) deleteWebhookEndpoint(r *http.Request, a service.Auth) (any, error) {
	err := s.svc.DeleteWebhookEndpoint(r.Context(), a, r.PathValue("id"))
	return Deleted{ID: r.PathValue("id"), Deleted: true}, err
}

func (s *Server) rotateWebhookSecret(r *http.Request, a service.Auth) (any, error) {
	return s.svc.RotateWebhookSecret(r.Context(), a, r.PathValue("id"))
}
