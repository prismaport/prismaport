package dashauth

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"hash"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B: the seeds are the ASCII digits repeated to the hash's block of 20, 32
// and 64 bytes.
func TestTOTPRFC6238Vectors(t *testing.T) {
	seeds := map[string][]byte{
		"SHA1":   []byte("12345678901234567890"),
		"SHA256": []byte("12345678901234567890123456789012"),
		"SHA512": []byte("1234567890123456789012345678901234567890123456789012345678901234"),
	}
	hashes := map[string]func() hash.Hash{"SHA1": sha1.New, "SHA256": sha256.New, "SHA512": sha512.New}
	vectors := []struct {
		unix int64
		alg  string
		code string
	}{
		{59, "SHA1", "94287082"}, {59, "SHA256", "46119246"}, {59, "SHA512", "90693936"},
		{1111111109, "SHA1", "07081804"}, {1111111109, "SHA256", "68084774"}, {1111111109, "SHA512", "25091201"},
		{1111111111, "SHA1", "14050471"}, {1111111111, "SHA256", "67062674"}, {1111111111, "SHA512", "99943326"},
		{1234567890, "SHA1", "89005924"}, {1234567890, "SHA256", "91819424"}, {1234567890, "SHA512", "93441116"},
		{2000000000, "SHA1", "69279037"}, {2000000000, "SHA256", "90698825"}, {2000000000, "SHA512", "38618901"},
		{20000000000, "SHA1", "65353130"}, {20000000000, "SHA256", "77737706"}, {20000000000, "SHA512", "47863826"},
	}
	for _, v := range vectors {
		t.Run(v.code, func(t *testing.T) {
			if got := TOTP(seeds[v.alg], time.Unix(v.unix, 0), 30, 8, hashes[v.alg]); got != v.code {
				t.Errorf("%s at %d: %s, want %s", v.alg, v.unix, got, v.code)
			}
		})
	}
}

// RFC 4226 Appendix D.
func TestHOTPRFC4226Vectors(t *testing.T) {
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for i, w := range want {
		if got := HOTP([]byte("12345678901234567890"), uint64(i), 6, sha1.New); got != w {
			t.Errorf("counter %d: %s, want %s", i, got, w)
		}
	}
}

func TestVerifyTOTP(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	step := TOTPStep(now, TOTPPeriod)
	code := TOTP(secret, now, TOTPPeriod, TOTPDigits, sha1.New)
	if got, ok := VerifyTOTP(secret, code, now, 0); !ok || got != step {
		t.Fatalf("current code: %d %v", got, ok)
	}
	if _, ok := VerifyTOTP(secret, code, now, step); ok {
		t.Fatal("a used step must not be accepted again")
	}
	if got, ok := VerifyTOTP(secret, code, now.Add(30*time.Second), 0); !ok || got != step {
		t.Fatalf("previous step within skew: %d %v", got, ok)
	}
	if _, ok := VerifyTOTP(secret, code, now.Add(90*time.Second), 0); ok {
		t.Fatal("a code two steps old must fail")
	}
	next := TOTP(secret, now.Add(30*time.Second), TOTPPeriod, TOTPDigits, sha1.New)
	if got, ok := VerifyTOTP(secret, next, now, step); !ok || got != step+1 {
		t.Fatalf("next step within skew after the current one was used: %d %v", got, ok)
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", " 12345", "０１２３４５"} {
		if _, ok := VerifyTOTP(secret, bad, now, 0); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, ok := VerifyTOTP([]byte("another secret 12345"), code, now, 0); ok {
		t.Fatal("code accepted for another secret")
	}
}

func TestOTPAuthURL(t *testing.T) {
	secret := []byte("12345678901234567890")
	u, err := url.Parse(OTPAuthURL("Prismaport", "owner@example.com", secret))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Prismaport:owner@example.com" ||
		q.Get("secret") != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" || q.Get("issuer") != "Prismaport" ||
		q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Fatalf("%s", u)
	}
	s, err := NewTOTPSecret()
	if err != nil || len(s) != 20 {
		t.Fatalf("secret %x %v", s, err)
	}
}

