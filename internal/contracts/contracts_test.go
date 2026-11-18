package contracts

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

func mustAddr(s string) evm.Address {
	a, err := evm.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

var (
	salt, _ = evm.ParseWord("0x1284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a")
	rcpA    = mustAddr("0x8ba1f109551bD432803012645Ac136ddd64DBA72")
	rcpB    = mustAddr("0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB")
	tre     = mustAddr("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	usdc    = mustAddr("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	pilot   = fee.Params{FeeBps: 100, FixedFeeMicros: 100_000}
)

func TestSignaturesMatchCast(t *testing.T) {
	for _, c := range []struct {
		got  string
		want string
	}{
		{hex.EncodeToString(deployAndFlush[:]), "61f1d3f3"},
		{hex.EncodeToString(settle[:]), "4653ffba"},
		{hex.EncodeToString(isBlacklisted[:]), "fe575a87"},
		{PayoutTopic.Hex(), "0xebe7adb4feddf2afa5de463169fdc706254a55c9cf2b930ac84bb49e28cfe6bb"},
		{SettledTopic.Hex(), "0x5d734739100f8290ff4ee0c44473bd0f150bb951cb727949dbe9321ee0007bbc"},
		{PaymentReceivedTopic.Hex(), "0xc88a8b064362c292d064d969e5bd11efb445602fca8c4adbd67c183f3253a08a"},
	} {
		t.Run(c.got, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %s, want %s", c.got, c.want)
			}
		})
	}
}

// Expected calldata produced by eth_abi 5.2 (Python) for the D41 Params tuple; the pre-D41
// vectors came from `cast calldata` (Foundry v1.8.1) and the encoder is unchanged otherwise.
func TestDeployAndFlushMatchesCast(t *testing.T) {
	got := DeployAndFlush(salt, ForwarderParams{Treasury: tre, Fee: pilot, Recipients: []evm.Address{rcpA, rcpB}, Bps: []uint16{7000, 3000},
		Tokens: []evm.Address{usdc}}, []evm.Address{usdc})
	want := "61f1d3f31284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a000000000000000000000000000000000000000000000000000000000000006000000000000000000000000000000000000000000000000000000000000002200000000000000000000000005aaeb6053f3e94c9b9a09f33669435e7ef1beaed000000000000000000000000000000000000000000000000000000000000006400000000000000000000000000000000000000000000000000000000000186a000000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000120000000000000000000000000000000000000000000000000000000000000018000000000000000000000000000000000000000000000000000000000000000020000000000000000000000008ba1f109551bd432803012645ac136ddd64dba72000000000000000000000000dbf03b407c01e7cd3cbea99509d93f8dddc8c6fb00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000001b580000000000000000000000000000000000000000000000000000000000000bb80000000000000000000000000000000000000000000000000000000000000001000000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda029130000000000000000000000000000000000000000000000000000000000000001000000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	if hex.EncodeToString(got) != want {
		t.Fatalf("calldata\n got %x\nwant %s", got, want)
	}
	got = DeployAndFlush(salt, ForwarderParams{Treasury: tre, Fee: pilot, Recipients: []evm.Address{rcpA}, Bps: []uint16{10_000},
		Tokens: []evm.Address{usdc, rcpB}}, []evm.Address{usdc, rcpB})
	want = "61f1d3f31284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a000000000000000000000000000000000000000000000000000000000000006000000000000000000000000000000000000000000000000000000000000002000000000000000000000000005aaeb6053f3e94c9b9a09f33669435e7ef1beaed000000000000000000000000000000000000000000000000000000000000006400000000000000000000000000000000000000000000000000000000000186a000000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000014000000000000000000000000000000000000000000000000000000000000000010000000000000000000000008ba1f109551bd432803012645ac136ddd64dba72000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000027100000000000000000000000000000000000000000000000000000000000000002000000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda02913000000000000000000000000dbf03b407c01e7cd3cbea99509d93f8dddc8c6fb0000000000000000000000000000000000000000000000000000000000000002000000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda02913000000000000000000000000dbf03b407c01e7cd3cbea99509d93f8dddc8c6fb"
	if hex.EncodeToString(got) != want {
		t.Fatalf("single recipient, two tokens\n got %x\nwant %s", got, want)
	}
}

