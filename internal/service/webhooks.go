package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// secretPrefix starts every secret issued now. Signing and verification treat a secret as an
// opaque string, so secrets issued without it keep working.
const secretPrefix = "whsec_"

// WebhookEndpointView is a webhook endpoint as the API returns it.
type WebhookEndpointView struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	EnabledEvents []string `json:"enabled_events"`
	// Description is the merchant's own note; it is never sent in a delivery.
	Description *string `json:"description"`
	Livemode    bool    `json:"livemode"`
	// Status is enabled or disabled; DisabledReason says why (merchant: paused, failing:
	// disabled by Prismaport after failing too long) and DisabledAt since when.
	Status         string  `json:"status"`
	DisabledReason *string `json:"disabled_reason"`
	DisabledAt     *string `json:"disabled_at"`
	// FailingSince is the start of the open failure streak; it is kept while disabled.
	FailingSince *string `json:"failing_since"`
	CreatedAt    string  `json:"created_at"`
	// PreviousSecretExpiresAt is set while a rotated-out secret still signs deliveries.
	PreviousSecretExpiresAt *string `json:"previous_secret_expires_at"`
	// Secret is present only in the create and rotate responses.
	Secret string `json:"secret,omitempty"`
}

func endpointView(e store.WebhookEndpoint, secret string, now time.Time) WebhookEndpointView {
	ev := e.EnabledEvents
	if ev == nil {
		ev = []string{}
	}
	v := WebhookEndpointView{ID: e.ID, URL: e.URL, EnabledEvents: ev, Description: e.Description, Livemode: e.Livemode,
		Status: e.Status, DisabledReason: e.DisabledReason, DisabledAt: optRFC3339(e.DisabledAt), FailingSince: optRFC3339(e.FailingSince),
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339), Secret: secret}
	if e.PreviousSecretSealed != nil && e.PreviousSecretExpiresAt != nil && now.Before(*e.PreviousSecretExpiresAt) {
		v.PreviousSecretExpiresAt = optRFC3339(e.PreviousSecretExpiresAt)
	}
	return v
}

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return secretPrefix + base58.Encode(b[:]), nil
}

func (s *Service) sealSecret(endpointID, secret string) ([]byte, error) {
	if s.cfg.SecretBox == nil {
		return nil, errors.New("service: no secret box configured")
	}
	return s.cfg.SecretBox.Seal([]byte(secret), []byte(endpointID))
}

// OpenSecret decrypts an endpoint secret; the AAD binds it to its endpoint.
func (s *Service) OpenSecret(endpointID string, sealed []byte) (string, error) {
	if s.cfg.SecretBox == nil {
		return "", errors.New("service: no secret box configured")
	}
	b, err := s.cfg.SecretBox.Open(sealed, []byte(endpointID))
	return string(b), err
}

// maxDescription is the most characters an endpoint description holds.
const maxDescription = 200

// EndpointInput is a create (url required) or an edit (nil fields do not change) of an endpoint.
type EndpointInput struct {
	URL           *string
	EnabledEvents *[]string
	// SetDescription changes the description; Description nil or "" clears it.
	SetDescription bool
	Description    *string
	Status         *string
}

// cleanEvents refuses unknown event types and drops repeats, keeping the first order.
func cleanEvents(events []string) ([]string, error) {
	seen := map[string]bool{}
	clean := []string{}
	for _, e := range events {
		if !session.EventType(e).Valid() {
			return nil, invalid("unknown event type %q", e)
		}
		if !seen[e] {
			seen[e] = true
			clean = append(clean, e)
		}
	}
	return clean, nil
}

// cleanDescription checks a description; "" means none.
func cleanDescription(d *string) (*string, error) {
	if d == nil || *d == "" {
		return nil, nil
	}
	if !utf8.ValidString(*d) || utf8.RuneCountInString(*d) > maxDescription {
		return nil, &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "description",
			Message: fmt.Sprintf("description must be at most %d characters", maxDescription)}
	}
	return d, nil
}

