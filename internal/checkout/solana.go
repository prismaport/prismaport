package checkout

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// Error codes of the Solana wallet payment endpoints.
const (
	CodeTransactionRejected = "transaction_rejected"
	// CodeDestinationNotReady means a recipient wallet's token account for the asset does not
	// exist. The gateway never pays its rent, so a wallet payment cannot be built until it does.
	CodeDestinationNotReady = "destination_not_ready"
	// CodePaymentInProgress means a wallet transaction built for the session can still land.
	CodePaymentInProgress = "payment_in_progress"
)

// SolanaChain is the connected-wallet configuration of one Solana cluster: the gateway's fee
// payer signs every transaction it builds, so payers need no SOL.
type SolanaChain struct {
	Chain       chain.Chain
	Quorum      *solwatch.Quorum
	FeePayer    *solana.Keypair
	PriorityFee uint64
}

// WithSolana enables the connected-wallet path on the given clusters.
func (c *Checkout) WithSolana(chains map[string]SolanaChain) *Checkout {
	c.sol = chains
	return c
}

func (c *Checkout) walletAvailable(chainID string) bool {
	sc, ok := c.sol[chainID]
	return ok && sc.Quorum != nil && sc.FeePayer != nil
}

// walletTarget is everything a connected-wallet payment needs for one (session, asset).
type walletTarget struct {
	signedTarget
	sc                      SolanaChain
	mint, tre               solana.PublicKey
	treATA, reference       solana.PublicKey
	recipients, recipientTA []solana.PublicKey
	bps                     []uint16
}

func (c *Checkout) walletTarget(ctx context.Context, id, chainID, asset string) (walletTarget, error) {
	var t walletTarget
	ses, err := c.load(ctx, id)
	if err != nil {
		return t, err
	}
	c.seen(ctx, ses.ID)
	t.ses = ses
	if err := noWalletPath(ses); err != nil {
		return t, err
	}
	found := false
	for _, p := range ses.PayTo {
		if p.Chain == chainID && p.Asset == asset {
			t.pt, found = p, true
		}
	}
	if !found || t.pt.Program == "" {
		return t, errf(service.KindInvalid, service.CodeInvalidRequest, "this session does not accept %s on %s", asset, chainID)
	}
	if err := c.svc.CheckPayable(ctx, chainID, asset); err != nil {
		return t, err
	}
	if !c.walletAvailable(chainID) {
		return t, errf(service.KindUnavailable, service.CodeServiceUnavailable, "wallet payments are not available on %s", chainID)
	}
	t.sc = c.sol[chainID]
	if t.mint, err = solana.ParsePublicKey(t.pt.Token); err != nil {
		return t, err
	}
	if len(t.pt.Recipients) == 0 {
		return t, errf(service.KindInvalid, service.CodeInvalidRequest, "this session has no recipients on %s", chainID)
	}
	for _, r := range t.pt.Recipients {
		w, err := solana.ParsePublicKey(r.Address)
		if err != nil {
			return t, err
		}
		ata, err := solana.AssociatedTokenAddress(w, t.mint)
		if err != nil {
			return t, err
		}
		t.recipients, t.recipientTA, t.bps = append(t.recipients, w), append(t.recipientTA, ata), append(t.bps, r.Bps)
	}
	if t.tre, err = solana.ParsePublicKey(t.pt.Treasury); err != nil {
		return t, err
	}
	if t.treATA, err = solana.AssociatedTokenAddress(t.tre, t.mint); err != nil {
		return t, err
	}
	t.reference = solana.SessionReference(ses.Salt)
	if t.ses, err = c.walletReveal(ctx, t.ses, chainID); err != nil {
		return t, err
	}
	return t, nil
}

