// Package solattr reads money movements out of agreed Solana transactions: what a transaction
// paid into a deposit token account, what a connected-wallet payment split between the
// recipients and the treasury, and what a forwarder flush settled. Every amount comes from
// SPL Token instructions and is cross-checked against the token balance changes the runtime
// recorded; logs and events are never trusted.
package solattr

import (
	"fmt"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
)

var tokenProgram = solana.TokenProgramID.String()

// Rejection explains why a transaction that touches a watched account was not credited.
type Rejection struct{ Reason string }

func (r *Rejection) Error() string { return "solattr: " + r.Reason }

func reject(format string, args ...any) error {
	return &Rejection{Reason: fmt.Sprintf(format, args...)}
}

func units(s string) (money.Units, error) {
	if s == "" {
		return money.Zero, nil
	}
	return money.ParseInteger(s)
}

// balance returns the account's classic-token balance before and after the transaction, after
// checking every recorded entry is a token account of mint owned by owner (owner "" skips the
// owner check).
func balance(tx *solrpc.Tx, idx int, mint, owner string) (pre, post money.Units, err error) {
	preB, postB := tx.TokenBalanceOf(idx)
	pre, post = money.Zero, money.Zero
	for _, b := range []*solrpc.TokenBalance{preB, postB} {
		if b == nil {
			continue
		}
		if b.Mint != mint || b.ProgramID != tokenProgram || (owner != "" && b.Owner != owner) {
			return pre, post, reject("account %s is not a classic %s token account of %s", tx.AccountKeys[idx], mint, owner)
		}
	}
	if preB != nil {
		if pre, err = units(preB.Amount); err != nil {
			return pre, post, err
		}
	}
	if postB != nil {
		if post, err = units(postB.Amount); err != nil {
			return pre, post, err
		}
	}
	return pre, post, nil
}

// flows sums the classic SPL Token amounts moved into and out of account.
func flows(tx *solrpc.Tx, account string) (in, out money.Units, err error) {
	in, out = money.Zero, money.Zero
	for _, m := range tx.Movements() {
		if m.ProgramID != tokenProgram {
			continue
		}
		if m.From == account && m.To == account {
			continue
		}
		amt, err := units(m.Amount)
		if err != nil {
			return in, out, err
		}
		switch {
		case m.To == account:
			if in, err = in.Add(amt); err != nil {
				return in, out, err
			}
		case m.From == account:
			if out, err = out.Add(amt); err != nil {
				return in, out, err
			}
		}
	}
	return in, out, nil
}

// consistent checks that the instructions explain the recorded balance change exactly.
func consistent(pre, post, in, out money.Units) bool {
	a, err1 := post.Add(out)
	b, err2 := pre.Add(in)
	return err1 == nil && err2 == nil && a.Equal(b)
}

// Deposit is what one transaction paid into a deposit token account.
type Deposit struct {
	Amount money.Units
	// AccountIndex is the token account's index in the message: the payment's position within
	// the transaction.
	AccountIndex int
}

// DepositInflow returns the gross amount tx moved into account (a classic token account of mint
// owned by owner). It counts inflows, not the net change, so a deposit flushed in the same
// transaction is still seen. ok is false when the transaction paid nothing in; err (a
// *Rejection) when the movements do not explain the balance change.
func DepositInflow(tx *solrpc.Tx, account, owner, mint string) (Deposit, bool, error) {
	if tx.Failed() {
		return Deposit{}, false, nil
	}
	idx := tx.AccountIndex(account)
	if idx < 0 {
		return Deposit{}, false, nil
	}
	pre, post, err := balance(tx, idx, mint, owner)
	if err != nil {
		return Deposit{}, false, err
	}
	in, out, err := flows(tx, account)
	if err != nil {
		return Deposit{}, false, err
	}
	if !consistent(pre, post, in, out) {
		return Deposit{}, false, reject("instructions move +%s/-%s but the balance went %s -> %s", in, out, pre, post)
	}
	if in.IsZero() {
		return Deposit{}, false, nil
	}
	return Deposit{Amount: in, AccountIndex: idx}, true, nil
}

