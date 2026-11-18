package service

import (
	"regexp"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/secretbox"
)

func recoveryService(t *testing.T) *Service {
	t.Helper()
	box, err := secretbox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{cfg: Config{SecretBox: box}}
}

func TestRecoveryCodeShape(t *testing.T) {
	s := recoveryService(t)
	format := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}-[0-9A-HJKMNP-TV-Z]{5}$`)
	codes, hashes, err := s.newRecoveryCodes("usr_1")
	if err != nil || len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("%v %d %d", err, len(codes), len(hashes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if !format.MatchString(c) {
			t.Fatalf("code %q", c)
		}
		if seen[c] {
			t.Fatalf("repeated code %q", c)
		}
		seen[c] = true
		if string(hashes[i]) != string(s.recoveryCodeHash("usr_1", normalizeRecoveryCode(c))) {
			t.Fatalf("hash of %q does not match its normalized form", c)
		}
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	for in, want := range map[string]string{
		"ABCDE-12345":   "ABCDE12345",
		"abcde12345":    "ABCDE12345",
		" abcde-12345 ": "ABCDE12345",
		"oOiIlL-11111":  "001111" + "11111",
		"":              "",
	} {
		t.Run(in, func(t *testing.T) {
			if got := normalizeRecoveryCode(in); got != want {
				t.Errorf("%q: %q, want %q", in, got, want)
			}
		})
	}
}

// The hash is keyed to the user, so one user's code never matches another's row.
func TestRecoveryCodeHashPerUser(t *testing.T) {
	s := recoveryService(t)
	if string(s.recoveryCodeHash("usr_1", "ABCDE12345")) == string(s.recoveryCodeHash("usr_2", "ABCDE12345")) {
		t.Fatal("hash does not depend on the user")
	}
}

// The token is stable for a removal, so the worker and a repeated request can name the link the
// first email carried, and it has the shape the cancel route accepts.
func TestRemovalCancelTokenStable(t *testing.T) {
	s := recoveryService(t)
	at := time.Date(2026, 11, 9, 12, 0, 0, 123456000, time.UTC)
	a, b := s.removalCancelToken("usr_1", at), s.removalCancelToken("usr_1", at.In(time.FixedZone("x", 3600)))
	if a != b {
		t.Fatalf("depends on the zone: %q %q", a, b)
	}
	if a == s.removalCancelToken("usr_2", at) || a == s.removalCancelToken("usr_1", at.Add(time.Microsecond)) {
		t.Fatal("token does not depend on the user and the moment")
	}
	hash, err := dashauth.HashToken(a)
	if err != nil {
		t.Fatalf("token %q: %v", a, err)
	}
	if string(hash[:]) != string(s.removalCancelHash("usr_1", at)) {
		t.Fatal("stored hash differs from the lookup hash")
	}
}
