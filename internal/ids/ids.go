// Package ids generates prefixed, time-ordered identifiers ("ses_01JB...") in ULID layout:
// 48-bit millisecond timestamp + 80 random bits, Crockford base32.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// ID prefixes of each kind of object.
const (
	Merchant        = "mer"
	Destination     = "dst"
	APIKey          = "key"
	Product         = "prod"
	PaymentLink     = "plink"
	CustomerAddress = "caddr"
	Session         = "ses"
	Payment         = "pay"
	Settlement      = "stl"
	Event           = "evt"
	WebhookEndpoint = "we"
	WebhookDelivery = "whd"
	User            = "usr"
	SandboxInbox    = "sbx"
	SandboxRequest  = "sbr"
	Wallet          = "wal"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns prefix + "_" + a 26-character ULID for the current time. Code with a clock uses
// NewAt with it, except for records whose lists sort by id (API keys, products, links, webhook
// endpoints, payments, settlements): under a fixed test clock IDs made in the same millisecond
// sort at random, so those keep the wall clock's creation order.
func New(prefix string) string { return NewAt(prefix, time.Now()) }

// NewAt is New with an explicit timestamp.
func NewAt(prefix string, t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("ids: crypto/rand failed: %v", err))
	}
	return prefix + "_" + encode(b)
}

func encode(b [16]byte) string {
	var out [26]byte
	// 128 bits -> 26 base32 symbols; the first symbol carries the top 3 bits.
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// Valid reports whether s is a well-formed identifier with the given prefix.
func Valid(prefix, s string) bool {
	rest, ok := strings.CutPrefix(s, prefix+"_")
	if !ok || len(rest) != 26 || rest[0] > '7' {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if strings.IndexByte(alphabet, rest[i]) < 0 {
			return false
		}
	}
	return true
}
