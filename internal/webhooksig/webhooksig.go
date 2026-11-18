// Package webhooksig signs and verifies Prismaport webhook deliveries.
//
// Every delivery carries the header
//
//	Signature: t=<unix seconds>,v1=<hex hmac_sha256(secret, t + "." + body)>
//
// During a secret rotation the header carries one v1 per valid secret. Consumers must verify
// against the raw request body bytes, reject stale timestamps, and dedupe events by id.
package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// HeaderName is the HTTP header that carries the webhook signature.
const HeaderName = "Signature"

// DefaultTolerance is how far a delivery timestamp may be from the verifier's clock.
const DefaultTolerance = 5 * time.Minute

// Errors returned when a signature fails verification.
var (
	ErrMalformed = errors.New("webhooksig: malformed Signature header")
	ErrTimestamp = errors.New("webhooksig: timestamp outside tolerance")
	ErrMismatch  = errors.New("webhooksig: no signature matches")
)

// Compute returns the hex HMAC for one secret.
func Compute(secret string, t int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(t, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Header builds the header value, signing with each secret in order.
func Header(t time.Time, body []byte, secrets ...string) string {
	ts := t.Unix()
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(ts, 10))
	for _, s := range secrets {
		b.WriteString(",v1=")
		b.WriteString(Compute(s, ts, body))
	}
	return b.String()
}

// Parse splits a Signature header into its timestamp and its v1 signatures. Unknown keys are
// ignored, so future schemes can be added next to v1.
func Parse(header string) (t int64, sigs []string, err error) {
	t = -1
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return 0, nil, ErrMalformed
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || t != -1 {
				return 0, nil, ErrMalformed
			}
			t = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if t < 0 || len(sigs) == 0 {
		return 0, nil, ErrMalformed
	}
	return t, sigs, nil
}

// Matches reports whether one of sigs is the signature of body at t with secret (constant time).
func Matches(body []byte, t int64, sigs []string, secret string) bool {
	want := []byte(Compute(secret, t, body))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), want) {
			return true
		}
	}
	return false
}

// Verify checks header against body with secret. tolerance <= 0 uses DefaultTolerance.
func Verify(body []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	ts, sigs, err := Parse(header)
	if err != nil {
		return err
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew > tolerance || skew < -tolerance {
		return ErrTimestamp
	}
	if Matches(body, ts, sigs, secret) {
		return nil
	}
	return ErrMismatch
}