// instructions are the payer's transfers, behind a compute budget: one per non-zero share into
// each recipient's token account, in recipient order, the first carrying the session reference
// key (the first share is never zero), then the fee into the treasury's token account. Only the
// treasury's account is created idempotently (it is ours); every recipient's must already exist,
// so the gateway never funds rent for a merchant account.
func (t walletTarget) instructions(payer solana.PublicKey, st payout.Settlement, limit uint32) ([]solana.Instruction, error) {
	src, err := solana.AssociatedTokenAddress(payer, t.mint)
	if err != nil {
		return nil, err
	}
	feePayer := t.sc.FeePayer.PublicKey()
	ixs := []solana.Instruction{solana.SetComputeUnitLimit(limit)}
	if t.sc.PriorityFee > 0 {
		ixs = append(ixs, solana.SetComputeUnitPrice(t.sc.PriorityFee))
	}
	for i, share := range st.Shares {
		if share.IsZero() {
			continue
		}
		var extra []solana.PublicKey
		if i == 0 {
			extra = []solana.PublicKey{t.reference}
		}
		ixs = append(ixs, solana.TransferChecked(solana.TokenProgramID, src, t.mint, t.recipientTA[i], payer, uint64Of(share), t.pt.Decimals, extra...))
	}
	if feeAmt := uint64Of(st.Fee); feeAmt > 0 {
		ixs = append(ixs,
			solana.CreateATAIdempotent(feePayer, t.treATA, t.tre, t.mint, solana.TokenProgramID),
			solana.TransferChecked(solana.TokenProgramID, src, t.mint, t.treATA, payer, feeAmt, t.pt.Decimals))
	}
	return ixs, nil
}

func uint64Of(u money.Units) uint64 {
	b := u.Big()
	if !b.IsUint64() {
		return 0
	}
	return b.Uint64()
}

func (t walletTarget) sign(blockhash [32]byte, ixs []solana.Instruction) (*solana.Transaction, error) {
	msg, err := solana.Compile(t.sc.FeePayer.PublicKey(), blockhash, ixs)
	if err != nil {
		return nil, err
	}
	tx := solana.NewTransaction(msg)
	return tx, tx.Sign(t.sc.FeePayer)
}

// WalletTxRequest asks for a wallet transaction that pays a session in one asset from the payer's account.
type WalletTxRequest struct {
	Chain string
	Asset string
	Payer string
}

// WalletTxResponse is the unsigned wallet transaction that pays a session, with the details a wallet shows.
type WalletTxResponse struct {
	SessionID            string `json:"session_id"`
	Chain                string `json:"chain"`
	Asset                string `json:"asset"`
	Amount               string `json:"amount"`
	FeePayer             string `json:"fee_payer"`
	Reference            string `json:"reference"`
	Transaction          string `json:"transaction"`
	LastValidBlockHeight uint64 `json:"last_valid_block_height"`
}

// walletUnavailable is the 503 of a wallet payment that cannot be prepared now; the cause stays on
// the error for the server log.
func walletUnavailable(cause error) error {
	return &service.Error{Kind: service.KindUnavailable, Code: service.CodeServiceUnavailable,
		Message: "wallet payments are temporarily unavailable", Cause: cause}
}

func parsePayer(s string, feePayer solana.PublicKey) (solana.PublicKey, error) {
	k, err := solana.ParsePublicKey(s)
	if err != nil || k.IsZero() {
		return k, errf(service.KindInvalid, service.CodeInvalidRequest, "account must be the payer's Solana wallet address")
	}
	if k == feePayer {
		return k, errf(service.KindInvalid, service.CodeInvalidRequest, "account must be the payer's own wallet")
	}
	return k, nil
}

// WalletWindow is how long a built transaction is taken to stay valid at least (its blockhash
// lives about 150 blocks): no other is built for the session meanwhile, and none is built for a
// session that expires sooner.
const WalletWindow = store.SolanaWalletWindow

// MaxValidBlocks bounds how far past the agreed block height a new transaction's last valid
// height may be.
const MaxValidBlocks = 300

var (
	errInProgress = errf(service.KindConflict, CodePaymentInProgress,
		"a payment for this checkout is already in progress. If you cancelled it, wait a minute and try again")
	errExpiresSoon = errf(service.KindConflict, CodeSessionNotPayable,
		"this checkout expires in less than two minutes. Ask the merchant for a new link")
)

// WalletTransaction builds the payment transaction for what is still due, signed by the fee
// payer, for the payer's wallet to sign and send (or to post back to WalletSubmit). A session has
// at most one such transaction that can still land: while it does, the same payer asking again
// for the same amount gets it back, and anyone else is refused (payment_in_progress). Otherwise
// two wallets could both pay the session, and presigned transactions minted in bulk would each
// cost the fee payer its network fee when they fail.
func (c *Checkout) WalletTransaction(ctx context.Context, id string, in WalletTxRequest) (WalletTxResponse, error) {
	out, _, _, err := c.buildWalletTx(ctx, id, in)
	return out, err
}