// Settlement is a settled fee split: gross = fee + net, and the net divided among the
// recipients by the split rule.
type Settlement struct {
	Gross, Fee, Net money.Units
	// Payouts holds one amount per recipient, in recipient order (zero for a zero share).
	Payouts []money.Units
	// Index is the payment's position in the transaction: the first recipient's token account
	// message index (wallet payments) or the flush instruction's index (flushes).
	Index int
}

// Split is what a settlement must pay: the fee to the treasury's token account and each share
// to its recipient's token account, both of the payment's mint.
type Split struct {
	TreasuryATA   string
	RecipientATAs []string
	Bps           []uint16
	Decimals      uint8
	Fee           fee.Params
}

// expectFlows applies the fee and split rule to gross and returns what each token account must
// receive. The treasury may also be a recipient, so one account can expect fee and share.
func (sp Split) expectFlows(gross money.Units) (payout.Settlement, map[string]money.Units, error) {
	rule, err := payout.Compute(gross, sp.Decimals, sp.Fee, sp.Bps)
	if err != nil || rule.Dust {
		return rule, nil, err
	}
	if len(sp.RecipientATAs) != len(rule.Shares) {
		return rule, nil, fmt.Errorf("solattr: %d recipient accounts for %d shares", len(sp.RecipientATAs), len(rule.Shares))
	}
	want := map[string]money.Units{}
	add := func(account string, amount money.Units) error {
		cur, ok := want[account]
		if !ok {
			cur = money.Zero
		}
		v, err := cur.Add(amount)
		want[account] = v
		return err
	}
	if err := add(sp.TreasuryATA, rule.Fee); err != nil {
		return rule, nil, err
	}
	for i, a := range sp.RecipientATAs {
		if err := add(a, rule.Shares[i]); err != nil {
			return rule, nil, err
		}
	}
	return rule, want, nil
}

// matches compares per-account inflows with what the rule expects; an account that expects
// nothing must receive nothing.
func matches(want, got map[string]money.Units) bool {
	for a, w := range want {
		g, ok := got[a]
		if !ok {
			g = money.Zero
		}
		if !g.Equal(w) {
			return false
		}
	}
	for a, g := range got {
		if _, ok := want[a]; !ok && !g.IsZero() {
			return false
		}
	}
	return true
}

// WalletPayment reads a connected-wallet payment: transfers of mint into the recipients' and the
// treasury's token accounts in one transaction, split by the fee and split rule. ok is false
// when the transaction pays nothing into the first recipient's token account.
func WalletPayment(tx *solrpc.Tx, mint string, sp Split) (Settlement, bool, error) {
	if tx.Failed() || len(sp.RecipientATAs) == 0 {
		return Settlement{}, false, nil
	}
	first := tx.AccountIndex(sp.RecipientATAs[0])
	if first < 0 {
		return Settlement{}, false, nil
	}
	got := map[string]money.Units{}
	gross := money.Zero
	for _, a := range append([]string{sp.TreasuryATA}, sp.RecipientATAs...) {
		if _, seen := got[a]; seen {
			continue
		}
		in := money.Zero
		if i := tx.AccountIndex(a); i >= 0 {
			v, err := received(tx, i, a, mint)
			if err != nil {
				return Settlement{}, false, err
			}
			in = v
		}
		got[a] = in
		var err error
		if gross, err = gross.Add(in); err != nil {
			return Settlement{}, false, err
		}
	}
	if got[sp.RecipientATAs[0]].IsZero() {
		return Settlement{}, false, nil
	}
	rule, want, err := sp.expectFlows(gross)
	if err != nil {
		return Settlement{}, false, err
	}
	if rule.Dust || !matches(want, got) {
		return Settlement{}, false, reject("payment of %s does not follow the fee and split rule (dust=%v)", gross, rule.Dust)
	}
	return Settlement{Gross: gross, Fee: rule.Fee, Net: rule.Net, Payouts: rule.Shares, Index: first}, true, nil
}

