// Package apikey generates and parses secret API keys. Only the SHA-256 of a key is stored.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"

	"github.com/prismaport/prismaport/internal/base58"
)

// Key prefixes that tell a live API key from a test one.
const (
	LivePrefix = "sk_live_"
	TestPrefix = "sk_test_"
	secretLen  = 32
)

// ErrMalformed is returned when a token is not a well-formed API key.
var ErrMalformed = errors.New("apikey: malformed key")

// Scope is one permission an API key may hold.
type Scope string

// The API key scopes, one read and one write scope per resource where it applies.
const (
	ProductsRead  Scope = "products:read"
	ProductsWrite Scope = "products:write"
	SessionsRead  Scope = "sessions:read"
	SessionsWrite Scope = "sessions:write"
	EventsRead    Scope = "events:read"
	WebhooksRead  Scope = "webhooks:read"
	WebhooksWrite Scope = "webhooks:write"
	// WalletsRead covers reading payout wallets. Wallets are added and archived only in the
	// dashboard, behind its confirmation code, so no scope writes them.
	WalletsRead Scope = "wallets:read"
	// PaymentLinksRead and PaymentLinksWrite cover /v1/payment_links.
	PaymentLinksRead  Scope = "payment_links:read"
	PaymentLinksWrite Scope = "payment_links:write"
	// CustomerAddressesRead and CustomerAddressesWrite cover /v1/customer_addresses (B11).
	CustomerAddressesRead  Scope = "customer_addresses:read"
	CustomerAddressesWrite Scope = "customer_addresses:write"
)

// AllScopes lists every scope in a stable order.
var AllScopes = []Scope{ProductsRead, ProductsWrite, SessionsRead, SessionsWrite, EventsRead, WebhooksRead, WebhooksWrite,
	WalletsRead, PaymentLinksRead, PaymentLinksWrite, CustomerAddressesRead, CustomerAddressesWrite}

// ValidScope reports whether s names a known scope.
func ValidScope(s string) bool { return slices.Contains(AllScopes, Scope(s)) }

// Generated is a new key. Plaintext is shown once and never stored.
type Generated struct {
	Plaintext string
	Hash      [32]byte
	Prefix    string
}

// Generate returns a new random API key for the given mode, with the hash and prefix the store
// keeps.
func Generate(livemode bool) (Generated, error) {
	var secret [secretLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Generated{}, err
	}
	p := TestPrefix
	if livemode {
		p = LivePrefix
	}
	token := p + base58.Encode(secret[:])
	return Generated{Plaintext: token, Hash: sha256.Sum256([]byte(token)), Prefix: token[:len(p)+4]}, nil
}

// Parse checks the shape of a presented key and returns its mode and lookup hash.
func Parse(token string) (livemode bool, hash [32]byte, err error) {
	var body string
	switch {
	case strings.HasPrefix(token, LivePrefix):
		livemode, body = true, token[len(LivePrefix):]
	case strings.HasPrefix(token, TestPrefix):
		body = token[len(TestPrefix):]
	default:
		return false, hash, ErrMalformed
	}
	raw, err := base58.Decode(body)
	if err != nil || len(raw) != secretLen || base58.Encode(raw) != body {
		return false, hash, ErrMalformed
	}
	return livemode, sha256.Sum256([]byte(token)), nil
}
