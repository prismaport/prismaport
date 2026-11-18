package apikey

import (
	"strings"
	"testing"
)

func TestGenerateParse(t *testing.T) {
	for _, live := range []bool{true, false} {
		g, err := Generate(live)
		if err != nil {
			t.Fatal(err)
		}
		gotLive, hash, err := Parse(g.Plaintext)
		if err != nil || gotLive != live || hash != g.Hash {
			t.Fatalf("parse %s: live=%v err=%v", g.Plaintext, gotLive, err)
		}
		if !strings.HasPrefix(g.Plaintext, g.Prefix) || len(g.Prefix) != 12 {
			t.Fatalf("prefix %q", g.Prefix)
		}
		other, _ := Generate(live)
		if other.Hash == g.Hash {
			t.Fatal("two keys share a hash")
		}
	}
}

func TestParseRejects(t *testing.T) {
	g, _ := Generate(true)
	for _, bad := range []string{
		"", "sk_live_", "pk_live_abc", "sk_live_0OIl", g.Plaintext + "!",
		"Bearer " + g.Plaintext, strings.ToUpper(g.Plaintext),
	} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestScopes(t *testing.T) {
	if !ValidScope("sessions:write") || ValidScope("sessions:delete") {
		t.Fatal("scope validation")
	}
}
