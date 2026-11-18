package evm

import (
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Vectors below were produced with Foundry's cast (v1.8.1) from anvil's first dev key, so they
// check this package against an independent implementation byte for byte.
const devKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func mustKey(t *testing.T) *PrivateKey {
	t.Helper()
	k, err := ParsePrivateKey(devKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyAddressAndRedaction(t *testing.T) {
	k := mustKey(t)
	if got := k.Address().Hex(); got != "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" {
		t.Fatalf("address %s", got)
	}
	for _, s := range []string{k.String(), k.GoString()} {
		if strings.Contains(s, "ac0974") {
			t.Fatal("key material in string form")
		}
	}
	for _, bad := range []string{"", "0x12", strings.Repeat("0", 64), strings.Repeat("f", 64), strings.Repeat("z", 64)} {
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Fatalf("accepted bad key %q", bad)
		}
	}
}

func TestLoadPrivateKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gas.key")
	if err := os.WriteFile(p, []byte(devKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadPrivateKey(p, "")
	if err != nil || k.Address() != mustKey(t).Address() {
		t.Fatalf("file key: %v", err)
	}
	if k, err = LoadPrivateKey("", devKey); err != nil || k.Address() != mustKey(t).Address() {
		t.Fatalf("env key: %v", err)
	}
	if _, err := LoadPrivateKey("", ""); err == nil {
		t.Fatal("empty config accepted")
	}
	_, err = LoadPrivateKey(filepath.Join(dir, "missing"), "")
	if err == nil {
		t.Fatal("missing file accepted")
	}
	if err := os.WriteFile(p, []byte("not a key "+devKey[:10]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(p, ""); err == nil || strings.Contains(err.Error(), devKey[2:10]) {
		t.Fatalf("bad key error leaks or passes: %v", err)
	}
}

func TestSignDynamicFeeTxMatchesCast(t *testing.T) {
	k := mustKey(t)
	to, _ := ParseAddress("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	tx := DynamicFeeTx{ChainID: 8453, Nonce: 7, GasTipCap: big.NewInt(1_000_000), GasFeeCap: big.NewInt(2_000_000),
		Gas: 100_000, To: &to, Value: new(big.Int), Data: []byte{0xde, 0xad, 0xbe, 0xef}}
	raw, hash := tx.Sign(k)
	want := "02f86f82210507830f4240831e8480830186a0945aaeb6053f3e94c9b9a09f33669435e7ef1beaed8084deadbeefc080a07ec6e510af8511367c78ba2a90cb93767a1d9bee735c7c5dfc469d29121ccf07a02d38fdc0f11cb2864e7cd8ff772e09b6f2518a7c98291a66dc60ae894173112e"
	if hex.EncodeToString(raw) != want {
		t.Fatalf("raw tx\n got %x\nwant %s", raw, want)
	}
	wantRaw, _ := hex.DecodeString(want)
	if hash.Hex() != "0x"+hex.EncodeToString(Keccak256(wantRaw)) {
		t.Fatal("tx hash is not keccak of the raw tx")
	}

	create := DynamicFeeTx{ChainID: 31337, Nonce: 0, GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(1_000_000_000),
		Gas: 3_000_000, Value: big.NewInt(1), Data: []byte{0x60, 0x00}}
	raw, _ = create.Sign(k)
	want = "02f857827a698080843b9aca00832dc6c08001826000c001a08f3429e7615e679fb81dc693a8ae3a614b56f0fafe74517f16cb3b23248995d4a0744dea0b94ca56693ac9b197e2323ed523d7c3bbf5f9424a0c1950cb318f805a"
	if hex.EncodeToString(raw) != want {
		t.Fatalf("create tx\n got %x\nwant %s", raw, want)
	}
}

func TestRLPLongForms(t *testing.T) {
	long := make([]byte, 60)
	enc := rlpBytes(long)
	if enc[0] != 0xb8 || enc[1] != 60 || len(enc) != 62 {
		t.Fatalf("long string header %x", enc[:2])
	}
	if got := rlpBytes([]byte{0x7f}); len(got) != 1 || got[0] != 0x7f {
		t.Fatal("single low byte must encode as itself")
	}
	if got := rlpBytes([]byte{0x80}); hex.EncodeToString(got) != "8180" {
		t.Fatalf("0x80 -> %x", got)
	}
	big := make([]byte, 300)
	if enc := rlpBytes(big); enc[0] != 0xb9 || enc[1] != 0x01 || enc[2] != 0x2c {
		t.Fatalf("two-byte length header %x", enc[:3])
	}
	if l := rlpList(long); l[0] != 0xf8 || l[1] != 60 {
		t.Fatalf("long list header %x", l[:2])
	}
}

func TestCreateAddressVectors(t *testing.T) {
	cases := []struct {
		deployer string
		nonce    uint64
		want     string
	}{
		{"0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083", 1, "0x43aE8FF66EB0F12eA2eBB04e59D0c0cD955ed21E"},
		{"0x7f1b1F5Bb2fd4D9E2a8a2Be1F4f8C0A0C3B8e2d1", 0, "0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083"},
		{"0x00000000000000000000000000000000000c0ffe", 7, "0x82DA78CfF4126C26c51AD1f5E80b3c29Da6E36FC"},
		{"0x82DA78CfF4126C26c51AD1f5E80b3c29Da6E36FC", 1, "0x98eFba31dea23BbB2F0317a0bb5081fF607B3262"},
	}
	for _, c := range cases {
		t.Run(c.deployer, func(t *testing.T) {
			d, err := ParseAddress(c.deployer)
			if err != nil {
				t.Fatal(err)
			}
			if got := CreateAddress(d, c.nonce).Hex(); got != c.want {
				t.Fatalf("CREATE(%s, %d) = %s, want %s", c.deployer, c.nonce, got, c.want)
			}
		})
	}
}

func usdcDomain() Domain {
	usdc, _ := ParseAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	return Domain{Name: "USD Coin", Version: "2", ChainID: 8453, VerifyingContract: usdc}
}

func TestReceiveAuthorizationSignatureMatchesCast(t *testing.T) {
	k := mustKey(t)
	to, _ := ParseAddress("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	nonce, _ := ParseWord("0x1284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a")
	auth := ReceiveAuthorization{From: k.Address(), To: to, Value: big.NewInt(25_000_000), ValidAfter: new(big.Int),
		ValidBefore: big.NewInt(1_800_003_600), Nonce: nonce}
	digest := auth.Digest(usdcDomain())
	v, r, s := k.SignHash(digest)
	sig := hex.EncodeToString(append(append(r[:], s[:]...), v+27))
	want := "f04954557b3da08be80ec8ad52e06775340e346154df874eceff5dac887948166a713023e3fd593a6a2a42b284773d3b8581ed77023c64e47c3e31dfae8e70441c"
	if sig != want {
		t.Fatalf("typed-data signature\n got %s\nwant %s", sig, want)
	}
	raw, _ := hex.DecodeString(want)
	sv, sr, ss, err := SplitSignature(raw)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := RecoverAddress(digest, sv, sr, ss)
	if err != nil || signer != k.Address() {
		t.Fatalf("recovered %s: %v", signer.Hex(), err)
	}
	tampered := auth
	tampered.Value = big.NewInt(25_000_001)
	if other, err := RecoverAddress(tampered.Digest(usdcDomain()), sv, sr, ss); err == nil && other == k.Address() {
		t.Fatal("tampered value still recovers the payer")
	}
}

func TestRecoverRejectsMalleableAndMalformed(t *testing.T) {
	k := mustKey(t)
	var digest Word
	digest[0] = 1
	v, r, s := k.SignHash(digest)
	highS := BigWord(new(big.Int).Sub(new(big.Int).Add(new(big.Int).Lsh(halfN, 1), big.NewInt(1)), s.Big()))
	if _, err := RecoverAddress(digest, v+27, r, highS); err == nil {
		t.Fatal("high-s signature accepted")
	}
	if _, err := RecoverAddress(digest, 29, r, s); err == nil {
		t.Fatal("bad v accepted")
	}
	if _, err := RecoverAddress(digest, 27, Word{}, s); err == nil {
		t.Fatal("zero r accepted")
	}
	if _, _, _, err := SplitSignature(make([]byte, 64)); err == nil {
		t.Fatal("64-byte signature accepted")
	}
	bad := make([]byte, 65)
	bad[64] = 30
	if _, _, _, err := SplitSignature(bad); err == nil {
		t.Fatal("v=30 accepted")
	}
}

func TestSignRecoverProperty(t *testing.T) {
	k := mustKey(t)
	rapid.Check(t, func(t *rapid.T) {
		var d Word
		copy(d[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "digest"))
		v, r, s := k.SignHash(d)
		if s.Big().Cmp(halfN) > 0 {
			t.Fatal("high s produced")
		}
		got, err := RecoverAddress(d, v, r, s)
		if err != nil || got != k.Address() {
			t.Fatalf("roundtrip failed: %v", err)
		}
	})
}

// Expected nonces from `cast keccak $(cast abi-encode "f(uint256,address,bytes32,address,uint16,uint64,address[],uint16[])" ...)`
// (Foundry v1.8.1); the first is the worked example of protocol.md, pinned by the Gateway tests.
func TestGatewayNonceMatchesCast(t *testing.T) {
	addr := func(s string) Address {
		t.Helper()
		a, err := ParseAddress(s)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	salt, _ := ParseWord("0x1284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a")
	tre := addr("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	a, b := addr("0x8ba1f109551bD432803012645Ac136ddd64DBA72"), addr("0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359")
	c, max := addr("0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB"), addr("0xffffffffffffffffffffffffffffffffffffffff")
	one := addr("0x0000000000000000000000000000000000000001")
	gw := addr("0x1111111111111111111111111111111111111111")
	for _, v := range []struct {
		chainID    uint64
		gateway    Address
		feeBps     uint16
		fixedFee   uint64
		recipients []Address
		bps        []uint16
		want       string
	}{
		{8453, addr("0x0000000000000000000000000000000000008453"), 100, 100_000, []Address{a, b}, []uint16{7000, 3000},
			"0xcc9446464067035be62382e52f7a2f37a1f8d39295bb41efb33e7a83c5c1cc51"},
		{8453, gw, 100, 100_000, []Address{a}, []uint16{10_000}, "0xdf0cce6e9f091b07a9b3f066db4576d3d7bc759f7006c9ade6a62574c7088ae0"},
		{84532, gw, 300, 5_000_000, []Address{one, a, b, c, max}, []uint16{1, 2499, 2500, 2500, 2500},
			"0x7667a7dc38c5c780d1ff92b56389b93aad4240cf4405fc47fe77acf16e7964af"},
	} {
		t.Run(v.want, func(t *testing.T) {
			got := GatewayNonce(v.chainID, v.gateway, salt, tre, v.feeBps, v.fixedFee, v.recipients, v.bps)
			if got.Hex() != v.want {
				t.Fatalf("nonce %s, want %s", got.Hex(), v.want)
			}
		})
	}
	base := GatewayNonce(8453, gw, salt, tre, 100, 100_000, []Address{a, b}, []uint16{7000, 3000})
	for name, other := range map[string]Word{
		"chain id":  GatewayNonce(8454, gw, salt, tre, 100, 100_000, []Address{a, b}, []uint16{7000, 3000}),
		"recipient": GatewayNonce(8453, gw, salt, tre, 100, 100_000, []Address{a, c}, []uint16{7000, 3000}),
		"share":     GatewayNonce(8453, gw, salt, tre, 100, 100_000, []Address{a, b}, []uint16{6000, 4000}),
		"order":     GatewayNonce(8453, gw, salt, tre, 100, 100_000, []Address{b, a}, []uint16{3000, 7000}),
		"count":     GatewayNonce(8453, gw, salt, tre, 100, 100_000, []Address{a, b, c}, []uint16{7000, 2000, 1000}),
		"treasury":  GatewayNonce(8453, gw, salt, a, 100, 100_000, []Address{a, b}, []uint16{7000, 3000}),
		"fee":       GatewayNonce(8453, gw, salt, tre, 101, 100_000, []Address{a, b}, []uint16{7000, 3000}),
		"fixed fee": GatewayNonce(8453, gw, salt, tre, 100, 100_001, []Address{a, b}, []uint16{7000, 3000}),
		"session":   GatewayNonce(8453, gw, Word{}, tre, 100, 100_000, []Address{a, b}, []uint16{7000, 3000}),
	} {
		t.Run(name, func(t *testing.T) {
			if other == base {
				t.Errorf("%s not bound in the nonce", name)
			}
		})
	}
}

func TestABIHelpers(t *testing.T) {
	if hex.EncodeToString(func() []byte { s := Selector("transfer(address,uint256)"); return s[:] }()) != "a9059cbb" {
		t.Fatal("selector")
	}
	if Topic("Transfer(address,address,uint256)").Hex() != "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef" {
		t.Fatal("transfer topic")
	}
	a, _ := ParseAddress("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	w := AddressWord(a)
	if back, ok := w.Address(); !ok || back != a {
		t.Fatal("address word roundtrip")
	}
	w[0] = 1
	if _, ok := w.Address(); ok {
		t.Fatal("dirty high bytes accepted as address")
	}
	if _, err := Words(make([]byte, 33)); err == nil {
		t.Fatal("ragged data accepted")
	}
	if _, err := ParseWord("0x12"); err == nil {
		t.Fatal("short word accepted")
	}
	if _, err := ParseHexBytes("12"); err == nil {
		t.Fatal("missing prefix accepted")
	}
	if b, err := ParseHexBytes("0x"); err != nil || len(b) != 0 {
		t.Fatal("empty hex")
	}
	if a.Lower() != "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed" {
		t.Fatal("lower")
	}
	if l, err := ParseAddressLoose("0X5AAEB6053F3E94C9B9A09F33669435E7EF1BEAED"); err != nil || l != a {
		t.Fatalf("loose parse: %v", err)
	}
	arr := EncodeAddressArray([]Address{a, a})
	if len(arr) != 3 || arr[0].Big().Int64() != 2 {
		t.Fatal("address array")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("negative word did not panic")
		}
	}()
	BigWord(big.NewInt(-1))
}