// walletCheck is what the payer's account does not change: the session can be paid now and for
// the whole window, the amount settles, every paid recipient's token account exists, and no
// other transaction is in flight. It returns what is due, its split, the chain's block height and
// the transaction in flight, if any (busy).
type walletCheck struct {
	gross  money.Units
	split  payout.Settlement
	height uint64
	prev   store.WalletTx
	busy   bool
}

func (c *Checkout) checkWallet(ctx context.Context, t walletTarget) (walletCheck, error) {
	var w walletCheck
	gross, err := c.payable(t.signedTarget)
	if err != nil {
		return w, err
	}
	if t.ses.State.ExpiresAt.Sub(c.st.Now()) < WalletWindow {
		return w, errExpiresSoon
	}
	w.gross = gross
	if w.split, err = payout.Compute(gross, t.pt.Decimals, t.pt.Fee, t.bps); err != nil {
		return w, err
	}
	if w.split.Dust {
		return w, errf(service.KindConflict, service.CodeAmountBelowMinimum, "the amount is too small to settle after the fee and cannot be paid from a wallet")
	}
	q := t.sc.Quorum
	for i, ata := range t.recipientTA {
		if w.split.Shares[i].IsZero() {
			continue
		}
		acc, err := q.Account(ctx, ata.String(), solrpc.Confirmed, 0)
		if err != nil {
			return w, walletUnavailable(fmt.Errorf("recipient token account %s: %w", ata, err))
		}
		if !acc.Exists || !solana.TokenAccountUsable(acc.Owner, acc.Data, t.mint) {
			return w, errf(service.KindConflict, CodeDestinationNotReady,
				"this merchant cannot receive %s from a connected wallet yet; send it to the deposit address instead", t.pt.Asset)
		}
	}
	// The agreed block height (the lower of two providers, D49: no slot reads) is the safety
	// bound of everything below: whether the transaction in flight can still land, and how far a
	// provider's blockhash may claim to stay valid.
	height, err := q.BlockHeight(ctx)
	if err != nil {
		return w, walletUnavailable(fmt.Errorf("block height: %w", err))
	}
	w.height = height
	prev, err := c.st.GetWalletTx(ctx, t.ses.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return w, err
	default:
		w.prev, w.busy = prev, prev.Outstanding(c.st.Now(), w.height, WalletWindow)
		if !w.busy && !c.walletTxGone(ctx, q, prev) {
			// It may have landed with the detector still behind: another wallet paying now would pay
			// twice.
			w.busy = true
		}
	}
	return w, nil
}

// walletTxGone reports whether an expired wallet transaction surely never landed: both providers
// know nothing of it, or only that it failed. Anything less, an unreadable transaction or a
// provider that cannot answer, keeps it in flight.
func (c *Checkout) walletTxGone(ctx context.Context, q *solwatch.Quorum, prev store.WalletTx) bool {
	tx, err := solana.ParseTransactionBase64(prev.Transaction)
	if err != nil {
		return false
	}
	landed, err := q.Landed(ctx, tx.ID())
	return err == nil && !landed
}

// buildWalletTx is WalletTransaction, also returning the session's target and the gross amount
// the transaction pays.
func (c *Checkout) buildWalletTx(ctx context.Context, id string, in WalletTxRequest) (WalletTxResponse, walletTarget, money.Units, error) {
	t, err := c.walletTarget(ctx, id, in.Chain, in.Asset)
	if err != nil {
		return WalletTxResponse{}, t, money.Zero, err
	}
	payer, err := parsePayer(in.Payer, t.sc.FeePayer.PublicKey())
	if err != nil {
		return WalletTxResponse{}, t, money.Zero, err
	}
	w, err := c.checkWallet(ctx, t)
	if err != nil {
		return WalletTxResponse{}, t, money.Zero, err
	}
	if w.busy {
		p := w.prev
		// The same transaction can land only once, so handing it out again costs nothing; one past its
		// last valid height could not land at all.
		if p.Payer == payer.String() && p.Chain == t.pt.Chain && p.Asset == t.pt.Asset && p.Gross.Equal(w.gross) &&
			w.height <= p.LastValidHeight {
			return t.walletResponse(w.gross, p.Transaction, p.LastValidHeight), t, w.gross, nil
		}
		return WalletTxResponse{}, t, money.Zero, errInProgress
	}
	out, err := c.walletTx(ctx, t, payer, w)
	return out, t, w.gross, err
}

