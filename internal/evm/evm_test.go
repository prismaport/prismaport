package evm

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Vectors from the EIP-55 specification.
var eip55 = []string{
	"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
	"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
	"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
	"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	"0x52908400098527886E0F7030069857D2E4169EE7",
	"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
	"0xde709f2102306220921060314715629080e2fb77",
	"0x27b1fdb04752bbc536007a920d24acb045561c26",
}

func TestEIP55(t *testing.T) {
	for _, v := range eip55 {
		a, err := ParseAddress(v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if a.Hex() != v {
			t.Errorf("Hex()=%s want %s", a.Hex(), v)
		}
		lower := "0x" + strings.ToLower(v[2:])
		if b, err := ParseAddress(lower); err != nil || b != a {
			t.Errorf("lowercase %s rejected: %v", lower, err)
		}
	}
	bad := "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD"
	if _, err := ParseAddress(bad); !errors.Is(err, ErrChecksum) {
		t.Errorf("bad checksum accepted: %v", err)
	}
	for _, f := range []string{"", "0x", "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeA", "0xZZAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0X5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"} {
		if _, err := ParseAddress(f); !errors.Is(err, ErrFormat) {
			t.Errorf("%q: %v", f, err)
		}
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		panic(err)
	}
	return b
}

// Vectors from EIP-1014.
func TestCreate2(t *testing.T) {
	cases := []struct{ deployer, salt, init, want string }{
		{"0x0000000000000000000000000000000000000000", "0x0000000000000000000000000000000000000000000000000000000000000000", "0x00", "0x4D1A2e2bB4F88F0250f26Ffff098B0b30B26BF38"},
		{"0xdeadbeef00000000000000000000000000000000", "0x0000000000000000000000000000000000000000000000000000000000000000", "0x00", "0xB928f69Bb1D91Cd65274e3c79d8986362984fDA3"},
		{"0xdeadbeef00000000000000000000000000000000", "0x000000000000000000000000feed000000000000000000000000000000000000", "0x00", "0xD04116cDd17beBE565EB2422F2497E06cC1C9833"},
		{"0x0000000000000000000000000000000000000000", "0x0000000000000000000000000000000000000000000000000000000000000000", "0xdeadbeef", "0x70f2b2914A2a4b783FaEFb75f459A580616Fcb5e"},
		{"0x00000000000000000000000000000000deadbeef", "0x00000000000000000000000000000000000000000000000000000000cafebabe", "0xdeadbeef", "0x60f3f640a8508fC6a86d45DF051962668E1e8AC7"},
		{"0x00000000000000000000000000000000deadbeef", "0x00000000000000000000000000000000000000000000000000000000cafebabe", "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "0x1d8bfDC5D46DC4f61D6b6115972536eBE6A8854C"},
		{"0x0000000000000000000000000000000000000000", "0x0000000000000000000000000000000000000000000000000000000000000000", "0x", "0xE33C0C7F7df4809055C3ebA6c09CFe4BaF1BD9e0"},
	}
	for _, c := range cases {
		t.Run(c.deployer, func(t *testing.T) {
			var d Address
			copy(d[:], mustHex(c.deployer))
			var salt, ih [32]byte
			copy(salt[:], mustHex(c.salt))
			copy(ih[:], Keccak256(mustHex(c.init)))
			if got := Create2Address(d, salt, ih).Hex(); got != c.want {
				t.Errorf("create2(%s,%s,%s)=%s want %s", c.deployer, c.salt, c.init, got, c.want)
			}
		})
	}
}

func TestPropertyChecksumRoundtrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var a Address
		copy(a[:], rapid.SliceOfN(rapid.Byte(), 20, 20).Draw(t, "a"))
		b, err := ParseAddress(a.Hex())
		if err != nil || b != a {
			t.Fatalf("roundtrip %x: %v", a, err)
		}
		u, err := ParseAddress("0x" + strings.ToUpper(a.Hex()[2:]))
		if err != nil || u != a {
			t.Fatalf("uppercase %x: %v", a, err)
		}
	})
}
