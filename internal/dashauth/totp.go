package dashauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"hash"
	"net/url"
	"time"
)

// TOTP settings shared with every authenticator app: SHA-1, 6 digits, 30 s steps.
const (
	TOTPPeriod    = 30
	TOTPDigits    = 6
	TOTPSecretLen = 20
	// TOTPSkew accepts the previous and the next step for clock drift.
	TOTPSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret (the RFC 4226 recommendation).
func NewTOTPSecret() ([]byte, error) {
	s := make([]byte, TOTPSecretLen)
	_, err := rand.Read(s)
	return s, err
}

// EncodeSecret is the base32 form authenticator apps take.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// OTPAuthURL is the provisioning URI behind the enrollment QR code.
func OTPAuthURL(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(TOTPPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// HOTP is RFC 4226: the dynamically truncated HMAC of the counter, as `digits` decimal digits.
func HOTP(secret []byte, counter uint64, digits int, h func() hash.Hash) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(h, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// TOTPStep is the RFC 6238 time step of t.
func TOTPStep(t time.Time, period int64) int64 {
	return t.Unix() / period
}

// TOTP is RFC 6238 with T0 = 0.
func TOTP(secret []byte, t time.Time, period int64, digits int, h func() hash.Hash) string {
	return HOTP(secret, uint64(TOTPStep(t, period)), digits, h)
}

// VerifyTOTP checks a 6-digit SHA-1 code against the steps around now. It returns the matched
// step; a code is accepted only for a step after lastStep, so a used code can never be
// replayed (the caller stores the step).
func VerifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	if len(code) != TOTPDigits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	cur := TOTPStep(now, TOTPPeriod)
	matched, ok := int64(0), false
	for step := cur - TOTPSkew; step <= cur+TOTPSkew; step++ {
		want := HOTP(secret, uint64(step), TOTPDigits, sha1.New)
		// Every candidate is compared so the time does not reveal which step matched.
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && step > lastStep && !ok {
			matched, ok = step, true
		}
	}
	return matched, ok
}
