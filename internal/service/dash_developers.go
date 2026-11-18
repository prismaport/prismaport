package service

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/store"
)

// APIKeyView is an API key as the dashboard lists it; the secret is present only when it was just
// created or rolled.
type APIKeyView struct {
	ID        string   `json:"id"`
	Name      *string  `json:"name"`
	Prefix    string   `json:"prefix"`
	Scopes    []string `json:"scopes"`
	Livemode  bool     `json:"livemode"`
	CreatedAt string   `json:"created_at"`
	RevokedAt *string  `json:"revoked_at"`
	// LastUsedAt is precise to LastUsedResolution.
	LastUsedAt *string `json:"last_used_at"`
	// ExpiresAt is when a rolled key stops working; ReplacedBy is the key the roll issued.
	ExpiresAt  *string `json:"expires_at"`
	ReplacedBy *string `json:"replaced_by"`
	// Status is active, expiring (rolled, still working until expires_at), expired or revoked.
	Status string `json:"status"`
	// Secret is present only in the create and roll responses.
	Secret string `json:"secret,omitempty"`
}

// API key statuses the dashboard shows; KeyExpiring marks a key whose expiry date has not been
// reached yet.
const (
	KeyActive   = "active"
	KeyExpiring = "expiring"
	KeyExpired  = "expired"
	KeyRevoked  = "revoked"
)

func keyStatus(k store.APIKey, now time.Time) string {
	switch {
	case k.ExpiresAt != nil && !now.Before(*k.ExpiresAt):
		return KeyExpired
	case k.RevokedAt != nil:
		return KeyRevoked
	case k.ExpiresAt != nil:
		return KeyExpiring
	}
	return KeyActive
}

func apiKeyView(k store.APIKeyRecord, secret string, now time.Time) APIKeyView {
	sc := k.Scopes
	if sc == nil {
		sc = []string{}
	}
	return APIKeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: sc, Livemode: k.Livemode, CreatedAt: rfc3339(k.CreatedAt),
		RevokedAt: optRFC3339(k.RevokedAt), LastUsedAt: optRFC3339(k.LastUsedAt), ExpiresAt: optRFC3339(k.ExpiresAt),
		ReplacedBy: k.ReplacedBy, Status: keyStatus(k.APIKey, now), Secret: secret}
}

// DashAPIKeys lists the merchant's API keys in the auth's mode with their current status.
func (s *Service) DashAPIKeys(ctx context.Context, a Auth) ([]APIKeyView, error) {
	list, err := s.store.ListAPIKeys(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return nil, err
	}
	now := s.store.Now()
	out := []APIKeyView{}
	for _, k := range list {
		out = append(out, apiKeyView(k, "", now))
	}
	return out, nil
}

// cleanScopes checks a requested scope list: at least one, all known, duplicates dropped.
func cleanScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, invalid("choose at least one scope")
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, sc := range scopes {
		if !apikey.ValidScope(sc) {
			return nil, invalid("unknown scope %q", sc)
		}
		if !seen[sc] {
			seen[sc] = true
			clean = append(clean, sc)
		}
	}
	return clean, nil
}

// DashCreateAPIKey issues a key and returns its secret once. Any non-empty subset of the scopes
// makes a restricted key. A live key takes a confirmation code (D58), after the request is
// validated, spent with the insert.
func (s *Service) DashCreateAPIKey(ctx context.Context, du DashUser, livemode bool, name string, scopes []string, proof StepUpProof) (APIKeyView, error) {
	if err := s.recoveryLock(du, livemode); err != nil {
		return APIKeyView{}, err
	}
	if n := utf8.RuneCountInString(name); n == 0 || n > 100 {
		return APIKeyView{}, invalid("name must have 1 to 100 characters")
	}
	clean, err := cleanScopes(scopes)
	if err != nil {
		return APIKeyView{}, err
	}
	g, err := apikey.Generate(livemode)
	if err != nil {
		return APIKeyView{}, err
	}
	var k store.APIKeyRecord
	if err := s.windowConfirm(ctx, du, livemode, ConfirmAPIKey, proof, func(b *Service) error {
		k, err = b.store.InsertNamedAPIKey(ctx, du.MerchantID, livemode, g.Hash, g.Prefix, clean, name, du.actor())
		return err
	}); err != nil {
		return APIKeyView{}, err
	}
	return apiKeyView(k, g.Plaintext, s.store.Now()), nil
}

// DashRevokeAPIKey revokes one of the merchant's API keys and records who did it.
func (s *Service) DashRevokeAPIKey(ctx context.Context, a Auth, id string) (APIKeyView, error) {
	k, err := s.store.RevokeMerchantAPIKey(ctx, a.MerchantID, a.Livemode, id, a.actor())
	if errors.Is(err, store.ErrNotFound) {
		return APIKeyView{}, keyNotFound()
	}
	if err != nil {
		return APIKeyView{}, err
	}
	return apiKeyView(k, "", s.store.Now()), nil
}

func keyNotFound() *Error { return newErr(KindNotFound, CodeNotFound, "no such API key") }

