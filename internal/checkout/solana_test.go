package checkout

import (
	"bytes"
	"math"
	"testing"

	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/store"
)

func testKey(t *testing.T, b byte) *solana.Keypair {
	t.Helper()
	k, err := solana.KeypairFromSeed(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testATA(t *testing.T, owner, mint solana.PublicKey) solana.PublicKey {
	t.Helper()
	a, err := solana.AssociatedTokenAddress(owner, mint)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// testWalletTarget pays 70/30 to two merchant wallets under the pilot fee rule.
func testWalletTarget(t *testing.T) walletTarget {
	t.Helper()
	var w walletTarget
	w.sc = SolanaChain{FeePayer: testKey(t, 1)}
	w.mint, w.tre = testKey(t, 2).PublicKey(), testKey(t, 3).PublicKey()
	w.treATA = testATA(t, w.tre, w.mint)
	w.reference = solana.SessionReference([32]byte{9})
	for i, b := range []byte{4, 5} {
		r := testKey(t, b).PublicKey()
		w.recipients = append(w.recipients, r)
		w.recipientTA = append(w.recipientTA, testATA(t, r, w.mint))
		w.bps = append(w.bps, []uint16{7000, 3000}[i])
	}
	w.pt = store.PayToRecord{Decimals: 6, Fee: fee.Params{FeeBps: 50, FixedFeeMicros: 100_000}}
	return w
}

func testSplit(t *testing.T, gross uint64, bps []uint16) payout.Settlement {
	t.Helper()
	st, err := payout.Compute(money.FromUint64(gross), 6, fee.Params{FeeBps: 50, FixedFeeMicros: 100_000}, bps)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// walletTx builds the payment the checkout would build for payer, then lets edit change its
// instructions (index 0 is the compute limit, 1 and 2 the shares, 3 the treasury account
// creation, 4 the fee) before the fee payer signs it.
func walletTx(t *testing.T, w walletTarget, payer solana.PublicKey, st payout.Settlement, edit func([]solana.Instruction) []solana.Instruction) *solana.Transaction {
	t.Helper()
	ixs, err := w.instructions(payer, st, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		ixs = edit(ixs)
	}
	tx, err := w.sign([32]byte{7}, ixs)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestWalletGrossReadsTheSplitBack(t *testing.T) {
	w := testWalletTarget(t)
	payer := testKey(t, 6).PublicKey()

	gross, got, ok := w.walletGross(walletTx(t, w, payer, testSplit(t, 25_000_000, w.bps), nil))

	if !ok || gross.String() != "25000000" || got != payer {
		t.Fatalf("gross %s payer %s ok %v", gross, got, ok)
	}
}

func TestWalletGrossRefusesWhatTheCheckoutDidNotBuild(t *testing.T) {
	w := testWalletTarget(t)
	payer := testKey(t, 6).PublicKey()
	good := testSplit(t, 25_000_000, w.bps)
	stranger := testATA(t, testKey(t, 8).PublicKey(), w.mint)
	for _, tc := range []struct {
		name string
		tx   func() *solana.Transaction
	}{
		{"shares of another split", func() *solana.Transaction {
			return walletTx(t, w, payer, testSplit(t, 25_000_000, []uint16{5000, 5000}), nil)
		}},
		{"fee missing", func() *solana.Transaction {
			return walletTx(t, w, payer, good, func(ixs []solana.Instruction) []solana.Instruction { return ixs[:3] })
		}},
		{"extra transfer to a recipient", func() *solana.Transaction {
			return walletTx(t, w, payer, good, func(ixs []solana.Instruction) []solana.Instruction { return append(ixs, ixs[2]) })
		}},
		{"no session reference", func() *solana.Transaction {
			return walletTx(t, w, payer, good, func(ixs []solana.Instruction) []solana.Instruction {
				ixs[1].Accounts = ixs[1].Accounts[:4]
				return ixs
			})
		}},
		{"reference on the wrong transfer", func() *solana.Transaction {
			return walletTx(t, w, payer, good, func(ixs []solana.Instruction) []solana.Instruction {
				ixs[2].Accounts = append(ixs[2].Accounts, ixs[1].Accounts[4])
				ixs[1].Accounts = ixs[1].Accounts[:4]
				return ixs
			})
		}},
		{"fee sent to a foreign account", func() *solana.Transaction {
			return walletTx(t, w, payer, good, func(ixs []solana.Instruction) []solana.Instruction {
				ixs[4].Accounts[2].Key = stranger
				return ixs
			})
		}},
		{"amounts that wrap around", func() *solana.Transaction {
			// 2^64-1 and 1 more into the treasury wrap back to the expected totals in uint64.
			return walletTx(t, w, payer, good, func(ixs []solana.Instruction) []solana.Instruction {
				src := ixs[1].Accounts[0].Key
				return append(ixs, solana.TransferChecked(solana.TokenProgramID, src, w.mint, w.treATA, payer, math.MaxUint64, 6),
					solana.TransferChecked(solana.TokenProgramID, src, w.mint, w.treATA, payer, 1, 6))
			})
		}},
		{"dust", func() *solana.Transaction {
			st := testSplit(t, 25_000_000, w.bps)
			st.Fee, st.Shares = money.Zero, []money.Units{money.FromUint64(50_000), money.Zero}
			return walletTx(t, w, payer, st, nil)
		}},
		{"built by another fee payer", func() *solana.Transaction {
			other := w
			other.sc = SolanaChain{FeePayer: testKey(t, 10)}
			return walletTx(t, other, payer, good, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gross, _, ok := w.walletGross(tc.tx()); ok {
				t.Fatalf("accepted with gross %s", gross)
			}
		})
	}
}
