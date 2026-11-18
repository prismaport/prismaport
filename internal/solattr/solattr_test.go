package solattr

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solanaderive"
	"github.com/prismaport/prismaport/internal/solrpc"
)

// The fixtures are real jsonParsed answers of a local validator running the program: a
// payer's deposit into a PDA's ATA (created in the same transaction) and the flusher's
// initialize + flush + close_token_account + close_forwarder that settled it.

func load(t *testing.T, name string, edit func(string) string) *solrpc.Tx {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.HasPrefix(name, "flush") {
		s = asD41Flush(t, s)
	}
	if edit != nil {
		s = edit(s)
	}
	tx, err := solrpc.ParseTransactionJSON(json.RawMessage(s))
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// asD41Flush moves a pre-D41 flush instruction to the D41 account list, which gained
// rent_receiver after treasury_ata (the fixture's rent receiver is its fee payer, accounts[0]).
// Nothing else FlushSettlement reads changed: the discriminator, the account positions it checks
// and the inner transfers are the same.
// TODO(D41 task 5): re-record the flush fixtures from the stateless program on devnet and drop
// this shim.
func asD41Flush(t *testing.T, s string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	msg := doc["transaction"].(map[string]any)["message"].(map[string]any)
	for _, raw := range msg["instructions"].([]any) {
		ix := raw.(map[string]any)
		accs, _ := ix["accounts"].([]any)
		data, _ := ix["data"].(string)
		d, err := base58.Decode(data)
		if len(accs) < 9 || err != nil || len(d) < 8 || [8]byte(d[:8]) != solana.IxFlush.Discriminator {
			continue
		}
		ix["accounts"] = append(append(append([]any{}, accs[:6]...), accs[0]), accs[6:]...)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The fixtures were recorded from a forwarder that took exactly 1% of every flush; with no fixed
// part the current rule moves the same amounts, so they still exercise it end to end.
var pilot = fee.Params{FeeBps: 100}

// flushParts names the accounts of the flush fixture: 1 forwarder, 2 source ATA, 4 recipient
// ATA, 5 treasury ATA, 8 program, 9 mint. The fixture pays a single recipient.
func flushParts(t *testing.T, tx *solrpc.Tx) (solana.Forwarder, solana.PublicKey, solana.PublicKey) {
	t.Helper()
	k := func(i int) solana.PublicKey { return solana.MustPublicKey(tx.AccountKeys[i]) }
	_, dst := tx.TokenBalanceOf(4)
	_, tre := tx.TokenBalanceOf(5)
	f := solana.Forwarder{Program: k(8), Address: k(1), Params: solana.ForwarderParams{Treasury: solana.MustPublicKey(tre.Owner), Fee: pilot,
		Recipients: []solanaderive.Recipient{{Wallet: solana.MustPublicKey(dst.Owner), Bps: 10_000}}}}
	return f, k(2), k(9)
}

func TestDepositInflowFromRealTransaction(t *testing.T) {
	tx := load(t, "deposit", nil)
	ftx := load(t, "flush", nil)
	f, source, mint := flushParts(t, ftx)
	d, ok, err := DepositInflow(tx, source.String(), f.Address.String(), mint.String())
	if err != nil || !ok || d.Amount.String() != "25000000" || tx.AccountKeys[d.AccountIndex] != source.String() {
		t.Fatalf("deposit %+v ok=%v err=%v", d, ok, err)
	}
	if _, ok, err := DepositInflow(tx, source.String(), f.Address.String(), solana.TokenProgramID.String()); ok || err == nil {
		t.Fatal("wrong mint credited")
	}
	if _, ok, err := DepositInflow(tx, source.String(), "11111111111111111111111111111111", mint.String()); ok || err == nil {
		t.Fatal("token account of another owner credited")
	}
	if _, ok, _ := DepositInflow(tx, "11111111111111111111111111111111", f.Address.String(), mint.String()); ok {
		t.Fatal("account not in the transaction credited")
	}
	// The flush moves the whole balance out and closes the account: nothing is paid in.
	if _, ok, err := DepositInflow(ftx, source.String(), f.Address.String(), mint.String()); ok || err != nil {
		t.Fatalf("flush counted as a deposit: ok=%v err=%v", ok, err)
	}
}

func TestDepositInflowRejectsInconsistentBalances(t *testing.T) {
	ftx := load(t, "flush", nil)
	f, source, mint := flushParts(t, ftx)
	tx := load(t, "deposit", func(s string) string {
		return strings.Replace(s, `"amount": "25000000"`, `"amount": "25000001"`, 1)
	})
	_, ok, err := DepositInflow(tx, source.String(), f.Address.String(), mint.String())
	var r *Rejection
	if ok || !errors.As(err, &r) {
		t.Fatalf("inconsistent deposit: ok=%v err=%v", ok, err)
	}
	t22 := load(t, "deposit", func(s string) string {
		return strings.ReplaceAll(s, `"programId": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"`, `"programId": "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"`)
	})
	if _, ok, err := DepositInflow(t22, source.String(), f.Address.String(), mint.String()); ok || err == nil {
		t.Fatal("Token-2022 account credited")
	}
	failed := load(t, "deposit", func(s string) string {
		return strings.Replace(s, `"err": null`, `"err": {"InstructionError": [1, "Custom"]}`, 1)
	})
	if _, ok, _ := DepositInflow(failed, source.String(), f.Address.String(), mint.String()); ok {
		t.Fatal("failed transaction credited")
	}
}

func TestFlushSettlementFromRealTransaction(t *testing.T) {
	tx := load(t, "flush", nil)
	f, source, mint := flushParts(t, tx)
	sp, ok, err := FlushSettlement(tx, f, source, mint, 6)
	if err != nil || !ok {
		t.Fatalf("flush: ok=%v err=%v", ok, err)
	}
	if sp.Gross.String() != "25000000" || sp.Fee.String() != "250000" || sp.Net.String() != "24750000" || sp.Index != 3 ||
		len(sp.Payouts) != 1 || sp.Payouts[0].String() != "24750000" {
		t.Fatalf("split %+v", sp)
	}
	other := f
	other.Address = mint
	if _, ok, _ := FlushSettlement(tx, other, source, mint, 6); ok {
		t.Fatal("flush of another forwarder attributed")
	}
	var r *Rejection
	wrongRecipient := f
	wrongRecipient.Params.Recipients = []solanaderive.Recipient{{Wallet: f.Params.Treasury, Bps: 10_000}}
	if _, ok, err := FlushSettlement(tx, wrongRecipient, source, mint, 6); ok || !errors.As(err, &r) {
		t.Fatalf("flush to an unexpected account accepted: %v", err)
	}
	otherFee := f
	otherFee.Params.Fee = fee.Params{FeeBps: 50, FixedFeeMicros: 100_000}
	if _, ok, err := FlushSettlement(tx, otherFee, source, mint, 6); ok || !errors.As(err, &r) {
		t.Fatalf("flush under another fee rule accepted: %v", err)
	}
	// The same transfers read as a 70/30 split: the account list is the program's, so a forwarder
	// that commits to two recipients does not match a one-recipient flush instruction.
	two := f
	two.Params.Recipients = []solanaderive.Recipient{{Wallet: f.Params.Recipients[0].Wallet, Bps: 7000}, {Wallet: f.Params.Treasury, Bps: 3000}}
	if _, ok, _ := FlushSettlement(tx, two, source, mint, 6); ok {
		t.Fatal("a flush instruction with another recipient count attributed")
	}
}

func walletSplit(t *testing.T, tx *solrpc.Tx, recipients ...string) Split {
	t.Helper()
	sp := Split{TreasuryATA: tx.AccountKeys[5], RecipientATAs: recipients, Decimals: 6, Fee: pilot}
	for range recipients {
		sp.Bps = append(sp.Bps, 0)
	}
	sp.Bps[0] = 10_000
	return sp
}

// A connected-wallet payment moves the shares into the recipients' ATAs and the fee into the
// treasury ATA; the flush fixture has exactly that shape for one recipient.
func TestWalletPaymentSplit(t *testing.T) {
	tx := load(t, "flush", nil)
	_, _, mint := flushParts(t, tx)
	dst, tre := tx.AccountKeys[4], tx.AccountKeys[5]
	sp, ok, err := WalletPayment(tx, mint.String(), walletSplit(t, tx, dst))
	if err != nil || !ok || sp.Gross.String() != "25000000" || sp.Fee.String() != "250000" || sp.Net.String() != "24750000" || sp.Index != 4 ||
		len(sp.Payouts) != 1 || sp.Payouts[0].String() != "24750000" {
		t.Fatalf("wallet split %+v ok=%v err=%v", sp, ok, err)
	}
	var r *Rejection
	otherFee := walletSplit(t, tx, dst)
	otherFee.Fee = fee.Params{FeeBps: 50, FixedFeeMicros: 100_000}
	if _, ok, err := WalletPayment(tx, mint.String(), otherFee); ok || !errors.As(err, &r) {
		t.Fatalf("split under another fee rule accepted: %v", err)
	}
	noFee := walletSplit(t, tx, dst)
	noFee.TreasuryATA = "11111111111111111111111111111111"
	if _, ok, err := WalletPayment(tx, mint.String(), noFee); ok || !errors.As(err, &r) {
		t.Fatalf("payment without the fee accepted: %v", err)
	}
	if _, ok, _ := WalletPayment(tx, mint.String(), walletSplit(t, tx, "11111111111111111111111111111111")); ok {
		t.Fatal("payment to another first recipient accepted")
	}
	// A second recipient that received nothing: the rule wants a share there.
	missing := walletSplit(t, tx, dst, "11111111111111111111111111111111")
	missing.Bps = []uint16{7000, 3000}
	if _, ok, err := WalletPayment(tx, mint.String(), missing); ok || !errors.As(err, &r) {
		t.Fatalf("payment missing a share accepted: %v", err)
	}
	// The treasury as the second recipient: its account would need fee plus share.
	treasuryToo := walletSplit(t, tx, dst, tre)
	treasuryToo.Bps = []uint16{9000, 1000}
	if _, ok, err := WalletPayment(tx, mint.String(), treasuryToo); ok || !errors.As(err, &r) {
		t.Fatalf("payment with the treasury share missing accepted: %v", err)
	}
}

func TestExpectFlowsTreasuryAsRecipient(t *testing.T) {
	sp := Split{TreasuryATA: "T", RecipientATAs: []string{"A", "T"}, Bps: []uint16{7000, 3000}, Decimals: 6, Fee: pilot}
	gross, _ := units("25000000")
	rule, want, err := sp.expectFlows(gross)
	if err != nil {
		t.Fatal(err)
	}
	if want["A"].String() != "17325000" || want["T"].String() != "7675000" || rule.Fee.String() != "250000" {
		t.Fatalf("want %v rule %+v", want, rule)
	}
	if !matches(want, map[string]money.Units{"A": want["A"], "T": want["T"]}) {
		t.Fatal("exact flows refused")
	}
	if matches(want, map[string]money.Units{"A": want["A"], "T": want["T"], "X": money.FromUint64(1)}) {
		t.Fatal("an extra account accepted")
	}
	if matches(want, map[string]money.Units{"A": want["T"], "T": want["A"]}) {
		t.Fatal("swapped flows accepted")
	}
}

// flush-5 is a real flush to five recipients (33.34/20/16.66/15/15): 1 forwarder, 2 source ATA,
// 3 treasury ATA, 4-8 recipient ATAs, 12 program, 13 mint.
func fiveRecipientFlush(t *testing.T, tx *solrpc.Tx) (solana.Forwarder, solana.PublicKey, solana.PublicKey) {
	t.Helper()
	k := func(i int) solana.PublicKey { return solana.MustPublicKey(tx.AccountKeys[i]) }
	_, tre := tx.TokenBalanceOf(3)
	f := solana.Forwarder{Program: k(12), Address: k(1), Params: solana.ForwarderParams{Treasury: solana.MustPublicKey(tre.Owner), Fee: pilot}}
	for i, bps := range []uint16{3334, 2000, 1666, 1500, 1500} {
		_, r := tx.TokenBalanceOf(4 + i)
		f.Params.Recipients = append(f.Params.Recipients, solanaderive.Recipient{Wallet: solana.MustPublicKey(r.Owner), Bps: bps})
	}
	return f, k(2), k(13)
}

func TestFlushSettlementFiveRecipientsFromRealTransaction(t *testing.T) {
	tx := load(t, "flush-5", nil)
	f, source, mint := fiveRecipientFlush(t, tx)
	sp, ok, err := FlushSettlement(tx, f, source, mint, 6)
	if err != nil || !ok || sp.Gross.String() != "25010000" || sp.Fee.String() != "250100" || sp.Net.String() != "24759900" {
		t.Fatalf("five-recipient flush %+v ok=%v err=%v", sp, ok, err)
	}
	for i, want := range []string{"8254951", "4951980", "4124999", "3713985", "3713985"} {
		if sp.Payouts[i].String() != want {
			t.Fatalf("payout %d = %s, want %s", i, sp.Payouts[i], want)
		}
	}
	// The same wallets with the shares in another order are another split: the flows do not match.
	var r *Rejection
	swapped := f
	swapped.Params.Recipients = append([]solanaderive.Recipient(nil), f.Params.Recipients...)
	swapped.Params.Recipients[0].Bps, swapped.Params.Recipients[1].Bps = 2000, 3334
	if _, ok, err := FlushSettlement(tx, swapped, source, mint, 6); ok || !errors.As(err, &r) {
		t.Fatalf("flush read under reordered shares: ok=%v err=%v", ok, err)
	}
	four := f
	four.Params.Recipients = f.Params.Recipients[:4]
	if _, ok, _ := FlushSettlement(tx, four, source, mint, 6); ok {
		t.Fatal("a five-recipient flush attributed to a four-recipient forwarder")
	}
}

// wallet-2 is a real connected-wallet payment split 70/30: 3 and 4 recipient ATAs, 5 treasury ATA.
func TestWalletPaymentTwoRecipientsFromRealTransaction(t *testing.T) {
	tx := load(t, "wallet-2", nil)
	mint := tx.AccountKeys[7]
	sp := Split{TreasuryATA: tx.AccountKeys[5], RecipientATAs: []string{tx.AccountKeys[3], tx.AccountKeys[4]}, Bps: []uint16{7000, 3000}, Decimals: 6, Fee: pilot}
	got, ok, err := WalletPayment(tx, mint, sp)
	if err != nil || !ok || got.Gross.String() != "25010000" || got.Fee.String() != "250100" || got.Index != 3 ||
		len(got.Payouts) != 2 || got.Payouts[0].String() != "17331930" || got.Payouts[1].String() != "7427970" {
		t.Fatalf("wallet payment %+v ok=%v err=%v", got, ok, err)
	}
	var r *Rejection
	reversed := sp
	reversed.Bps = []uint16{3000, 7000}
	if _, ok, err := WalletPayment(tx, mint, reversed); ok || !errors.As(err, &r) {
		t.Fatalf("payment read under reversed shares: ok=%v err=%v", ok, err)
	}
	one := sp
	one.RecipientATAs, one.Bps = sp.RecipientATAs[:1], []uint16{10_000}
	if _, ok, err := WalletPayment(tx, mint, one); ok || !errors.As(err, &r) {
		t.Fatalf("a share paid to an account outside the split was ignored: ok=%v err=%v", ok, err)
	}
}
