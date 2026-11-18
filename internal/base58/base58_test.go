package base58

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

func TestKnown(t *testing.T) {
	cases := map[string][]byte{
		"11111111111111111111111111111111": make([]byte, 32),
		"StV1DL6CwTryKyV":                  []byte("hello world"),
		"1112":                             {0, 0, 0, 1},
	}
	for s, b := range cases {
		t.Run(s, func(t *testing.T) {
			if got := Encode(b); got != s {
				t.Errorf("Encode(%x)=%s want %s", b, got, s)
			}
			got, err := Decode(s)
			if err != nil || !bytes.Equal(got, b) {
				t.Errorf("Decode(%s)=%x,%v", s, got, err)
			}
		})
	}
	for _, bad := range []string{"", "0", "O", "I", "l", "abc+"} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("Decode(%q) accepted", bad)
		}
	}
}

func TestPropertyRoundtrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(t, "b")
		got, err := Decode(Encode(b))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("roundtrip %x -> %x (%v)", b, got, err)
		}
	})
}