// received is the amount a token account received in tx, which must be pure inflow matching
// its recorded balance change.
func received(tx *solrpc.Tx, idx int, account, mint string) (money.Units, error) {
	pre, post, err := balance(tx, idx, mint, "")
	if err != nil {
		return money.Zero, err
	}
	in, out, err := flows(tx, account)
	if err != nil {
		return money.Zero, err
	}
	if !out.IsZero() || !consistent(pre, post, in, out) {
		return money.Zero, reject("account %s: instructions move +%s/-%s but the balance went %s -> %s", account, in, out, pre, post)
	}
	return in, nil
}

// FlushSplit is the split a forwarder commits to for one mint.
func FlushSplit(f solana.Forwarder, mint solana.PublicKey, decimals uint8) (Split, error) {
	treATA, err := solana.AssociatedTokenAddress(f.Params.Treasury, mint)
	if err != nil {
		return Split{}, err
	}
	atas, err := f.RecipientATAs(mint)
	if err != nil {
		return Split{}, err
	}
	sp := Split{TreasuryATA: treATA.String(), Decimals: decimals, Fee: f.Params.Fee}
	for i, a := range atas {
		sp.RecipientATAs = append(sp.RecipientATAs, a.String())
		sp.Bps = append(sp.Bps, f.Params.Recipients[i].Bps)
	}
	return sp, nil
}

// FlushSettlement reads what the forwarder's flush instruction for source settled: the inner
// transfers signed by the forwarder from source into the treasury's and the recipients' token
// accounts, checked against the fee and split rule the forwarder commits to. ok is false when
// tx has no such flush or it moved nothing (empty or dust).
func FlushSettlement(tx *solrpc.Tx, f solana.Forwarder, source, mint solana.PublicKey, decimals uint8) (Settlement, bool, error) {
	if tx.Failed() {
		return Settlement{}, false, nil
	}
	sp, err := FlushSplit(f, mint, decimals)
	if err != nil {
		return Settlement{}, false, err
	}
	allowed := map[string]bool{sp.TreasuryATA: true}
	for _, a := range sp.RecipientATAs {
		allowed[a] = true
	}
	src, fwd, prog := source.String(), f.Address.String(), f.Program.String()
	for i, ix := range tx.Instructions {
		if ix.ProgramID != prog || len(ix.Accounts) != len(solana.IxFlush.Accounts)+len(sp.RecipientATAs) || ix.Accounts[1] != fwd || ix.Accounts[3] != src {
			continue
		}
		data, err := base58.Decode(ix.Data)
		if err != nil || len(data) < 8 || [8]byte(data[:8]) != solana.IxFlush.Discriminator {
			continue
		}
		got := map[string]money.Units{}
		gross := money.Zero
		for _, m := range tx.Movements() {
			if m.Instruction != i || m.ProgramID != tokenProgram || m.From != src || m.Authority != fwd {
				continue
			}
			if !allowed[m.To] {
				return Settlement{}, false, reject("flush moved funds to %s, which is neither the treasury's nor a recipient's token account", m.To)
			}
			amt, err := units(m.Amount)
			if err != nil {
				return Settlement{}, false, err
			}
			cur, ok := got[m.To]
			if !ok {
				cur = money.Zero
			}
			if got[m.To], err = cur.Add(amt); err != nil {
				return Settlement{}, false, err
			}
			if gross, err = gross.Add(amt); err != nil {
				return Settlement{}, false, err
			}
		}
		if gross.IsZero() {
			return Settlement{}, false, nil
		}
		rule, want, err := sp.expectFlows(gross)
		if err != nil {
			return Settlement{}, false, err
		}
		if rule.Dust || !matches(want, got) {
			return Settlement{}, false, reject("flush of %s does not follow the fee and split rule the forwarder commits to", gross)
		}
		return Settlement{Gross: gross, Fee: rule.Fee, Net: rule.Net, Payouts: rule.Shares, Index: i}, true, nil
	}
	return Settlement{}, false, nil
}
