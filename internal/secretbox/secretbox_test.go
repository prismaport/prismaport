package secretbox

import (
	"bytes"
	"testing"
)

func TestSealOpen(t *testing.T) {
	b, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal([]byte("whsec_x"), []byte("we_1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Open(sealed, []byte("we_1"))
	if err != nil || string(got) != "whsec_x" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := b.Open(sealed, []byte("we_2")); err == nil {
		t.Fatal("opened with the wrong aad")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed, []byte("we_1")); err == nil {
		t.Fatal("opened a tampered ciphertext")
	}
	if _, err := b.Open([]byte{1, 2}, nil); err == nil {
		t.Fatal("opened a truncated ciphertext")
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	again, _ := b.Seal([]byte("whsec_x"), nil)
	if _, err := other.Open(again, nil); err == nil {
		t.Fatal("opened with another key")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := FromHex("zz"); err == nil {
		t.Fatal("bad hex accepted")
	}
}

func TestDeriveKey(t *testing.T) {
	a, _ := New(make([]byte, 32))
	b, _ := New(append(make([]byte, 31), 1))
	k1, k2 := a.DeriveKey("dashboard_email_code"), a.DeriveKey("other")
	if len(k1) != 32 || string(k1) == string(k2) || string(k1) != string(a.DeriveKey("dashboard_email_code")) ||
		string(k1) == string(b.DeriveKey("dashboard_email_code")) {
		t.Fatal("derived keys are not per label and per secret")
	}
}
