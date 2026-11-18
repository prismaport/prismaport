package dashauth

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestCheckPassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		want error
	}{
		{"accepted", "Tr4vel-the-river", nil},
		{"exactly 8 runes", "Xk7$mvqz", nil},
		{"7 runes", "Xk7$mvq", ErrWeakPassword},
		{"empty", "", ErrWeakPassword},
		{"runes are counted, not bytes", "Çã1!é", ErrWeakPassword},
		{"8 runes in more bytes", "Çãé1!Ñõx", nil},
		{"exactly 256 bytes", "Aa1!xkv7" + strings.Repeat("b", 248), nil},
		{"257 bytes", "Aa1!xkv7" + strings.Repeat("b", 249), ErrWeakPassword},
		{"256 runes over 256 bytes", "Aa1!xkv7" + strings.Repeat("é", 248), ErrWeakPassword},
		{"no uppercase", "tr4vel-the-river", ErrWeakPassword},
		{"no lowercase", "TR4VEL-THE-RIVER", ErrWeakPassword},
		{"no number", "Travel-the-river", ErrWeakPassword},
		{"no special", "Tr4velTheRiver", ErrWeakPassword},
		{"a space is special", "Tr4vel the river", nil},
		{"unicode classes", "Ñandú ٣ río", nil},
		{"a non-ascii number counts", "Tr-velٹher٣iver", nil},
		{"common", "Password123!", ErrCommonPassword},
		{"common in another case", "pASSWORD123!", ErrCommonPassword},
		{"common, brand", "PrismaporT1!", ErrCommonPassword},
		{"common, portuguese", "Senha123!", ErrCommonPassword},
		{"three distinct after folding", "Aa1!Aa1!", ErrCommonPassword},
		{"repeated", "Aa1!aA1!", ErrCommonPassword},
		{"not common or repeated", "Aa1!Bb2@", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckPassword(tc.pw); !errors.Is(err, tc.want) {
				t.Fatalf("CheckPassword(%q) = %v, want %v", tc.pw, err, tc.want)
			}
		})
	}
}

func TestRepeatedPassesTheCharacterRules(t *testing.T) {
	// Four distinct characters is the first that is allowed; one fewer is refused.
	if err := CheckPassword("Aa1!Aa1!Aa1!Bb"); err != nil {
		t.Fatalf("four distinct: %v", err)
	}
	if !commonOrRepeated("aA1!aA1!") || !commonOrRepeated("AAaa11!!") {
		t.Fatal("three distinct characters after folding were not refused")
	}
	if commonOrRepeated("Aa1!Bb") {
		t.Fatal("four distinct characters were refused")
	}
}

// commonListEntries are the lines of the embedded list.
func commonListEntries() []string {
	var out []string
	for _, line := range strings.Split(commonPasswordList, "\n") {
		if pw := strings.TrimSpace(line); pw != "" {
			out = append(out, pw)
		}
	}
	return out
}

// An entry that fails the character rules is dead weight: rule 2 already refuses it.
func TestCommonListOnlyHoldsWhatTheRulesAccept(t *testing.T) {
	entries := commonListEntries()
	if len(entries) < 20 {
		t.Fatalf("%d entries", len(entries))
	}
	for _, pw := range entries {
		if err := CheckPassword(pw); !errors.Is(err, ErrCommonPassword) {
			t.Errorf("%q: %v", pw, err)
		}
	}
}

// The dashboard mirrors the list for its checklist; the server is the authority.
func TestCommonListMatchesTheDashboard(t *testing.T) {
	raw, err := os.ReadFile("../../web/dashboard/src/lib/commonPasswords.json")
	if err != nil {
		t.Fatal(err)
	}
	var web []string
	if err := json.Unmarshal(raw, &web); err != nil {
		t.Fatal(err)
	}
	server := commonListEntries()
	slices.Sort(web)
	slices.Sort(server)
	if !slices.Equal(web, server) {
		t.Fatalf("the lists differ:\nserver %v\nweb    %v", server, web)
	}
}

func TestVerifyIsNotBoundByTheNewPasswordRules(t *testing.T) {
	p := Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1}
	hs := NewHasher(0, 0)
	for _, old := range []string{"short", "no classes at all here", "Password123!", "Aa1!Aa1!", strings.Repeat("a", 256)} {
		h, err := hs.HashPassword(t.Context(), old, p)
		if err != nil {
			t.Fatalf("hash %q: %v", old, err)
		}
		if ok, err := hs.VerifyPassword(t.Context(), h, old); err != nil || !ok {
			t.Fatalf("verify %q: %v %v", old, ok, err)
		}
	}
}