// CreateWebhookEndpoint validates the URL and events and creates a webhook endpoint with a new
// signing secret; it needs the webhooks write scope.
func (s *Service) CreateWebhookEndpoint(ctx context.Context, a Auth, in EndpointInput) (WebhookEndpointView, error) {
	if err := a.Require(apikey.WebhooksWrite); err != nil {
		return WebhookEndpointView{}, err
	}
	if in.URL == nil {
		return WebhookEndpointView{}, invalid("url is required")
	}
	if err := s.checkWebhookURL(*in.URL, a.Livemode); err != nil {
		return WebhookEndpointView{}, err
	}
	var events []string
	if in.EnabledEvents != nil {
		events = *in.EnabledEvents
	}
	clean, err := cleanEvents(events)
	if err != nil {
		return WebhookEndpointView{}, err
	}
	desc, err := cleanDescription(in.Description)
	if err != nil {
		return WebhookEndpointView{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return WebhookEndpointView{}, err
	}
	id := ids.New(ids.WebhookEndpoint)
	sealed, err := s.sealSecret(id, secret)
	if err != nil {
		return WebhookEndpointView{}, err
	}
	e, err := s.store.CreateWebhookEndpoint(ctx, store.WebhookEndpoint{ID: id, MerchantID: a.MerchantID, Livemode: a.Livemode,
		URL: *in.URL, EnabledEvents: clean, Description: desc, SecretSealed: sealed}, a.actor())
	if errors.Is(err, store.ErrEndpointLimit) {
		return WebhookEndpointView{}, invalid("a mode holds at most %d webhook endpoints: delete one first", store.MaxWebhookEndpoints)
	}
	if err != nil {
		return WebhookEndpointView{}, err
	}
	return endpointView(e, secret, s.store.Now()), nil
}

// UpdateWebhookEndpoint edits an endpoint's url, enabled_events, description or status (see
// store.UpdateWebhookEndpoint). The secret never changes: rotate it after moving the URL to
// another party. The sandbox inbox's endpoint is managed through the inbox and refuses edits.
func (s *Service) UpdateWebhookEndpoint(ctx context.Context, a Auth, id string, in EndpointInput) (WebhookEndpointView, error) {
	if err := a.Require(apikey.WebhooksWrite); err != nil {
		return WebhookEndpointView{}, err
	}
	if in.URL == nil && in.EnabledEvents == nil && !in.SetDescription && in.Status == nil {
		return WebhookEndpointView{}, invalid("nothing to change: send url, enabled_events, description or status")
	}
	p := store.EndpointPatch{URL: in.URL, Status: in.Status, SetDescription: in.SetDescription,
		CheckURL: func(url string) error { return s.checkWebhookURL(url, a.Livemode) }}
	if in.URL != nil {
		if err := s.checkWebhookURL(*in.URL, a.Livemode); err != nil {
			return WebhookEndpointView{}, err
		}
	}
	if in.EnabledEvents != nil {
		clean, err := cleanEvents(*in.EnabledEvents)
		if err != nil {
			return WebhookEndpointView{}, err
		}
		p.EnabledEvents = &clean
	}
	if in.SetDescription {
		desc, err := cleanDescription(in.Description)
		if err != nil {
			return WebhookEndpointView{}, err
		}
		p.Description = desc
	}
	if in.Status != nil && *in.Status != store.EndpointEnabled && *in.Status != store.EndpointDisabled {
		return WebhookEndpointView{}, &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "status",
			Message: "status must be enabled or disabled"}
	}
	e, err := s.store.UpdateWebhookEndpoint(ctx, a.MerchantID, a.Livemode, id, p, a.actor())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return WebhookEndpointView{}, endpointNotFound()
	case errors.Is(err, store.ErrInboxEndpoint):
		return WebhookEndpointView{}, invalid("the sandbox inbox endpoint cannot be edited: delete the inbox and create a new one instead")
	case err != nil:
		return WebhookEndpointView{}, err
	}
	return endpointView(e, "", s.store.Now()), nil
}

// ListWebhookEndpoints returns every webhook endpoint of the merchant in the auth's mode; it needs
// the webhooks read scope.
func (s *Service) ListWebhookEndpoints(ctx context.Context, a Auth) ([]WebhookEndpointView, error) {
	if err := a.Require(apikey.WebhooksRead); err != nil {
		return nil, err
	}
	list, err := s.store.ListWebhookEndpoints(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return nil, err
	}
	out := []WebhookEndpointView{}
	now := s.store.Now()
	for _, e := range list {
		out = append(out, endpointView(e, "", now))
	}
	return out, nil
}

func endpointNotFound() *Error {
	return newErr(KindNotFound, CodeNotFound, "no such webhook endpoint")
}

// DeleteWebhookEndpoint removes one of the merchant's webhook endpoints; it needs the webhooks
// write scope.
func (s *Service) DeleteWebhookEndpoint(ctx context.Context, a Auth, id string) error {
	if err := a.Require(apikey.WebhooksWrite); err != nil {
		return err
	}
	err := s.store.DeleteWebhookEndpoint(ctx, a.MerchantID, a.Livemode, id, a.actor())
	if errors.Is(err, store.ErrNotFound) {
		return endpointNotFound()
	}
	return err
}

// CodeSecretRotationPending refuses a rotation while the previous secret still signs deliveries.
const CodeSecretRotationPending = "secret_rotation_pending"

func rotationPending(until time.Time) error {
	return newErr(KindConflict, CodeSecretRotationPending,
		"the previous secret still signs deliveries until %s; rotate again after that (if the current secret leaked, delete this endpoint and create a new one)",
		until.UTC().Format("2006-01-02 15:04 UTC"))
}

// RotateWebhookSecret issues a new secret; the previous one keeps signing deliveries for the
// rotation window (24 hours) so consumers can switch without dropping events. Rotating again
// inside the window is refused (secret_rotation_pending): it would end at once the secret a
// consumer that has not switched yet still verifies with.
func (s *Service) RotateWebhookSecret(ctx context.Context, a Auth, id string) (WebhookEndpointView, error) {
	if err := a.Require(apikey.WebhooksWrite); err != nil {
		return WebhookEndpointView{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return WebhookEndpointView{}, err
	}
	e, err := s.store.RotateWebhookSecret(ctx, a.MerchantID, a.Livemode, id, func(eid string) ([]byte, error) {
		return s.sealSecret(eid, secret)
	}, s.store.Now().Add(s.cfg.SecretRotation), a.actor())
	var pending *store.RotationPendingError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return WebhookEndpointView{}, endpointNotFound()
	case errors.As(err, &pending):
		return WebhookEndpointView{}, rotationPending(pending.Until)
	case err != nil:
		return WebhookEndpointView{}, err
	}
	return endpointView(e, secret, s.store.Now()), nil
}

