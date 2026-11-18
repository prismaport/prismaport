package solsender

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
)

// forwarderOf re-derives the forwarder of a pay_to entry from its stored salt and params,
// including the mints it binds (every Solana asset the session offered, D41).
func forwarderOf(e store.WatchEntry) (solana.Forwarder, error) {
	program, err := solana.ParsePublicKey(e.Program)
	if err != nil {
		return solana.Forwarder{}, fmt.Errorf("program: %w", err)
	}
	p, err := derive.SolanaParams(derive.Params{Treasury: e.Treasury, RentReceiver: e.RentReceiver, Fee: e.Fee, Recipients: e.Recipients,
		Tokens: e.Tokens})
	if err != nil {
		return solana.Forwarder{}, err
	}
	return solana.NewForwarder(program, e.Salt, p)
}

func decodeRecipients(v string) ([]payout.Recipient, error) {
	var out []payout.Recipient
	for _, part := range strings.Split(v, ",") {
		addr, bps, ok := strings.Cut(part, ":")
		n, err := strconv.ParseUint(bps, 10, 16)
		if !ok || err != nil || addr == "" {
			return nil, errors.New("malformed recipients")
		}
		out = append(out, payout.Recipient{Address: addr, Bps: uint16(n)})
	}
	return out, nil
}

// jobForwarder rebuilds the forwarder a flush job targets from its details.
func jobForwarder(j store.TxJob) (solana.Forwarder, solana.PublicKey, solana.PublicKey, error) {
	var zero solana.Forwarder
	salt, err := hex.DecodeString(j.Details["salt"])
	if err != nil || len(salt) != 32 {
		return zero, solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("flush job %s: bad salt", j.ID)
	}
	bps, err1 := strconv.ParseUint(j.Details["fee_bps"], 10, 16)
	fixedFee, err2 := strconv.ParseUint(j.Details["fixed_fee_micros"], 10, 64)
	if err1 != nil || err2 != nil {
		return zero, solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("flush job %s: bad fee params", j.ID)
	}
	rs, err := decodeRecipients(j.Details["recipients"])
	if err != nil {
		return zero, solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("flush job %s: %w", j.ID, err)
	}
	if j.Details["mints"] == "" {
		return zero, solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("flush job %s: no bound mints", j.ID)
	}
	f, err := forwarderOf(store.WatchEntry{Program: j.Details["program"], Recipients: rs, Token: j.Details["mint"],
		Treasury: j.Details["treasury"], RentReceiver: j.Details["rent_receiver"], Salt: [32]byte(salt),
		Fee: fee.Params{FeeBps: uint16(bps), FixedFeeMicros: fixedFee}, Tokens: strings.Split(j.Details["mints"], ",")})
	if err != nil {
		return zero, solana.PublicKey{}, solana.PublicKey{}, err
	}
	if f.Address.String() != j.Details["owner"] {
		return zero, solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("flush job %s: forwarder does not re-derive", j.ID)
	}
	mint, err := solana.ParsePublicKey(j.Details["mint"])
	if err != nil {
		return zero, solana.PublicKey{}, solana.PublicKey{}, err
	}
	ata, err := solana.ParsePublicKey(j.Address)
	return f, mint, ata, err
}

// tokenBalance is the agreed classic-token balance of an account (zero when it does not exist).
func (s *Sender) tokenBalance(ctx context.Context, account solana.PublicKey) (money.Units, error) {
	acc, err := s.q.Account(ctx, account.String(), solrpc.Confirmed, 0)
	if err != nil {
		return money.Zero, err
	}
	if !acc.Exists || acc.Owner != solana.TokenProgramID.String() {
		return money.Zero, nil
	}
	amt, _, _, ok := solana.TokenAmount(acc.Data)
	if !ok {
		return money.Zero, nil
	}
	return money.FromUint64(amt), nil
}

// tokenAccountReady reports whether account is an agreed, usable classic token account of
// mint (existing, initialized, not frozen).
func (s *Sender) tokenAccountReady(ctx context.Context, account, mint solana.PublicKey) (bool, error) {
	acc, err := s.q.Account(ctx, account.String(), solrpc.Confirmed, 0)
	if err != nil {
		return false, err
	}
	return acc.Exists && solana.TokenAccountUsable(acc.Owner, acc.Data, mint), nil
}

// forwarderBps lists the shares a forwarder commits to, in recipient order.
func forwarderBps(f solana.Forwarder) []uint16 {
	out := make([]uint16, len(f.Params.Recipients))
	for i, r := range f.Params.Recipients {
		out[i] = r.Bps
	}
	return out
}

// missingRecipient is a recipient whose token account for the flush's mint is not usable.
type missingRecipient struct {
	Index  int
	Wallet solana.PublicKey
	ATA    solana.PublicKey
}