func (t walletTarget) walletResponse(gross money.Units, b64 string, lastValid uint64) WalletTxResponse {
	return WalletTxResponse{SessionID: t.ses.ID, Chain: t.pt.Chain, Asset: t.pt.Asset, Amount: money.FormatDecimal(gross, t.pt.Decimals),
		FeePayer: t.sc.FeePayer.PublicKey().String(), Reference: t.reference.String(), Transaction: b64, LastValidBlockHeight: lastValid}
}

// walletTx builds and simulates the payment from payer, signs it as fee payer and records it as
// the session's transaction in flight.
func (c *Checkout) walletTx(ctx context.Context, t walletTarget, payer solana.PublicKey, w walletCheck) (WalletTxResponse, error) {
	q := t.sc.Quorum
	bh, err := q.LatestBlockhash(ctx)
	if err != nil {
		return WalletTxResponse{}, walletUnavailable(fmt.Errorf("latest blockhash: %w", err))
	}
	hash, err := solana.ParseBlockhash(bh.Hash)
	if err != nil {
		return WalletTxResponse{}, walletUnavailable(fmt.Errorf("latest blockhash: %w", err))
	}
	// A blockhash lives about 150 blocks. A provider claiming far more would keep the session
	// locked against wallet payments until it expires.
	if bh.LastValidBlockHeight > w.height+MaxValidBlocks {
		return WalletTxResponse{}, walletUnavailable(fmt.Errorf("blockhash valid to %d, more than %d blocks past height %d",
			bh.LastValidBlockHeight, MaxValidBlocks, w.height))
	}
	ixs, err := t.instructions(payer, w.split, 200_000)
	if err != nil {
		return WalletTxResponse{}, err
	}
	probe, err := t.sign(hash, ixs)
	if err != nil {
		return WalletTxResponse{}, err
	}
	raw, err := probe.Serialize()
	if err != nil {
		return WalletTxResponse{}, err
	}
	// The payer has not signed yet, so the simulation skips signature checks; it still proves
	// the payer holds the funds and every account is what we expect.
	sim, err := q.Simulate(ctx, raw, false)
	if err != nil {
		return WalletTxResponse{}, walletUnavailable(fmt.Errorf("simulation: %w", err))
	}
	if sim.Failed() {
		return WalletTxResponse{}, errf(service.KindInvalid, CodeTransactionRejected, "the payment would fail on chain: %s", sim.Err)
	}
	limit := uint32(min(200_000, sim.UnitsConsumed+sim.UnitsConsumed/5+2_000))
	if ixs, err = t.instructions(payer, w.split, limit); err != nil {
		return WalletTxResponse{}, err
	}
	tx, err := t.sign(hash, ixs)
	if err != nil {
		return WalletTxResponse{}, err
	}
	b64, err := tx.Base64()
	if err != nil {
		return WalletTxResponse{}, err
	}
	ok, err := c.svc.ReserveWalletTx(ctx, store.WalletTx{SessionID: t.ses.ID, Chain: t.pt.Chain, Asset: t.pt.Asset, Payer: payer.String(),
		Gross: w.gross, Transaction: b64, LastValidHeight: bh.LastValidBlockHeight}, w.height, WalletWindow)
	if err != nil {
		return WalletTxResponse{}, err
	}
	if !ok {
		return WalletTxResponse{}, errInProgress
	}
	return t.walletResponse(w.gross, b64, bh.LastValidBlockHeight), nil
}

// WalletSubmitRequest carries the signed wallet transaction the payer submits for a session.
type WalletSubmitRequest struct {
	Chain       string
	Asset       string
	Transaction string
}