func TestSettleMatchesCast(t *testing.T) {
	r, _ := evm.ParseWord("0x1111111111111111111111111111111111111111111111111111111111111111")
	s, _ := evm.ParseWord("0x2222222222222222222222222222222222222222222222222222222222222222")
	got := Settle(Authorization{Token: usdc, From: mustAddr("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"), Value: big.NewInt(25_000_000),
		ValidAfter: new(big.Int), ValidBefore: big.NewInt(1_800_003_600), SessionSalt: salt, Recipients: []evm.Address{rcpA, rcpB},
		Bps: []uint16{7000, 3000}, FeeBps: 100, FixedFeeMicros: 100_000, V: 27, R: r, S: s})
	want := "4653ffba0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda02913000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb9226600000000000000000000000000000000000000000000000000000000017d78400000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000006b49e0101284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a00000000000000000000000000000000000000000000000000000000000001a00000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000006400000000000000000000000000000000000000000000000000000000000186a0000000000000000000000000000000000000000000000000000000000000001b1111111111111111111111111111111111111111111111111111111111111111222222222222222222222222222222222222222222222222222222222222222200000000000000000000000000000000000000000000000000000000000000020000000000000000000000008ba1f109551bd432803012645ac136ddd64dba72000000000000000000000000dbf03b407c01e7cd3cbea99509d93f8dddc8c6fb00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000001b580000000000000000000000000000000000000000000000000000000000000bb8"
	if hex.EncodeToString(got) != want {
		t.Fatalf("calldata\n got %x\nwant %s", got, want)
	}
}

func word(n uint64) []byte { w := evm.Uint64Word(n); return w[:] }

func TestResultsAndEvents(t *testing.T) {
	str, _ := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000855534420436f696e000000000000000000000000000000000000000000000000")
	if s, err := StringResult(str); err != nil || s != "USD Coin" {
		t.Fatalf("string %q %v", s, err)
	}
	if _, err := StringResult(str[:40]); err == nil {
		t.Fatal("short string accepted")
	}
	bad := append([]byte(nil), str...)
	bad[31] = 0xff
	if _, err := StringResult(bad); err == nil {
		t.Fatal("bad offset accepted")
	}
	if u, err := Uint(word(42)); err != nil || u.String() != "42" {
		t.Fatal("uint")
	}
	if _, err := Uint(word(42)[:31]); err == nil {
		t.Fatal("short uint accepted")
	}
	for v, want := range map[uint64]bool{0: false, 1: true} {
		if got, err := Bool(word(v)); err != nil || got != want {
			t.Fatalf("bool %d: %v %v", v, got, err)
		}
	}
	if _, err := Bool(word(2)); err == nil {
		t.Fatal("2 decoded as a bool")
	}
	aw := evm.AddressWord(tre)
	if a, err := AddressResult(aw[:]); err != nil || a != tre {
		t.Fatal("address")
	}
	fwd := mustAddr("0xA6A882118113729326f1490373b59aD6dEb82c3c")
	tx, _ := evm.ParseWord("0x00000000000000000000000000000000000000000000000000000000000000aa")
	data := append(append(append(word(100), word(1)...), word(99)...), aw[:]...)
	l := evmrpc.Log{Address: fwd, Topics: []evm.Word{SettledTopic, evm.AddressWord(usdc)}, Data: data, LogIndex: 3, TxHash: tx}
	ev, ok := ParseSettled(l)
	if !ok || ev.Forwarder != fwd || ev.Token != usdc || ev.Treasury != tre || ev.Gross.String() != "100" ||
		ev.Fee.String() != "1" || ev.Net.String() != "99" || ev.LogIndex != 3 || ev.TxHash != tx {
		t.Fatalf("settled %+v", ev)
	}
	l.Topics[0] = TransferTopic
	if _, ok := ParseSettled(l); ok {
		t.Fatal("other event parsed as Settled")
	}
	l.Topics = []evm.Word{SettledTopic, evm.AddressWord(usdc), evm.AddressWord(tre)}
	if _, ok := ParseSettled(l); ok {
		t.Fatal("the single-destination Settled layout parsed")
	}
	pl := evmrpc.Log{Address: fwd, Topics: []evm.Word{PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(rcpA)}, Data: word(70), LogIndex: 1, TxHash: tx}
	p, ok := ParsePayout(pl)
	if !ok || p.Emitter != fwd || p.Token != usdc || p.Recipient != rcpA || p.Amount.String() != "70" {
		t.Fatalf("payout %+v", p)
	}
	if _, ok := ParsePayout(evmrpc.Log{Address: fwd, Topics: pl.Topics[:2], Data: word(70)}); ok {
		t.Fatal("payout without an indexed recipient parsed")
	}
	nonce := evm.Word{9}
	from := mustAddr("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	prl := evmrpc.Log{Address: fwd, Topics: []evm.Word{PaymentReceivedTopic, nonce, evm.AddressWord(usdc), evm.AddressWord(from)},
		Data: append(append(word(100), word(1)...), word(99)...), LogIndex: 4}
	pr, ok := ParsePaymentReceived(prl)
	if !ok || pr.Nonce != nonce || pr.Token != usdc || pr.From != from || pr.Gross.String() != "100" || pr.Fee.String() != "1" || pr.Net.String() != "99" {
		t.Fatalf("payment received %+v", pr)
	}
	prl.Data = append(prl.Data, aw[:]...)
	if _, ok := ParsePaymentReceived(prl); ok {
		t.Fatal("the single-destination PaymentReceived layout parsed")
	}
	errSel := evm.Selector("Error(string)")
	if got := RevertReason(append(errSel[:], str...)); got != "USD Coin" {
		t.Fatalf("revert reason %q", got)
	}
	for _, e := range []string{"DustPayment()", "InvalidShareSum(uint256)", "DuplicateRecipient(uint256)", "InvalidRecipient(uint256)",
		"InvalidTokenCount(uint256)", "ZeroToken(uint256)", "TokensNotAscending(uint256)", "TokenNotAllowed(address)", "NotTreasury()", "TokenIsBound(address)"} {
		sel := evm.Selector(e)
		if got := RevertReason(append(sel[:], word(1)...)); got != e {
			t.Fatalf("custom error %q", got)
		}
	}
	if RevertReason(nil) == "" || RevertReason([]byte{1, 2, 3, 4}) == "" {
		t.Fatal("unknown revert")
	}
	if len(BalanceOf(tre)) != 36 || len(IsBlacklisted(tre)) != 36 || len(TreasuryCall()) != 4 || len(DomainSeparatorCall()) != 4 || len(NameCall()) != 4 {
		t.Fatal("call encodings")
	}
}

