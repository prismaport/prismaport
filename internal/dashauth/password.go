// Package dashauth holds the dashboard's credential primitives: argon2id password hashes,
// RFC 6238 TOTP codes and random session tokens. It does no I/O.
package dashauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters. They are stored in every hash, so raising them later
// only affects new hashes.
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultParams exceed the OWASP argon2id minimums (19 MiB, t=2, p=1).
var DefaultParams = Params{MemoryKiB: 64 * 1024, Time: 2, Threads: 1}

// Hashing sizes and the password length bounds.
const (
	saltLen = 16
	keyLen  = 32
	// MinPasswordLen and MaxPasswordLen bound what a user may set; the upper bound keeps a
	// hash request from becoming a denial of service. Verifying applies only the upper bound,
	// so an account with a password from an earlier policy keeps working.
	MinPasswordLen = 8
	MaxPasswordLen = 256
	// maxRepeatedDistinct is the most distinct characters, after case folding, of a password
	// refused as repeated ("Aa1!Aa1!").
	maxRepeatedDistinct = 3
)

// Errors returned when hashing, verifying or checking a password.
var (
	ErrMalformedHash = errors.New("dashauth: malformed password hash")
	// ErrWeakPassword is a new password outside the length bounds or missing a character class.
	ErrWeakPassword = fmt.Errorf("dashauth: password must have %d to %d characters, with an uppercase letter, a lowercase letter, a number and a special character", MinPasswordLen, MaxPasswordLen)
	// ErrCommonPassword is a new password on the common list, or one repeating a few characters.
	ErrCommonPassword = errors.New("dashauth: password is common or repeated")
	// ErrBusy is a password hash refused because every hashing slot of its Hasher stayed taken
	// for the Hasher's wait.
	// Callers answer it with 503 and Retry-After.
	ErrBusy = errors.New("dashauth: too many password checks at once")
)

// DefaultHashWait is how long a hash waits for a free slot before it gives up with ErrBusy. A
// hash takes tens of milliseconds, so a short queue drains well within it.
const DefaultHashWait = time.Second

// Hasher runs the password hashes of one process. It caps how many argon2id hashes run at once:
// each holds MemoryKiB (64 MiB) for its whole run and the routes that hash are public, so without
// a cap a burst of logins or sign-ups could take the memory of the process that also serves
// checkout and the merchant API. The composition root makes one per process.
type Hasher struct {
	slots chan struct{}
	wait  time.Duration
}

// NewHasher returns a Hasher with slots hashing slots (zero or less: one per core, since more
// only queues on the CPU anyway) whose hashes wait up to wait for one (zero or less:
// DefaultHashWait).
func NewHasher(slots int, wait time.Duration) *Hasher {
	if slots <= 0 {
		slots = runtime.GOMAXPROCS(0)
	}
	if wait <= 0 {
		wait = DefaultHashWait
	}
	return &Hasher{slots: make(chan struct{}, max(1, slots)), wait: wait}
}

// idKey runs argon2id in a hashing slot. A request that ends while it waits for one stops
// waiting.
func (h *Hasher) idKey(ctx context.Context, pw, salt []byte, p Params, keyLen uint32) ([]byte, error) {
	select {
	case h.slots <- struct{}{}:
	default:
		t := time.NewTimer(h.wait)
		defer t.Stop()
		select {
		case h.slots <- struct{}{}:
		case <-t.C:
			return nil, ErrBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer func() { <-h.slots }()
	return argon2.IDKey(pw, salt, p.Time, p.MemoryKiB, p.Threads, keyLen), nil
}

// commonPasswordList holds common passwords that would otherwise meet the character rules, one
// per line as people type them. web/dashboard/src/lib/commonPasswords.json mirrors it for the
// live checklist; a test keeps the two equal.
//
//go:embed common_passwords.txt
var commonPasswordList string

var commonPasswords = func() map[string]struct{} {
	set := make(map[string]struct{})
	for _, line := range strings.Split(commonPasswordList, "\n") {
		if pw := strings.TrimSpace(line); pw != "" {
			set[strings.ToLower(pw)] = struct{}{}
		}
	}
	return set
}()

// hasCharacterClasses reports an uppercase letter, a lowercase letter, a number and a special
// character (anything that is not a letter or a number, a space included).
func hasCharacterClasses(pw string) bool {
	var upper, lower, number, special bool
	for _, r := range pw {
		switch {
		case unicode.Is(unicode.Lu, r):
			upper = true
		case unicode.Is(unicode.Ll, r):
			lower = true
		case unicode.IsNumber(r):
			number = true
		case !unicode.IsLetter(r):
			special = true
		}
	}
	return upper && lower && number && special
}

// commonOrRepeated compares in lower case, like the dashboard's mirror of this rule.
func commonOrRepeated(pw string) bool {
	folded := strings.ToLower(pw)
	if _, ok := commonPasswords[folded]; ok {
		return true
	}
	distinct := make(map[rune]struct{})
	for _, r := range folded {
		distinct[r] = struct{}{}
	}
	return len(distinct) <= maxRepeatedDistinct
}

// CheckPassword enforces the policy on a password being created or changed: ErrWeakPassword for
// the length and character rules, then ErrCommonPassword. Never call it on a password being
// verified: login and step-up only compare, so accounts from before this policy keep working.
func CheckPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen || len(pw) > MaxPasswordLen || !hasCharacterClasses(pw) {
		return ErrWeakPassword
	}
	if commonOrRepeated(pw) {
		return ErrCommonPassword
	}
	return nil
}

var b64 = base64.RawStdEncoding

// HashPassword returns a PHC-format argon2id hash: $argon2id$v=19$m=..,t=..,p=..$salt$key.
func (h *Hasher) HashPassword(ctx context.Context, pw string, p Params) (string, error) {
	if len(pw) > MaxPasswordLen {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := h.idKey(ctx, []byte(pw), salt, p, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the hash, in time independent of where they differ.
// ErrBusy when no hashing slot freed up in time.
func (h *Hasher) VerifyPassword(ctx context.Context, hash, pw string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrMalformedHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil ||
		p.MemoryKiB < 8*1024 || p.MemoryKiB > 1024*1024 || p.Time < 1 || p.Time > 16 || p.Threads < 1 {
		return false, ErrMalformedHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, ErrMalformedHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 {
		return false, ErrMalformedHash
	}
	if len(pw) > MaxPasswordLen {
		return false, nil
	}
	got, err := h.idKey(ctx, []byte(pw), salt, p, uint32(len(want)))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when the email is unknown, so a failed login takes as long
// whether or not the account exists. Its key matches no password; only its cost matters, so it
// is built rather than hashed (a hash could be refused with ErrBusy).
var dummyHash = func() string {
	p := DefaultParams
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		b64.EncodeToString([]byte("prismaport-dummy")), b64.EncodeToString(make([]byte, keyLen)))
}()

// BurnPasswordCheck spends the time of one password verification. It returns ErrBusy like a
// real check would, so a caller answers an unknown email the same as a known one under load.
func (h *Hasher) BurnPasswordCheck(ctx context.Context, pw string) error {
	_, err := h.VerifyPassword(ctx, dummyHash, pw)
	return err
}
