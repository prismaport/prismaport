package dashauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
)

const tokenBytes = 32

// ErrMalformedToken is returned when a session or code token is not well formed.
var ErrMalformedToken = errors.New("dashauth: malformed token")

// NewToken returns a random 256-bit token (URL-safe base64) and the SHA-256 of it. Session
// tokens are stored by hash only, so a database read never yields a usable cookie.
func NewToken() (string, [32]byte, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", [32]byte{}, err
	}
	t := base64.RawURLEncoding.EncodeToString(b[:])
	return t, sha256.Sum256([]byte(t)), nil
}

// HashToken checks the shape of a presented token and returns its lookup hash.
func HashToken(t string) ([32]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(t)
	if err != nil || len(raw) != tokenBytes || base64.RawURLEncoding.EncodeToString(raw) != t {
		return [32]byte{}, ErrMalformedToken
	}
	return sha256.Sum256([]byte(t)), nil
}

// EqualTokens compares two secrets in constant time (for the CSRF header).
func EqualTokens(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