// missingRecipients lists the recipients of a flush job whose token account does not exist (or
// is frozen or not a classic token account of the mint). shares, when known, skips recipients
// whose share of this flush is zero: flush pays them nothing and does not need their account.
func (s *Sender) missingRecipients(ctx context.Context, f solana.Forwarder, mint solana.PublicKey, shares []money.Units) ([]missingRecipient, error) {
	atas, err := f.RecipientATAs(mint)
	if err != nil {
		return nil, err
	}
	var out []missingRecipient
	for i, ata := range atas {
		if shares != nil && shares[i].IsZero() {
			continue
		}
		ready, err := s.tokenAccountReady(ctx, ata, mint)
		if err != nil {
			return nil, err
		}
		if !ready {
			out = append(out, missingRecipient{Index: i, Wallet: f.Params.Recipients[i].Wallet, ATA: ata})
		}
	}
	return out, nil
}

// jobMissing checks a flush job's recipients against the deposit account's current balance.
func (s *Sender) jobMissing(ctx context.Context, j store.TxJob) ([]missingRecipient, error) {
	f, mint, ata, err := jobForwarder(j)
	if err != nil {
		return nil, err
	}
	bal, err := s.tokenBalance(ctx, ata)
	if err != nil {
		return nil, err
	}
	var shares []money.Units
	if rule, err := payout.Compute(bal, s.assets[j.Asset].Decimals, f.Params.Fee, forwarderBps(f)); err == nil && !rule.Dust {
		shares = rule.Shares
	}
	return s.missingRecipients(ctx, f, mint, shares)
}

// block parks a flush whose recipient token accounts are not all there. Nothing is sent: the
// funds stay in the forwarder and the job is checked again after BlockedRecheck.
func (s *Sender) block(ctx context.Context, j store.TxJob, missing []missingRecipient) error {
	now := s.st.Now()
	var accounts, wallets []string
	for _, m := range missing {
		accounts = append(accounts, m.ATA.String())
		wallets = append(wallets, fmt.Sprintf("recipient %d (%s)", m.Index+1, m.Wallet))
	}
	j.Status = store.JobBlocked
	j.LastError = "recipient token account(s) " + strings.Join(accounts, ", ") + " do not exist"
	j.RawTx, j.SubmittedAt, j.LastValidHeight = nil, nil, nil
	j.NextAttemptAt = now.Add(s.cfg.BlockedRecheck)
	if last, ok := s.alerted[j.ID]; !ok || now.Sub(last) >= blockedAlertEvery {
		s.alerted[j.ID] = now
		s.jobs.Alert(ctx, alert.Warning, "recipient_ata_missing",
			fmt.Sprintf("%s of the merchant's split has no %s token account, so the payment stays in its deposit address and no recipient is paid; each must receive any amount of %s once to create it, then the flush resumes on its own",
				strings.Join(wallets, ", "), j.Asset, j.Asset),
			map[string]string{"job": j.ID, "session": j.SessionID, "asset": j.Asset, "recipients": strings.Join(wallets, ", "),
				"token_accounts": strings.Join(accounts, ", "), "deposit_account": j.Address})
	}
	s.log.Warn("flush blocked: recipient token account missing", "chain", s.chainID(), "job", j.ID, "session", j.SessionID,
		"token_accounts", strings.Join(accounts, ", "))
	return s.st.UpdateTxJob(ctx, &j)
}

// recheckBlocked moves blocked flushes whose recipient token accounts now all exist back to
// pending; the others wait for the next check.
func (s *Sender) recheckBlocked(ctx context.Context) (int, error) {
	jobs, err := s.st.TxJobs(ctx, s.chainID(), store.JobBlocked)
	if err != nil {
		return 0, err
	}
	now, n := s.st.Now(), 0
	for _, j := range jobs {
		if j.NextAttemptAt.After(now) {
			continue
		}
		missing, err := s.jobMissing(ctx, j)
		if err != nil {
			return n, err
		}
		if len(missing) > 0 {
			if err := s.block(ctx, j, missing); err != nil {
				return n, err
			}
			continue
		}
		delete(s.alerted, j.ID)
		j.Status, j.LastError, j.NextAttemptAt = store.JobPending, "", now
		if err := s.st.UpdateTxJob(ctx, &j); err != nil {
			return n, err
		}
		s.log.Info("recipient token accounts exist; flush resumes", "chain", s.chainID(), "job", j.ID, "session", j.SessionID)
		n++
	}
	return n, nil
}

// blockIfRecipientGone blocks a failed flush whose recipient token account disappeared (a
// merchant closed it after the check), instead of counting it as a failure.
func (s *Sender) blockIfRecipientGone(ctx context.Context, j store.TxJob) (bool, error) {
	if j.Kind != store.JobFlush {
		return false, nil
	}
	missing, err := s.jobMissing(ctx, j)
	if err != nil || len(missing) == 0 {
		return false, err
	}
	return true, s.block(ctx, j, missing)
}