func TestPasswordHash(t *testing.T) {
	p := Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1}
	hs := NewHasher(0, 0)
	h, err := hs.HashPassword(t.Context(), "correct horse battery staple", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("hash format: %s", h)
	}
	if ok, err := hs.VerifyPassword(t.Context(), h, "correct horse battery staple"); !ok || err != nil {
		t.Fatalf("verify: %v %v", ok, err)
	}
	if ok, _ := hs.VerifyPassword(t.Context(), h, "correct horse battery stapl"); ok {
		t.Fatal("wrong password accepted")
	}
	if h2, _ := hs.HashPassword(t.Context(), "correct horse battery staple", p); h2 == h {
		t.Fatal("hashes must be salted")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=18$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=8192,t=1,p=1$!!$a2V5a2V5a2V5a2V5a2V5a2V5"} {
		if _, err := hs.VerifyPassword(t.Context(), bad, "x"); err == nil {
			t.Errorf("malformed hash accepted: %q", bad)
		}
	}
	if ok, err := hs.VerifyPassword(t.Context(), h, strings.Repeat("a", MaxPasswordLen+1)); ok || err != nil {
		t.Fatalf("over-long password: %v %v", ok, err)
	}
	if def, _ := hs.HashPassword(t.Context(), "x", DefaultParams); !strings.Contains(def, "m=65536,t=2,p=1") {
		t.Fatalf("default params: %s", def)
	}
	if err := hs.BurnPasswordCheck(t.Context(), "anything"); err != nil {
		t.Fatal(err)
	}
}

func TestTokens(t *testing.T) {
	tok, h, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashToken(tok)
	if err != nil || h2 != h {
		t.Fatalf("hash token: %v", err)
	}
	other, _, _ := NewToken()
	if tok == other {
		t.Fatal("tokens repeat")
	}
	for _, bad := range []string{"", "abc", tok + "A", tok[:len(tok)-1] + "!", strings.Repeat("A", 44)} {
		if _, err := HashToken(bad); err == nil {
			t.Errorf("malformed token accepted: %q", bad)
		}
	}
	if !EqualTokens(tok, tok) || EqualTokens(tok, other) || EqualTokens("", "") {
		t.Fatal("EqualTokens")
	}
}

// TestHashSlots: with every hashing slot taken, a hash, a check and the unknown-email burn wait
// the Hasher's wait and then give up with ErrBusy instead of allocating another 64 MiB; a freed
// slot lets them run again.
func TestHashSlots(t *testing.T) {
	p := Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1}
	hs := NewHasher(2, 20*time.Millisecond)
	h, err := hs.HashPassword(t.Context(), "correct horse battery staple", p)
	if err != nil {
		t.Fatal(err)
	}
	hashSlots := hs.slots
	if cap(hashSlots) != 2 {
		t.Fatalf("slots: %d", cap(hashSlots))
	}
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	if _, err := hs.HashPassword(t.Context(), "correct horse battery staple", p); err != ErrBusy {
		t.Fatalf("hash with every slot taken: %v", err)
	}
	if ok, err := hs.VerifyPassword(t.Context(), h, "correct horse battery staple"); ok || err != ErrBusy {
		t.Fatalf("verify with every slot taken: %v %v", ok, err)
	}
	if err := hs.BurnPasswordCheck(t.Context(), "anything"); err != ErrBusy {
		t.Fatalf("burn with every slot taken: %v", err)
	}
	<-hashSlots
	if ok, err := hs.VerifyPassword(t.Context(), h, "correct horse battery staple"); !ok || err != nil {
		t.Fatalf("verify with a free slot: %v %v", ok, err)
	}
	for range cap(hashSlots) - 1 {
		<-hashSlots
	}
	if len(hashSlots) != 0 {
		t.Fatalf("slots left taken: %d", len(hashSlots))
	}
}

// A request that ends while every hashing slot is taken stops waiting at once, instead of holding
// on for the Hasher's wait.
func TestHashWaitEndsWithTheRequest(t *testing.T) {
	p := Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1}
	hs := NewHasher(0, time.Hour)
	hashSlots := hs.slots
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	defer func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := hs.HashPassword(ctx, "correct horse battery staple", p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hash: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("waited %s", took)
	}
}

// A Hasher made without settings has a slot per core and waits DefaultHashWait.
func TestHasherDefaults(t *testing.T) {
	hs := NewHasher(0, 0)
	if cap(hs.slots) != max(1, runtime.GOMAXPROCS(0)) || hs.wait != DefaultHashWait {
		t.Fatalf("defaults: %d slots, wait %s", cap(hs.slots), hs.wait)
	}
}