// walletGross reads the transfers back out of a transaction this checkout built: what reached
// each recipient's and the treasury's token account must be exactly the fee and split rule of
// the total, and the first recipient's transfer must carry the session reference.
func (t walletTarget) walletGross(tx *solana.Transaction) (gross money.Units, payer solana.PublicKey, ok bool) {
	m := tx.Message
	if m.Header.RequiredSignatures != 2 || len(m.AccountKeys) < 2 || m.AccountKeys[0] != t.sc.FeePayer.PublicKey() {
		return money.Zero, payer, false
	}
	payer = m.AccountKeys[1]
	got := map[solana.PublicKey]uint64{}
	allowed := map[solana.PublicKey]bool{t.treATA: true}
	for _, a := range t.recipientTA {
		allowed[a] = true
	}
	hasRef := false
	var total uint64
	for _, ix := range m.Instructions {
		if m.AccountKeys[ix.ProgramIndex] != solana.TokenProgramID || len(ix.Data) != 10 || ix.Data[0] != 12 || len(ix.Accounts) < 4 {
			continue
		}
		to := m.AccountKeys[ix.Accounts[2]]
		if !allowed[to] {
			return money.Zero, payer, false
		}
		amount := binary.LittleEndian.Uint64(ix.Data[1:9])
		if total+amount < total {
			return money.Zero, payer, false
		}
		got[to] += amount
		total += amount
		if to == t.recipientTA[0] {
			for _, a := range ix.Accounts[4:] {
				hasRef = hasRef || m.AccountKeys[a] == t.reference
			}
		}
	}
	gross = money.FromUint64(total)
	rule, err := payout.Compute(gross, t.pt.Decimals, t.pt.Fee, t.bps)
	if err != nil || rule.Dust || !hasRef {
		return money.Zero, payer, false
	}
	want := map[solana.PublicKey]uint64{t.treATA: uint64Of(rule.Fee)}
	for i, a := range t.recipientTA {
		want[a] += uint64Of(rule.Shares[i])
	}
	for a, w := range want {
		if got[a] != w {
			return money.Zero, payer, false
		}
	}
	return gross, payer, true
}

// WalletSubmit takes a payment transaction this checkout built, now signed by the payer, and
// queues it for broadcasting. Only transactions carrying the fee payer's own signature are
// accepted, so the server never relays a message it did not build.
func (c *Checkout) WalletSubmit(ctx context.Context, id string, in WalletSubmitRequest) (SubmitResponse, error) {
	tx, err := solana.ParseTransactionBase64(in.Transaction)
	if err != nil {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "transaction must be a base64 legacy Solana transaction")
	}
	t, err := c.walletTarget(ctx, id, in.Chain, in.Asset)
	if err != nil {
		return SubmitResponse{}, err
	}
	gross, payer, ok := t.walletGross(tx)
	if !ok || !tx.VerifySignature(t.sc.FeePayer.PublicKey()) {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "this is not a payment transaction built for this session")
	}
	if !tx.VerifySignature(payer) {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "the payer has not signed the transaction")
	}
	if _, err := c.payable(t.signedTarget); err != nil {
		return SubmitResponse{}, err
	}
	if err := c.notDust(t.signedTarget, gross); err != nil {
		return SubmitResponse{}, err
	}
	q := t.sc.Quorum
	valid, err := q.IsBlockhashValid(ctx, base58.Encode(tx.Message.RecentBlockhash[:]))
	if err != nil {
		return SubmitResponse{}, walletUnavailable(fmt.Errorf("blockhash validity: %w", err))
	}
	if !valid {
		return SubmitResponse{}, errf(service.KindInvalid, CodeTransactionRejected, "the transaction expired; request a new one")
	}
	// Agreed (the lower of two providers): the job gives up only once the transaction surely
	// cannot land.
	height, err := q.BlockHeight(ctx)
	if err != nil {
		return SubmitResponse{}, walletUnavailable(fmt.Errorf("block height: %w", err))
	}
	raw, err := tx.Serialize()
	if err != nil {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "%v", err)
	}
	sim, err := q.Simulate(ctx, raw, true)
	if err != nil {
		return SubmitResponse{}, walletUnavailable(fmt.Errorf("simulation: %w", err))
	}
	if sim.Failed() && string(sim.Err) != `"AlreadyProcessed"` {
		return SubmitResponse{}, errf(service.KindInvalid, CodeTransactionRejected, "the payment would fail on chain: %s", sim.Err)
	}
	// A blockhash stays valid for 150 blocks after its own; counting from now over-estimates
	// that bound, so the job only gives up once the transaction surely cannot land.
	lastValid := height + 151
	j, err := c.svc.QueueSettle(ctx, store.TxJob{Chain: in.Chain, Ref: tx.ID(), SessionID: id, Asset: in.Asset,
		To: solana.TokenProgramID.String(), Calldata: raw,
		Details: map[string]string{"payer": payer.String(), "gross": gross.String(), "reference": t.reference.String(),
			"last_valid_height": strconv.FormatUint(lastValid, 10)}})
	if err != nil {
		return SubmitResponse{}, err
	}
	return SubmitResponse{ID: j.ID, SessionID: id, Status: j.Status}, nil
}