// SigningSecrets returns the secrets a delivery must be signed with right now: the current one
// first, then the previous one while its rotation window is open.
func (s *Service) SigningSecrets(e store.WebhookEndpoint, now time.Time) ([]string, error) {
	cur, err := s.OpenSecret(e.ID, e.SecretSealed)
	if err != nil {
		return nil, err
	}
	out := []string{cur}
	if e.PreviousSecretSealed != nil && e.PreviousSecretExpiresAt != nil && now.Before(*e.PreviousSecretExpiresAt) {
		prev, err := s.OpenSecret(e.ID, e.PreviousSecretSealed)
		if err != nil {
			return nil, err
		}
		out = append(out, prev)
	}
	return out, nil
}

// DashCreateWebhookEndpoint is CreateWebhookEndpoint from the dashboard. A Live mode endpoint
// receives every live payment event, so it takes the confirmation of a live API key: a code
// emailed to the owner or, with 2FA on, the TOTP code (ADR 0008 C-3, D58). A stolen session alone
// cannot add one. The request is checked first and the code spent with the insert, so a refused
// one (the endpoint limit included) never spends the code.
func (s *Service) DashCreateWebhookEndpoint(ctx context.Context, du DashUser, a Auth, in EndpointInput, proof StepUpProof) (WebhookEndpointView, error) {
	if a.Livemode {
		if in.URL == nil {
			return WebhookEndpointView{}, invalid("url is required")
		}
		if err := s.checkWebhookURL(*in.URL, true); err != nil {
			return WebhookEndpointView{}, err
		}
		if in.EnabledEvents != nil {
			if _, err := cleanEvents(*in.EnabledEvents); err != nil {
				return WebhookEndpointView{}, err
			}
		}
		if _, err := cleanDescription(in.Description); err != nil {
			return WebhookEndpointView{}, err
		}
	}
	var out WebhookEndpointView
	err := s.windowConfirm(ctx, du, a.Livemode, ConfirmWebhookEndpoint, proof, func(b *Service) error {
		var err error
		out, err = b.CreateWebhookEndpoint(ctx, a, in)
		return err
	})
	return out, err
}

// DashUpdateWebhookEndpoint is UpdateWebhookEndpoint from the dashboard: moving a Live mode
// endpoint to another URL takes the confirmation of creating one. Every other edit (events,
// description, pause, the same URL) takes none. The code is spent with the edit.
func (s *Service) DashUpdateWebhookEndpoint(ctx context.Context, du DashUser, a Auth, id string, in EndpointInput, proof StepUpProof) (WebhookEndpointView, error) {
	if a.Livemode && in.URL != nil {
		e, err := s.store.GetWebhookEndpoint(ctx, a.MerchantID, a.Livemode, id)
		if errors.Is(err, store.ErrNotFound) {
			return WebhookEndpointView{}, endpointNotFound()
		}
		if err != nil {
			return WebhookEndpointView{}, err
		}
		if e.URL != *in.URL {
			if err := s.checkWebhookURL(*in.URL, true); err != nil {
				return WebhookEndpointView{}, err
			}
			var out WebhookEndpointView
			err := s.windowConfirm(ctx, du, true, ConfirmWebhookEndpoint, proof, func(b *Service) error {
				out, err = b.UpdateWebhookEndpoint(ctx, a, id, in)
				return err
			})
			return out, err
		}
	}
	return s.UpdateWebhookEndpoint(ctx, a, id, in)
}

// DashRotateWebhookSecret is RotateWebhookSecret from the dashboard: a Live mode secret is a
// credential that outlives the session, so rotating one takes the confirmation, spent with the
// rotation.
func (s *Service) DashRotateWebhookSecret(ctx context.Context, du DashUser, a Auth, id string, proof StepUpProof) (WebhookEndpointView, error) {
	if a.Livemode {
		e, err := s.store.GetWebhookEndpoint(ctx, a.MerchantID, true, id)
		if errors.Is(err, store.ErrNotFound) {
			return WebhookEndpointView{}, endpointNotFound()
		} else if err != nil {
			return WebhookEndpointView{}, err
		}
		// A rotation the window refuses never spends the confirmation.
		if until := e.PreviousSecretExpiresAt; until != nil && s.store.Now().Before(*until) {
			return WebhookEndpointView{}, rotationPending(*until)
		}
	}
	var out WebhookEndpointView
	err := s.liveConfirm(ctx, du, a.Livemode, ConfirmWebhookEndpoint, proof, func(b *Service) error {
		var err error
		out, err = b.RotateWebhookSecret(ctx, a, id)
		return err
	})
	return out, err
}