// RollOverlaps are how long a rolled key may keep working, by the name the API takes.
var RollOverlaps = map[string]time.Duration{"now": 0, "1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// RollResult is a roll: the new key with its secret (shown once) and the old key with the
// time it stops working.
type RollResult struct {
	APIKey   APIKeyView `json:"api_key"`
	Previous APIKeyView `json:"previous"`
}

// DashRollAPIKey replaces an active key with a new secret of the same name and scopes. The old
// key keeps working for the chosen overlap and is refused afterwards. A live key takes a
// confirmation code, checked after the key and spent with the roll, so a refused roll never
// spends a code.
func (s *Service) DashRollAPIKey(ctx context.Context, du DashUser, livemode bool, id, overlap string, proof StepUpProof) (RollResult, error) {
	if err := s.recoveryLock(du, livemode); err != nil {
		return RollResult{}, err
	}
	d, ok := RollOverlaps[overlap]
	if !ok {
		return RollResult{}, invalid("overlap must be one of now, 1h, 24h or 7d")
	}
	k, err := s.store.GetAPIKey(ctx, du.MerchantID, livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return RollResult{}, keyNotFound()
	}
	if err != nil {
		return RollResult{}, err
	}
	if !k.Active(s.store.Now()) || k.ReplacedBy != nil {
		return RollResult{}, keyNotRollable()
	}
	g, err := apikey.Generate(livemode)
	if err != nil {
		return RollResult{}, err
	}
	var fresh, old store.APIKeyRecord
	if err := s.liveConfirm(ctx, du, livemode, ConfirmAPIKey, proof, func(b *Service) error {
		fresh, old, err = b.store.RollAPIKey(ctx, du.MerchantID, livemode, id, g.Hash, g.Prefix, d, du.actor())
		switch {
		case errors.Is(err, store.ErrNotFound):
			return keyNotFound()
		case errors.Is(err, store.ErrKeyNotActive):
			return keyNotRollable()
		}
		return err
	}); err != nil {
		return RollResult{}, err
	}
	now := s.store.Now()
	return RollResult{APIKey: apiKeyView(fresh, g.Plaintext, now), Previous: apiKeyView(old, "", now)}, nil
}

func keyNotRollable() *Error {
	return newErr(KindConflict, CodeInvalidState, "this key is revoked, expired or already rolled; roll the key that replaced it")
}

// ExpireRolledAPIKeys records the end of every overlap that has passed (worker loop).
func (s *Service) ExpireRolledAPIKeys(ctx context.Context) (int, error) {
	return s.store.ExpireRolledAPIKeys(ctx, 200)
}

// DeliveryView is one webhook delivery attempt as the dashboard shows it.
type DeliveryView struct {
	ID             string  `json:"id"`
	EndpointID     string  `json:"endpoint_id"`
	EventID        string  `json:"event_id"`
	EventType      string  `json:"event_type"`
	SessionID      *string `json:"session_id"`
	Status         string  `json:"status"`
	Attempts       int     `json:"attempts"`
	NextAttemptAt  *string `json:"next_attempt_at"`
	LastAttemptAt  *string `json:"last_attempt_at"`
	LastStatusCode *int    `json:"last_status_code"`
	LastError      *string `json:"last_error"`
	DeliveredAt    *string `json:"delivered_at"`
	CreatedAt      string  `json:"created_at"`
	// Test marks a "Send test event" delivery: a sample event that is not in the feed.
	Test bool `json:"test"`
}

func deliveryView(d store.DeliveryRecord) DeliveryView {
	v := DeliveryView{ID: d.ID, EndpointID: d.EndpointID, EventID: d.EventID, EventType: d.EventType, SessionID: d.SessionID,
		Status: d.Status, Attempts: d.Attempts, LastAttemptAt: optRFC3339(d.LastAttemptAt), LastStatusCode: d.LastStatusCode,
		LastError: d.LastError, DeliveredAt: optRFC3339(d.DeliveredAt), CreatedAt: rfc3339(d.CreatedAt), Test: d.Test}
	if d.Status == "pending" {
		v.NextAttemptAt = optRFC3339(&d.NextAttemptAt)
	}
	return v
}

// DeliveryList is one page of webhook deliveries and the cursor of the next page, if any.
type DeliveryList struct {
	Data       []DeliveryView `json:"data"`
	NextCursor *string        `json:"next_cursor"`
}

// DashDeliveries returns one page of the deliveries of a webhook endpoint of the merchant.
func (s *Service) DashDeliveries(ctx context.Context, a Auth, endpointID, cursor string, limit int) (DeliveryList, error) {
	if len(cursor) > 64 {
		return DeliveryList{}, invalid("cursor is not valid")
	}
	list, more, err := s.store.ListDeliveries(ctx, a.MerchantID, a.Livemode, endpointID, cursor, limit)
	if errors.Is(err, store.ErrNotFound) {
		return DeliveryList{}, endpointNotFound()
	}
	if err != nil {
		return DeliveryList{}, err
	}
	out := DeliveryList{Data: []DeliveryView{}}
	for _, d := range list {
		out.Data = append(out.Data, deliveryView(d))
	}
	if more && len(list) > 0 {
		next := list[len(list)-1].ID
		out.NextCursor = &next
	}
	return out, nil
}

// DashResendDelivery schedules a webhook delivery of the merchant to be sent again and returns it.
func (s *Service) DashResendDelivery(ctx context.Context, a Auth, id string) (DeliveryView, error) {
	d, err := s.store.ResendDelivery(ctx, a.MerchantID, a.Livemode, id, a.actor())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return DeliveryView{}, newErr(KindNotFound, CodeNotFound, "no such webhook delivery")
	case errors.Is(err, store.ErrEndpointDeleted):
		return DeliveryView{}, newErr(KindConflict, CodeInvalidRequest, "the endpoint of this delivery was deleted")
	case errors.Is(err, store.ErrEndpointDisabled):
		return DeliveryView{}, newErr(KindConflict, CodeEndpointDisabled, "the endpoint of this delivery is disabled: enable it, then resend")
	case errors.Is(err, store.ErrTestDelivery):
		return DeliveryView{}, newErr(KindConflict, CodeInvalidRequest, "a test delivery is not resent; send a new test event instead")
	case err != nil:
		return DeliveryView{}, err
	}
	return deliveryView(d), nil
}