func TestPayoutsBeforeAndMatch(t *testing.T) {
	fwd := mustAddr("0xA6A882118113729326f1490373b59aD6dEb82c3c")
	other := mustAddr("0x1111111111111111111111111111111111111111")
	tx := evm.Word{1}
	payout := func(emitter evm.Address, token, rcp evm.Address, amount uint64, idx uint32, txh evm.Word) evmrpc.Log {
		return evmrpc.Log{Address: emitter, Topics: []evm.Word{PayoutTopic, evm.AddressWord(token), evm.AddressWord(rcp)}, Data: word(amount),
			LogIndex: idx, TxHash: txh}
	}
	rcpC := mustAddr("0x0000000000000000000000000000000000000001")
	logs := []evmrpc.Log{
		payout(fwd, usdc, rcpA, 70, 2, tx),
		payout(other, usdc, rcpB, 1_000, 3, tx), // spoofed by another contract
		payout(fwd, usdc, rcpB, 30, 4, tx),
		payout(fwd, rcpB, rcpB, 5, 5, tx), // another token
		payout(fwd, usdc, rcpC, 9, 9, tx), // after the settlement
		payout(fwd, usdc, rcpC, 9, 1, evm.Word{2}),
	}
	got := PayoutsBefore(logs, fwd, usdc, tx, 6, nil)
	if len(got) != 2 || got[0].Recipient != rcpA || got[1].Recipient != rcpB {
		t.Fatalf("payouts %+v", got)
	}
	if late := PayoutsBefore(logs, fwd, usdc, tx, 6, []uint32{3}); len(late) != 1 || late[0].Recipient != rcpB {
		t.Fatalf("payouts after an earlier settlement %+v", late)
	}
	amounts, ok := MatchPayouts([]evm.Address{rcpA, rcpC, rcpB}, got)
	if !ok || amounts[0].String() != "70" || !amounts[1].IsZero() || amounts[2].String() != "30" {
		t.Fatalf("matched %v %v", amounts, ok)
	}
	if _, ok := MatchPayouts([]evm.Address{rcpB, rcpA}, got); ok {
		t.Fatal("payouts out of committed order matched")
	}
	if _, ok := MatchPayouts([]evm.Address{rcpA}, got); ok {
		t.Fatal("payout to an address outside the split matched")
	}
	dup := []Payout{{Recipient: rcpA, Amount: money.FromUint64(1)}, {Recipient: rcpA, Amount: money.FromUint64(1)}}
	if _, ok := MatchPayouts([]evm.Address{rcpA, rcpB}, dup); ok {
		t.Fatal("repeated recipient matched")
	}
	if _, ok := MatchPayouts([]evm.Address{rcpA}, []Payout{{Recipient: rcpA, Amount: money.Zero}}); ok {
		t.Fatal("zero payout event matched")
	}
}
