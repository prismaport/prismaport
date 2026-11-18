package solana

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/solanaderive"
)

func key(b byte) PublicKey { return PublicKey(bytes.Repeat([]byte{b}, 32)) }

func TestShortVec(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 255, 16383, 16384, 65535} {
		b := appendShortVec(nil, n)
		got, k, err := readShortVec(b)
		if err != nil || got != n || k != len(b) {
			t.Fatalf("%d: got %d (%d bytes) %v", n, got, k, err)
		}
	}
	if b := appendShortVec(nil, 0x80); !bytes.Equal(b, []byte{0x80, 0x01}) {
		t.Fatalf("0x80 encodes as %x", b)
	}
	if _, _, err := readShortVec([]byte{0x80, 0x00}); err == nil {
		t.Fatal("non-canonical length accepted")
	}
}

func TestCompileOrdersAccountsAndRoundTrips(t *testing.T) {
	payer, other, prog := key(1), key(2), key(3)
	ixs := []Instruction{
		{Program: prog, Accounts: []AccountMeta{R(key(4)), W(key(5)), {Key: other, Signer: true}, R(payer)}, Data: []byte{9}},
		SystemTransfer(payer, key(5), 7),
	}
	m, err := Compile(payer, [32]byte{0xaa}, ixs)
	if err != nil {
		t.Fatal(err)
	}
	want := []PublicKey{payer, other, key(5), key(4), prog, SystemProgramID}
	if fmt.Sprint(m.AccountKeys) != fmt.Sprint(want) {
		t.Fatalf("order %v", m.AccountKeys)
	}
	if m.Header != (Header{RequiredSignatures: 2, ReadonlySigned: 1, ReadonlyUnsigned: 3}) {
		t.Fatalf("header %+v", m.Header)
	}
	for i, w := range []bool{true, false, true, false, false, false} {
		if m.IsWritable(i) != w {
			t.Fatalf("writable[%d] = %v", i, !w)
		}
	}
	if !bytes.Equal(m.Instructions[0].Accounts, []uint8{3, 2, 1, 0}) || m.Instructions[0].ProgramIndex != 4 {
		t.Fatalf("compiled %+v", m.Instructions[0])
	}
	back, err := ParseMessage(m.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Serialize(), m.Serialize()) {
		t.Fatal("message round trip differs")
	}
	if _, err := ParseMessage(append(m.Serialize(), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	bad := m.Serialize()
	bad[0] |= 0x80
	if _, err := ParseMessage(bad); err == nil {
		t.Fatal("versioned prefix accepted")
	}
}

// RFC 8032 test vector 1.
func TestKeypairRFC8032AndRedaction(t *testing.T) {
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	k, err := KeypairFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if pk := k.PublicKey(); hex.EncodeToString(pk[:]) != "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a" {
		t.Fatalf("pub %x", k.PublicKey())
	}
	sig := k.Sign(nil)
	if hex.EncodeToString(sig[:]) != "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b" {
		t.Fatalf("sig %x", sig)
	}
	back, err := ParseKeypair(k.JSON())
	if err != nil || back.PublicKey() != k.PublicKey() {
		t.Fatalf("json round trip: %v", err)
	}
	secretHex := hex.EncodeToString(seed)
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%+v %#v %s %x %q", k, k, k, k, k), fmt.Sprintf("%v", *k)} {
		if strings.Contains(s, secretHex[:16]) || strings.Contains(s, "157") {
			t.Fatalf("secret leaked: %s", s)
		}
	}
	tampered := strings.Replace(k.JSON(), "[157,", "[158,", 1)
	if _, err := ParseKeypair(tampered); err == nil {
		t.Fatal("mismatched public key accepted")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "k.json")
	_ = os.WriteFile(f, []byte(k.JSON()), 0o600)
	if got, err := LoadKeypair(f, ""); err != nil || got.PublicKey() != k.PublicKey() {
		t.Fatalf("load file: %v", err)
	}
	if _, err := LoadKeypair("", ""); err == nil {
		t.Fatal("empty key accepted")
	}
}

func TestTransactionSignParseAndPartialSlots(t *testing.T) {
	a, _ := KeypairFromSeed(bytes.Repeat([]byte{1}, 32))
	b, _ := KeypairFromSeed(bytes.Repeat([]byte{2}, 32))
	m, err := Compile(a.PublicKey(), [32]byte{1}, []Instruction{TransferChecked(TokenProgramID, key(7), key(8), key(9), b.PublicKey(), 5, 6, key(10))})
	if err != nil {
		t.Fatal(err)
	}
	tx := NewTransaction(m)
	if err := tx.Sign(a); err != nil {
		t.Fatal(err)
	}
	raw, err := tx.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseTransaction(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !back.VerifySignature(a.PublicKey()) || back.VerifySignature(b.PublicKey()) {
		t.Fatal("partial signature state wrong")
	}
	if err := back.Sign(b); err != nil || !back.VerifySignature(b.PublicKey()) || !back.VerifySignature(a.PublicKey()) {
		t.Fatalf("second signer: %v", err)
	}
	if err := back.Sign(&Keypair{pub: key(99)}); err == nil {
		t.Fatal("non-signer signed")
	}
	if back.ID() != tx.ID() || tx.ID() == "" {
		t.Fatal("id changed after the second signature")
	}
	ix := back.Message.Instructions[0]
	if len(ix.Accounts) != 5 || back.Message.AccountKeys[ix.Accounts[4]] != key(10) {
		t.Fatal("reference key not appended")
	}
	if _, err := ParseTransaction(raw[:len(raw)-1]); err == nil {
		t.Fatal("truncated transaction accepted")
	}
}

func TestInstructionData(t *testing.T) {
	if d := SetComputeUnitLimit(100_000).Data; !bytes.Equal(d, []byte{2, 0xa0, 0x86, 0x01, 0}) {
		t.Fatalf("cu limit %x", d)
	}
	if d := SetComputeUnitPrice(1).Data; !bytes.Equal(d, []byte{3, 1, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("cu price %x", d)
	}
	if d := TransferChecked(TokenProgramID, key(1), key(2), key(3), key(4), 25_000_000, 6).Data; hex.EncodeToString(d) != "0c40787d010000000006" {
		t.Fatalf("transferChecked %x", d)
	}
	if d := SystemTransfer(key(1), key(2), 1).Data; hex.EncodeToString(d) != "020000000100000000000000" {
		t.Fatalf("system transfer %x", d)
	}
}

type idlFile struct {
	Address      string `json:"address"`
	Instructions []struct {
		Name          string `json:"name"`
		Discriminator []int  `json:"discriminator"`
		Accounts      []struct {
			Name     string `json:"name"`
			Writable bool   `json:"writable"`
			Signer   bool   `json:"signer"`
			Address  string `json:"address"`
		} `json:"accounts"`
		Args []struct {
			Name string          `json:"name"`
			Type json.RawMessage `json:"type"`
		} `json:"args"`
	} `json:"instructions"`
	Events []struct {
		Name          string `json:"name"`
		Discriminator []int  `json:"discriminator"`
	} `json:"events"`
	Types []struct {
		Name string `json:"name"`
		Type struct {
			Fields []struct {
				Name string          `json:"name"`
				Type json.RawMessage `json:"type"`
			} `json:"fields"`
		} `json:"type"`
	} `json:"types"`
}

func disc(prefix, name string) [8]byte {
	h := sha256.Sum256([]byte(prefix + ":" + name))
	return [8]byte(h[:8])
}

// The instruction table must be exactly the IDL the program build produced.
func TestForwarderTableMatchesIDL(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "solana", "idl", "prismaport_forwarder.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idl idlFile
	if err := json.Unmarshal(raw, &idl); err != nil {
		t.Fatal(err)
	}
	if len(idl.Instructions) != len(ForwarderInstructions) {
		t.Fatalf("IDL has %d instructions, table %d", len(idl.Instructions), len(ForwarderInstructions))
	}
	for _, spec := range ForwarderInstructions {
		found := false
		for _, ix := range idl.Instructions {
			if ix.Name != spec.Name {
				continue
			}
			found = true
			var d [8]byte
			for i, v := range ix.Discriminator {
				d[i] = byte(v)
			}
			if d != spec.Discriminator || d != disc("global", spec.Name) {
				t.Fatalf("%s discriminator %x, table %x", spec.Name, d, spec.Discriminator)
			}
			if len(ix.Accounts) != len(spec.Accounts) {
				t.Fatalf("%s: %d accounts in IDL, %d in table", spec.Name, len(ix.Accounts), len(spec.Accounts))
			}
			for i, a := range ix.Accounts {
				s := spec.Accounts[i]
				if a.Name != s.Name || a.Writable != s.Writable || a.Signer != s.Signer || a.Address != s.Fixed {
					t.Fatalf("%s account %d: IDL %+v, table %+v", spec.Name, i, a, s)
				}
			}
			var args []string
			for _, a := range ix.Args {
				var compact bytes.Buffer
				if err := json.Compact(&compact, a.Type); err != nil {
					t.Fatal(err)
				}
				args = append(args, a.Name+":"+compact.String())
			}
			want := `salt:{"array":["u8",32]},params:{"defined":{"name":"ForwarderParams"}}`
			if spec.Name == "flush" {
				want += `,close_source:"bool"`
			}
			if got := strings.Join(args, ","); got != want {
				t.Fatalf("%s args %s, want %s", spec.Name, got, want)
			}
		}
		if !found {
			t.Fatalf("%s missing from the IDL", spec.Name)
		}
	}
	layouts := map[string]string{
		"ForwarderParams": `treasury:"pubkey",rent_receiver:"pubkey",fee_bps:"u16",fixed_fee_micros:"u64",recipients:{"vec":{"defined":{"name":"Recipient"}}},mints:{"vec":"pubkey"}`,
		"Recipient":       `wallet:"pubkey",bps:"u16"`,
		"Settled":         `forwarder:"pubkey",mint:"pubkey",source:"pubkey",gross:"u64",fee:"u64",net:"u64",treasury:"pubkey"`,
		"Payout":          `forwarder:"pubkey",mint:"pubkey",recipient:"pubkey",amount:"u64"`,
	}
	for _, ty := range idl.Types {
		want, ok := layouts[ty.Name]
		if !ok {
			continue
		}
		delete(layouts, ty.Name)
		var names []string
		for _, f := range ty.Type.Fields {
			var compact bytes.Buffer
			if err := json.Compact(&compact, f.Type); err != nil {
				t.Fatal(err)
			}
			names = append(names, f.Name+":"+compact.String())
		}
		if strings.Join(names, ",") != want {
			t.Fatalf("%s layout changed: %v", ty.Name, names)
		}
	}
	if len(layouts) != 0 {
		t.Fatalf("types missing from the IDL: %v", layouts)
	}
	events := map[string][8]byte{"Settled": SettledEventDiscriminator, "Payout": PayoutEventDiscriminator}
	for _, ev := range idl.Events {
		want, ok := events[ev.Name]
		if !ok {
			continue
		}
		delete(events, ev.Name)
		var d [8]byte
		for i, v := range ev.Discriminator {
			d[i] = byte(v)
		}
		if d != want || d != disc("event", ev.Name) {
			t.Fatalf("%s discriminator %x", ev.Name, d)
		}
	}
	if len(events) != 0 {
		t.Fatalf("events missing from the IDL: %v", events)
	}
	if idl.Address != "3DHLtkMYKFmxLbQRHkHcNPibfaKYJNntQEnJvUvcnPya" {
		t.Logf("IDL address is %s (the placeholder changed)", idl.Address)
	}
}

func TestForwarderInstructionsAndEvents(t *testing.T) {
	program := MustPublicKey("3DHLtkMYKFmxLbQRHkHcNPibfaKYJNntQEnJvUvcnPya")
	recipients := []solanaderive.Recipient{{Wallet: key(7), Bps: 7000}, {Wallet: key(8), Bps: 2000}, {Wallet: key(9), Bps: 1000}}
	mint, other := key(20), key(21)
	p := ForwarderParams{Treasury: key(9), RentReceiver: key(11), Fee: fee.Params{FeeBps: 100, FixedFeeMicros: 100_000}, Recipients: recipients,
		Mints: []PublicKey{mint, other}}
	f, err := NewForwarder(program, [32]byte{5}, p)
	if err != nil {
		t.Fatal(err)
	}
	fl, err := f.Flush(key(1), mint, key(30), true)
	if err != nil {
		t.Fatal(err)
	}
	treATA, _ := AssociatedTokenAddress(p.Treasury, mint)
	if len(fl.Accounts) != len(IxFlush.Accounts)+3 || fl.Accounts[3].Key != key(30) || fl.Accounts[4].Key != p.Treasury ||
		fl.Accounts[5].Key != treATA || fl.Accounts[6].Key != key(11) || !fl.Accounts[6].Writable {
		t.Fatalf("flush accounts %+v", fl.Accounts)
	}
	if len(fl.Data) != 8+32+82+34*3+32*2+1 || [8]byte(fl.Data[:8]) != IxFlush.Discriminator || fl.Data[8] != 5 ||
		fl.Data[len(fl.Data)-1] != 1 || !bytes.Equal(fl.Data[40:len(fl.Data)-1], p.Encode()) {
		t.Fatalf("flush data %x", fl.Data)
	}
	if keep, _ := f.Flush(key(1), other, key(31), false); keep.Data[len(keep.Data)-1] != 0 {
		t.Fatal("close_source false")
	}
	if _, err := f.Flush(key(1), key(22), key(30), true); !errors.Is(err, solanaderive.ErrMintNotAllowed) {
		t.Fatalf("unbound mint: %v", err)
	}
	ct := f.CloseTokenAccount(key(30))
	if len(ct.Accounts) != 4 || ct.Accounts[0].Key != f.Address || ct.Accounts[1].Key != key(30) || ct.Accounts[2].Key != key(11) ||
		!ct.Accounts[2].Writable || !bytes.Equal(ct.Data, append(append(IxCloseTokenAccount.Discriminator[:], 5), fl.Data[9:len(fl.Data)-1]...)) {
		t.Fatalf("close_token_account %+v %x", ct.Accounts, ct.Data)
	}
	atas, err := f.RecipientATAs(mint)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recipients {
		want, _ := AssociatedTokenAddress(r.Wallet, mint)
		acc := fl.Accounts[len(IxFlush.Accounts)+i]
		if atas[i] != want || acc.Key != want || !acc.Writable || acc.Signer {
			t.Fatalf("recipient %d account %+v, want writable %s", i, acc, want)
		}
	}
	if _, err := NewForwarder(program, [32]byte{5}, ForwarderParams{Treasury: key(9), Recipients: recipients, Mints: p.Mints}); err == nil {
		t.Fatal("zero rent receiver accepted")
	}
	if _, err := NewForwarder(program, [32]byte{5}, ForwarderParams{Treasury: key(9), RentReceiver: key(11), Recipients: recipients,
		Mints: []PublicKey{other, mint}}); !errors.Is(err, solanaderive.ErrMintsNotAscending) {
		t.Fatalf("unsorted mints: %v", err)
	}
	ev := append(SettledEventDiscriminator[:], f.Address[:]...)
	ev = append(ev, mint[:]...)
	src := key(30)
	ev = append(ev, src[:]...)
	for _, v := range []uint64{25_000_000, 250_000, 24_750_000} {
		ev = binary.LittleEndian.AppendUint64(ev, v)
	}
	ev = append(ev, p.Treasury[:]...)
	po := append(PayoutEventDiscriminator[:], f.Address[:]...)
	po = append(po, mint[:]...)
	po = append(po, recipients[0].Wallet[:]...)
	po = binary.LittleEndian.AppendUint64(po, 17_325_000)
	logs := []string{"Program log: Instruction: Flush", "Program data: " + base64.StdEncoding.EncodeToString(ev),
		"Program data: " + base64.StdEncoding.EncodeToString(po), "Program data: AAAA"}
	got := SettledEvents(logs)
	if len(got) != 1 || got[0].Gross != 25_000_000 || got[0].Fee != 250_000 || got[0].Net != 24_750_000 || got[0].Source != key(30) ||
		got[0].Treasury != p.Treasury {
		t.Fatalf("settled %+v", got)
	}
	pays := PayoutEvents(logs)
	if len(pays) != 1 || pays[0].Recipient != recipients[0].Wallet || pays[0].Amount != 17_325_000 || pays[0].Mint != mint {
		t.Fatalf("payouts %+v", pays)
	}
	if _, ok := DecodeSettled(po); ok {
		t.Fatal("a Payout decoded as Settled")
	}
}

// flush's instruction data must match the program's vectors byte for byte, and the worst case
// (five recipients, four mints, close_source) must still fit a transaction.
func TestFlushDataMatchesVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "solana", "vectors", "solana-pda.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ProgramID      string            `json:"programId"`
		Discriminators map[string]string `json:"discriminators"`
		Vectors        []struct {
			Name, Treasury, RentReceiver, Salt, Pda, FlushData string
			FeeBps                                             uint16 `json:"feeBps"`
			FixedFeeMicros                                     uint64 `json:"fixedFeeMicros"`
			Recipients                                         []struct {
				Wallet string `json:"wallet"`
				Bps    uint16 `json:"bps"`
			} `json:"recipients"`
			Mints []string `json:"mints"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Discriminators["ix.flush"] != hex.EncodeToString(IxFlush.Discriminator[:]) ||
		v.Discriminators["ix.close_token_account"] != hex.EncodeToString(IxCloseTokenAccount.Discriminator[:]) {
		t.Fatalf("discriminators %v", v.Discriminators)
	}
	program := MustPublicKey(v.ProgramID)
	for _, c := range v.Vectors {
		t.Run(c.Name, func(t *testing.T) {
			p := ForwarderParams{Treasury: MustPublicKey(c.Treasury), RentReceiver: MustPublicKey(c.RentReceiver),
				Fee: fee.Params{FeeBps: c.FeeBps, FixedFeeMicros: c.FixedFeeMicros}}
			for _, r := range c.Recipients {
				p.Recipients = append(p.Recipients, solanaderive.Recipient{Wallet: MustPublicKey(r.Wallet), Bps: r.Bps})
			}
			for _, m := range c.Mints {
				p.Mints = append(p.Mints, MustPublicKey(m))
			}
			sb, _ := hex.DecodeString(c.Salt)
			f, err := NewForwarder(program, [32]byte(sb), p)
			if err != nil {
				t.Fatal(err)
			}
			if f.Address.String() != c.Pda {
				t.Fatalf("pda %s, want %s", f.Address, c.Pda)
			}
			if got := hex.EncodeToString(f.FlushData(true)); got != c.FlushData {
				t.Fatalf("flush data %s\nwant %s", got, c.FlushData)
			}
			ix, err := f.Flush(key(1), p.Mints[0], key(2), true)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(ix.Data) != c.FlushData {
				t.Fatal("Flush does not use FlushData")
			}
			// The largest flush transaction the senders build: compute budget, the treasury ATA
			// creation, then flush with close_source.
			treATA, _ := AssociatedTokenAddress(p.Treasury, p.Mints[0])
			ixs := []Instruction{SetComputeUnitLimit(400_000), SetComputeUnitPrice(1),
				CreateATAIdempotent(key(1), treATA, p.Treasury, p.Mints[0], TokenProgramID), ix}
			msg, err := Compile(key(1), [32]byte{}, ixs)
			if err != nil {
				t.Fatal(err)
			}
			tx := NewTransaction(msg)
			if raw, err := tx.Serialize(); err != nil || len(raw) > 1232 {
				t.Fatalf("flush transaction is %d bytes: %v", len(raw), err)
			}
		})
	}
}

func TestTokenAccountUsable(t *testing.T) {
	mint := MustPublicKey("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	data := make([]byte, TokenAccountSize)
	copy(data, mint[:])
	data[108] = 1
	if !TokenAccountUsable(TokenProgramID.String(), data, mint) {
		t.Fatal("initialized account refused")
	}
	other := MustPublicKey("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	frozen := append([]byte(nil), data...)
	frozen[108] = 2
	for name, c := range map[string]struct {
		owner string
		data  []byte
		mint  PublicKey
	}{
		"other mint": {TokenProgramID.String(), data, other}, "frozen": {TokenProgramID.String(), frozen, mint},
		"token-2022": {Token2022ProgramID.String(), data, mint}, "short": {TokenProgramID.String(), data[:100], mint},
		"system": {SystemProgramID.String(), nil, mint},
	} {
		t.Run(name, func(t *testing.T) {
			if TokenAccountUsable(c.owner, c.data, c.mint) {
				t.Errorf("%s accepted", name)
			}
			if got := TokenAccountFrozen(c.owner, c.data, c.mint); got != (name == "frozen") {
				t.Errorf("%s frozen = %v", name, got)
			}
		})
	}
	if TokenAccountFrozen(TokenProgramID.String(), data, mint) {
		t.Error("initialized account reported frozen")
	}
}
