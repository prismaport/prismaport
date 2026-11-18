package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/session"
)

// PayToRecord is a session pay_to entry plus the inputs its address was derived from.
type PayToRecord struct {
	Chain    string
	Asset    string
	Family   chain.Family
	Token    string
	Decimals uint8
	Address  string
	Amount   money.Units
	// Recipients are the session's payout snapshot for the entry's family, in committed order.
	Recipients []payout.Recipient
	Treasury   string
	Fee        fee.Params
	// Program and RentReceiver are what a Solana forwarder address also commits to; empty on EVM.
	Program      string
	RentReceiver string
	// BoundTokens are the tokens the forwarder binds (D41): every asset the session offers on the
	// chain, in canonical order (derive.BoundTokens). Every entry of a session on one chain has the
	// same list.
	BoundTokens []string
}

// SessionPayout is one recipient of a session's payout snapshot: copied from the product's payout
// when the session opened and committed in its deposit addresses and signed nonces.
type SessionPayout struct {
	// Chain is the network this recipient list is for; Family is that chain's family.
	Chain    string
	Family   chain.Family
	Position int
	// WalletID says where the recipient came from; Label is the wallet's label now.
	WalletID *string
	Label    *string
	Address  string
	Bps      uint16
}

// SettlementPayout is what one recipient received in a settlement. Position is the recipient's
// index in the session's snapshot; recipients whose share was zero have no payout.
type SettlementPayout struct {
	Position int
	WalletID *string
	Label    *string
	Address  string
	Amount   money.Units
}

// Settlement is the on-chain outcome of a session payment, split into gross, fee and net.
type Settlement struct {
	ID        string
	Chain     string
	Asset     string
	Decimals  uint8
	Gross     money.Units
	Fee       money.Units
	Net       money.Units
	TxHash    string
	LogIndex  uint32
	SettledAt time.Time
	// Block is the block of the settling transaction (EVM; 0 when unknown). The emptied token's
	// balance is recorded at that block, so a balance read from before it is ignored. Reach bounds
	// how many blocks that zero moves checked_block back (as FlushMined's reach; not stored).
	Block uint64
	Reach uint64
	// Direct is a split the payer's own transaction made (EVM signed path through the Gateway,
	// Solana connected wallet): nothing left the deposit address, so its watched EVM balance is
	// left as read (a zero stamped over a partial deposit there would read as an unexplained
	// rise). Not stored.
	Direct bool
	// Shares is the observed amount per recipient of the snapshot, in recipient order (zero for a
	// share that moved nothing): RecordSettlement's input, checked against the split rule.
	Shares []money.Units
	// Payouts are the stored per-recipient rows.
	Payouts []SettlementPayout
	// Simulated settlements are made by the test-mode simulator; nothing moved on chain.
	Simulated bool
}

// Session is a checkout session with its owner, mode and current state.
type Session struct {
	ID         string
	MerchantID string
	Livemode   bool
	ProductID  *string
	// LinkID is the payment link whose page opened the session; nil for API sessions.
	LinkID *string
	// CustomerAddressID is the static customer address the session was opened on (B11); such a
	// session carries the address's salt, pay_to addresses and payout snapshot.
	CustomerAddressID *string
	Metadata          map[string]string
	SuccessURL        *string
	// ClientReferenceID correlates the session with something of the merchant's; Source says
	// who set it: ReferenceMerchant (the API, trusted) or ReferencePayer (a link URL, untrusted).
	ClientReferenceID     *string
	ClientReferenceSource *string
	Salt                  [32]byte
	CreatedAt             time.Time
	UpdatedAt             time.Time
	PayTo                 []PayToRecord
	// Payouts is the payout snapshot of every chain the session offers, ordered by chain and
	// position.
	Payouts     []SessionPayout
	Settlements []Settlement
	State       session.State
	// PayWindow is how long the session stays open after its first reveal (expires_in, D48);
	// PaymentStartedAt is that first reveal (nil while no network was revealed). Reveals are the
	// networks whose deposit details were shown, in reveal order.
	PayWindow        time.Duration
	PaymentStartedAt *time.Time
	Reveals          []Reveal
}

// Recipients returns the snapshot of one chain in committed order.
func (s *Session) Recipients(chainID string) []payout.Recipient {
	var out []payout.Recipient
	for _, p := range s.Payouts {
		if p.Chain == chainID {
			out = append(out, payout.Recipient{Address: p.Address, Bps: p.Bps})
		}
	}
	return out
}

// PayToState projects pay_to records onto the state machine's view.
func PayToState(recs []PayToRecord) []session.PayTo {
	out := make([]session.PayTo, len(recs))
	for i, r := range recs {
		out[i] = session.PayTo{Chain: r.Chain, Asset: r.Asset, Address: r.Address, Decimals: r.Decimals, Amount: r.Amount, Fee: r.Fee}
	}
	return out
}

// InsertSession writes a new session and its pay_to rows inside tx. An open-amount session stores
// no amount, on the session or its pay_to rows.
func (s *Store) InsertSession(ctx context.Context, tx pgx.Tx, ses *Session) error {
	st := ses.State
	if ses.PayWindow < time.Second {
		return fmt.Errorf("store: session %s has a pay window of %s", ses.ID, ses.PayWindow)
	}
	if err := st.Validate(); err != nil {
		return fmt.Errorf("store: session %s: %w", ses.ID, err)
	}
	amount := func(u money.Units) *string {
		if st.OpenAmount {
			return nil
		}
		v := u.String()
		return &v
	}
	// A session on a customer address shows its addresses from creation (its customer already
	// holds them): every network counts as revealed and the pay window starts now.
	if ses.CustomerAddressID != nil && ses.PaymentStartedAt == nil {
		at := ses.CreatedAt
		ses.PaymentStartedAt = &at
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, merchant_id, livemode, product_id, link_id, amount_cents, status, version,
		salt, metadata, success_url, expires_at, created_at, updated_at, client_reference_id, client_reference_source, customer_address_id,
		pay_window_seconds, payment_started_at)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11, $12, $13, $13, $14, $15, $16, $17, $18)`,
		ses.ID, ses.MerchantID, ses.Livemode, ses.ProductID, ses.LinkID, amount(st.AmountCents), string(st.Status), st.Version,
		ses.Salt[:], metadataArg(ses.Metadata), ses.SuccessURL, st.ExpiresAt, ses.CreatedAt, ses.ClientReferenceID,
		ses.ClientReferenceSource, ses.CustomerAddressID, int32(ses.PayWindow/time.Second), ses.PaymentStartedAt); err != nil {
		return opErr("insert session", err)
	}
	for i, p := range ses.PayTo {
		if _, err := tx.Exec(ctx, `INSERT INTO session_pay_to (session_id, position, chain, asset, family, token, decimals, address,
			amount, treasury, fee_bps, fixed_fee_micros, program, rent_receiver, bound_tokens)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12, $13, $14, $15)`,
			ses.ID, i, p.Chain, p.Asset, string(p.Family), p.Token, int16(p.Decimals), p.Address, amount(p.Amount),
			p.Treasury, int32(p.Fee.FeeBps), int64(p.Fee.FixedFeeMicros), optString(p.Program), optString(p.RentReceiver),
			p.BoundTokens); err != nil {
			return opErr("insert session", err)
		}
		if ses.CustomerAddressID != nil && ses.Revealed(p.Chain) == nil {
			if _, err := tx.Exec(ctx, `INSERT INTO session_reveals (session_id, chain, revealed_at, source) VALUES ($1, $2, $3, $4)`,
				ses.ID, p.Chain, ses.CreatedAt, RevealCustomerAddress); err != nil {
				return opErr("insert session", err)
			}
			ses.Reveals = append(ses.Reveals, Reveal{Chain: p.Chain, RevealedAt: ses.CreatedAt, Source: RevealCustomerAddress})
		}
	}
	for _, p := range ses.Payouts {
		if _, err := tx.Exec(ctx, `INSERT INTO session_payouts (session_id, merchant_id, livemode, chain_id, family, position, wallet_id,
			address, bps) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, ses.ID, ses.MerchantID, ses.Livemode, p.Chain, string(p.Family),
			p.Position, p.WalletID, p.Address, int32(p.Bps)); err != nil {
			return opErr("insert session", err)
		}
	}
	return nil
}

// GetSession returns a merchant's session by id in the given mode, or ErrNotFound when none matches.
func (s *Store) GetSession(ctx context.Context, merchantID string, livemode bool, id string) (*Session, error) {
	ses, err := loadSession(ctx, s.db, id, false)
	if err != nil {
		return nil, opErr("get session", err)
	}
	if ses.MerchantID != merchantID || ses.Livemode != livemode {
		return nil, ErrNotFound
	}
	return ses, nil
}

// LoadSession reads a session without a merchant check; internal callers only.
func (s *Store) LoadSession(ctx context.Context, id string) (*Session, error) {
	out, err := loadSession(ctx, s.db, id, false)
	return out, opErr("load session", err)
}

// loadSession reads a session with its payouts, pay-to records, reveals, payments and
// settlements; forUpdate locks the session row.
func loadSession(ctx context.Context, q querier, id string, forUpdate bool) (*Session, error) {
	ses, err := loadSessionRow(ctx, q, id, forUpdate)
	if err != nil {
		return nil, err
	}
	if ses.Payouts, err = loadPayouts(ctx, q, id); err != nil {
		return nil, err
	}
	if ses.PayTo, err = loadPayTo(ctx, q, id); err != nil {
		return nil, err
	}
	for i := range ses.PayTo {
		ses.PayTo[i].Recipients = ses.Recipients(ses.PayTo[i].Chain)
	}
	ses.State.PayTo = PayToState(ses.PayTo)
	if ses.Reveals, err = sessionReveals(ctx, q, id); err != nil {
		return nil, err
	}
	if ses.State.Payments, err = loadPayments(ctx, q, id); err != nil {
		return nil, err
	}
	if ses.Settlements, err = loadSettlements(ctx, q, id); err != nil {
		return nil, err
	}
	// A stored amount the session's value methods cannot compute with is refused here, as an
	// error, before anything renders or transitions the session.
	if err := ses.State.Validate(); err != nil {
		return nil, fmt.Errorf("store: session %s: %w", id, err)
	}
	return ses, nil
}

// loadSessionRow reads the session's own row.
func loadSessionRow(ctx context.Context, q querier, id string, forUpdate bool) (*Session, error) {
	sql := `SELECT id, merchant_id, livemode, product_id, link_id, amount_cents::text, status, version, salt, metadata,
		success_url, expires_at, expired_at, paid_at, review_reason, created_at, updated_at, client_reference_id,
		client_reference_source, customer_address_id, pay_window_seconds, payment_started_at FROM sessions WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	ses := &Session{}
	var status string
	var amount *string
	var salt []byte
	var review *string
	var payWindow int32
	if err := q.QueryRow(ctx, sql, id).Scan(&ses.ID, &ses.MerchantID, &ses.Livemode, &ses.ProductID, &ses.LinkID, &amount, &status,
		&ses.State.Version, &salt, &ses.Metadata, &ses.SuccessURL, &ses.State.ExpiresAt, &ses.State.ExpiredAt,
		&ses.State.PaidAt, &review, &ses.CreatedAt, &ses.UpdatedAt, &ses.ClientReferenceID, &ses.ClientReferenceSource,
		&ses.CustomerAddressID, &payWindow, &ses.PaymentStartedAt); err != nil {
		return nil, notFound(err)
	}
	ses.PayWindow = time.Duration(payWindow) * time.Second
	var err error
	if amount == nil {
		ses.State.OpenAmount = true
	} else if ses.State.AmountCents, err = parseUnits("amount_cents", *amount); err != nil {
		return nil, err
	}
	ses.State.Status = session.Status(status)
	if !ses.State.Status.Valid() {
		return nil, fmt.Errorf("store: session %s has unknown status %q", id, status)
	}
	if len(salt) != 32 {
		return nil, fmt.Errorf("store: session %s salt has %d bytes", id, len(salt))
	}
	copy(ses.Salt[:], salt)
	if review != nil {
		ses.State.ReviewReason = *review
	}
	return ses, nil
}

// loadPayouts reads the session's payout snapshot, by chain and position.
func loadPayouts(ctx context.Context, q querier, id string) ([]SessionPayout, error) {
	rows, err := q.Query(ctx, `SELECT sp.chain_id, sp.family, sp.position, sp.wallet_id, w.label, sp.address, sp.bps
		FROM session_payouts sp LEFT JOIN wallets w ON w.id = sp.wallet_id
		WHERE sp.session_id = $1 ORDER BY sp.chain_id, sp.position`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SessionPayout, error) {
		var p SessionPayout
		var fam string
		var pos int16
		var bps int32
		if err := r.Scan(&p.Chain, &fam, &pos, &p.WalletID, &p.Label, &p.Address, &bps); err != nil {
			return p, err
		}
		p.Family, p.Position, p.Bps = chain.Family(fam), int(pos), uint16(bps)
		return p, nil
	})
}

// loadPayTo reads the session's pay-to records, by position.
func loadPayTo(ctx context.Context, q querier, id string) ([]PayToRecord, error) {
	rows, err := q.Query(ctx, `SELECT chain, asset, family, token, decimals, address, amount::text, treasury,
		fee_bps, fixed_fee_micros, coalesce(program, ''), coalesce(rent_receiver, ''), bound_tokens FROM session_pay_to
		WHERE session_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayToRecord, error) {
		var p PayToRecord
		var dec int16
		var bps int32
		var fixedFee int64
		var fam string
		var amt *string
		if err := r.Scan(&p.Chain, &p.Asset, &fam, &p.Token, &dec, &p.Address, &amt, &p.Treasury, &bps, &fixedFee,
			&p.Program, &p.RentReceiver, &p.BoundTokens); err != nil {
			return p, err
		}
		p.Family, p.Decimals, p.Fee = chain.Family(fam), uint8(dec), fee.Params{FeeBps: uint16(bps), FixedFeeMicros: uint64(fixedFee)}
		if amt == nil {
			return p, nil
		}
		p.Amount, err = parseUnits("session_pay_to.amount", *amt)
		return p, err
	})
}

// loadPayments reads the session's payments, by detection.
func loadPayments(ctx context.Context, q querier, id string) ([]session.Payment, error) {
	rows, err := q.Query(ctx, `SELECT id, chain, asset, address, tx_hash, log_index, amount::text, decimals, status, late,
		detected_at, confirmed_at, finalized_at, reversed_at, simulated FROM payments WHERE session_id = $1 ORDER BY detected_at, id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (session.Payment, error) {
		var p session.Payment
		var logIndex int32
		var dec int16
		var amt, st string
		if err := r.Scan(&p.ID, &p.Chain, &p.Asset, &p.Address, &p.TxHash, &logIndex, &amt, &dec, &st, &p.Late,
			&p.DetectedAt, &p.ConfirmedAt, &p.FinalizedAt, &p.ReversedAt, &p.Simulated); err != nil {
			return p, err
		}
		p.LogIndex, p.Decimals, p.Status = uint32(logIndex), uint8(dec), session.PaymentStatus(st)
		if !p.Status.Valid() {
			return p, fmt.Errorf("store: payment %s has unknown status %q", p.ID, st)
		}
		p.Amount, err = parseUnits("payments.amount", amt)
		return p, err
	})
}

// loadSettlements reads the session's settlements with their payouts, by settlement time.
func loadSettlements(ctx context.Context, q querier, id string) ([]Settlement, error) {
	payouts, err := settlementPayouts(ctx, q, id)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT s.id, s.chain, s.asset, p.decimals, s.gross::text, s.fee::text, s.net::text, s.tx_hash,
		s.log_index, s.settled_at, s.simulated FROM settlements s
		JOIN session_pay_to p ON p.session_id = s.session_id AND p.chain = s.chain AND p.asset = s.asset
		WHERE s.session_id = $1 ORDER BY s.settled_at, s.id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Settlement, error) {
		var st Settlement
		var dec int16
		var logIndex int32
		var g, f, n string
		if err := r.Scan(&st.ID, &st.Chain, &st.Asset, &dec, &g, &f, &n, &st.TxHash, &logIndex, &st.SettledAt, &st.Simulated); err != nil {
			return st, err
		}
		st.Payouts = payouts[st.ID]
		st.Decimals, st.LogIndex = uint8(dec), uint32(logIndex)
		if st.Gross, err = parseUnits("settlements.gross", g); err != nil {
			return st, err
		}
		if st.Fee, err = parseUnits("settlements.fee", f); err != nil {
			return st, err
		}
		st.Net, err = parseUnits("settlements.net", n)
		return st, err
	})
}

func settlementPayouts(ctx context.Context, q querier, sessionID string) (map[string][]SettlementPayout, error) {
	rows, err := q.Query(ctx, `SELECT p.settlement_id, p.position, p.wallet_id, w.label, p.address, p.amount::text
		FROM settlement_payouts p JOIN settlements s ON s.id = p.settlement_id LEFT JOIN wallets w ON w.id = p.wallet_id
		WHERE s.session_id = $1 ORDER BY p.settlement_id, p.position`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]SettlementPayout{}
	for rows.Next() {
		var sid, amt string
		var pos int16
		var p SettlementPayout
		if err := rows.Scan(&sid, &pos, &p.WalletID, &p.Label, &p.Address, &amt); err != nil {
			return nil, err
		}
		p.Position = int(pos)
		if p.Amount, err = parseUnits("settlement_payouts.amount", amt); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], p)
	}
	return out, rows.Err()
}

// Command is a state machine step.
type Command func(session.State) (session.State, []session.Event, error)

// EventRecord is an outbox row written by a session change.
type EventRecord struct {
	ID             string
	Type           string
	SessionVersion int64
	Payload        []byte
}

// ErrSessionSuperseded refuses to credit a session on a customer address that is no longer the
// address's current session: a newer session owns its deposit addresses and reads them again.
var ErrSessionSuperseded = errors.New("store: a newer session on the customer address owns its deposit addresses")

// ApplySessionCommand runs cmd against the locked session and persists the new state, its
// payments and its events in one transaction. A command error leaves everything untouched.
func (s *Store) ApplySessionCommand(ctx context.Context, sessionID string, cmd Command) (*Session, []EventRecord, error) {
	ses, events, err := s.applySessionCommand(ctx, sessionID, false, cmd)
	return ses, events, opErr("apply session command", err)
}

// ApplyOwnerCommand is ApplySessionCommand for a command that credits the session with funds
// found at its deposit addresses: a session on a customer address must be the address's current
// session (ErrSessionSuperseded otherwise). The address row is read FOR SHARE after the session
// lock, so an open of the next session (which holds it FOR UPDATE) is either committed and seen,
// or waits for this command.
func (s *Store) ApplyOwnerCommand(ctx context.Context, sessionID string, cmd Command) (*Session, []EventRecord, error) {
	ses, events, err := s.applySessionCommand(ctx, sessionID, true, cmd)
	return ses, events, opErr("apply owner command", err)
}

func (s *Store) applySessionCommand(ctx context.Context, sessionID string, owner bool, cmd Command) (*Session, []EventRecord, error) {
	var out *Session
	var events []EventRecord
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := loadSession(ctx, tx, sessionID, true)
		if err != nil {
			return err
		}
		if owner && cur.CustomerAddressID != nil {
			var current *string
			if err := tx.QueryRow(ctx, `SELECT current_session_id FROM customer_addresses WHERE id = $1 FOR SHARE`,
				*cur.CustomerAddressID).Scan(&current); err != nil {
				return err
			}
			if current == nil || *current != cur.ID {
				return ErrSessionSuperseded
			}
		}
		next, evs, err := cmd(cur.State)
		if err != nil {
			return err
		}
		if next.Version == cur.State.Version {
			out = cur
			return nil
		}
		if next.Version != cur.State.Version+1 {
			return fmt.Errorf("store: command moved version %d -> %d", cur.State.Version, next.Version)
		}
		now := s.now()
		updated := *cur
		updated.State = next
		updated.UpdatedAt = now
		if err := persistState(ctx, tx, &updated, cur.State, now); err != nil {
			return err
		}
		for _, e := range evs {
			rec, err := s.insertEvent(ctx, tx, &updated, string(e.Type), e.Version, now)
			if err != nil {
				return err
			}
			events = append(events, rec)
		}
		out = &updated
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, events, nil
}

func persistState(ctx context.Context, tx pgx.Tx, ses *Session, prev session.State, now time.Time) error {
	st := ses.State
	var review *string
	if st.ReviewReason != "" {
		review = &st.ReviewReason
	}
	tag, err := tx.Exec(ctx, `UPDATE sessions SET status = $3, version = $4, expired_at = $5, paid_at = $6,
		review_reason = $7, updated_at = $8 WHERE id = $1 AND version = $2`,
		ses.ID, prev.Version, string(st.Status), st.Version, st.ExpiredAt, st.PaidAt, review, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := keepLastPaid(ctx, tx, ses, prev); err != nil {
		return err
	}
	old := map[string]session.Payment{}
	for _, p := range prev.Payments {
		old[p.ID] = p
	}
	for _, p := range st.Payments {
		before, existed := old[p.ID]
		if !existed {
			_, err = tx.Exec(ctx, `INSERT INTO payments (id, session_id, chain, asset, address, tx_hash, log_index, amount,
				decimals, status, late, detected_at, confirmed_at, finalized_at, reversed_at, simulated)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12, $13, $14, $15, $16)`,
				p.ID, ses.ID, p.Chain, p.Asset, p.Address, p.TxHash, int32(p.LogIndex), p.Amount.String(), int16(p.Decimals),
				string(p.Status), p.Late, p.DetectedAt, p.ConfirmedAt, p.FinalizedAt, p.ReversedAt, p.Simulated)
		} else if before.Status != p.Status {
			_, err = tx.Exec(ctx, `UPDATE payments SET status = $2, confirmed_at = $3, finalized_at = $4, reversed_at = $5
				WHERE id = $1`, p.ID, string(p.Status), p.ConfirmedAt, p.FinalizedAt, p.ReversedAt)
		}
		if err != nil {
			if isUniqueViolation(err) {
				return session.ErrDuplicatePayment
			}
			return err
		}
	}
	return nil
}

// keepLastPaid keeps last_paid_at of the session's product and link (00046) as the latest paid
// time of their paid or overpaid sessions, in the transaction that changes the session. Entering
// paid can only move it later; leaving paid (a reorg) or a paid time that changed recomputes it.
// The product row is locked before the link row, the order every link writer follows. The
// recompute locks the row first and reads in a new statement: under read committed it then sees
// every session committed by a writer that held the row before it.
func keepLastPaid(ctx context.Context, tx pgx.Tx, ses *Session, prev session.State) error {
	st := ses.State
	was, is := paidStatus(prev.Status), paidStatus(st.Status)
	recompute := false
	switch {
	case !was && !is:
		return nil
	case was && is && sameTime(prev.PaidAt, st.PaidAt):
		return nil
	case was:
		recompute = true
	}
	for _, row := range []struct {
		table, col string
		id         *string
	}{{"products", "product_id", ses.ProductID}, {"payment_links", "link_id", ses.LinkID}} {
		if row.id == nil {
			continue
		}
		if !recompute {
			if _, err := tx.Exec(ctx, `UPDATE `+row.table+` SET last_paid_at = GREATEST(last_paid_at, $2) WHERE id = $1`,
				*row.id, st.PaidAt); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM `+row.table+` WHERE id = $1 FOR NO KEY UPDATE`, *row.id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+row.table+` SET last_paid_at = (SELECT max(paid_at) FROM sessions
			WHERE `+row.col+` = $1 AND status IN `+paidStatuses+`) WHERE id = $1`, *row.id); err != nil {
			return err
		}
	}
	return nil
}

// paidStatus is the Go side of paidStatuses.
func paidStatus(st session.Status) bool { return st == session.Paid || st == session.Overpaid }

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (s *Store) insertEvent(ctx context.Context, tx pgx.Tx, ses *Session, typ string, version int64, now time.Time) (EventRecord, error) {
	meta := EventMeta{ID: ids.NewAt(ids.Event, now), Type: typ, CreatedAt: now}
	payload, err := s.renderer.EventPayload(meta, ses)
	if err != nil {
		return EventRecord{}, fmt.Errorf("store: render event: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO events (id, merchant_id, livemode, type, session_id, session_version, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, meta.ID, ses.MerchantID, ses.Livemode, typ, ses.ID, version, string(payload), now)
	if err != nil {
		return EventRecord{}, err
	}
	return EventRecord{ID: meta.ID, Type: typ, SessionVersion: version, Payload: payload}, nil
}

// expiryHeldExpr is true while a session past its expires_at may still hold an on-time payment
// the scanner has not read yet (ADR-0008 H-3). The warm scan reads a deposit address about once a
// minute and drops it at expires_at, so a transfer mined in the last minute before expiry is only
// seen by the first late check (balance at the head, rescan on a change). The session is held
// while one of its watched deposit entries (revealed networks, D48; on a customer address only
// while it is the address's current session, D50) has not had that first late check
// (next_check_at is written by a late check once it read the address in full, scan.NextCheck, and
// cleared when the row changes owner; store.WatchEntry.AwaitsFirstLateCheck), or while one of its
// Gateway settle jobs is in flight or landed but maybe not credited yet (valid_before <=
// expires_at: it is mined before expiry or never; a landed Solana settle turns mined before the
// reference-key scan records it, an EVM one only after its payment is recorded). s is the session
// row.
const expiryHeldExpr = `(((s.customer_address_id IS NULL OR EXISTS (SELECT 1 FROM customer_addresses ca
			WHERE ca.id = s.customer_address_id AND ca.current_session_id = s.id))
		AND (EXISTS (SELECT 1 FROM session_pay_to p WHERE p.session_id = s.id AND p.family = 'evm'` + revealedCond + `
				AND NOT EXISTS (SELECT 1 FROM chain_watch w WHERE w.chain = p.chain AND w.address = lower(p.address)
					AND w.session_id = s.id AND w.next_check_at IS NOT NULL))
			OR EXISTS (SELECT 1 FROM session_pay_to p WHERE p.session_id = s.id AND p.family = 'solana'
				AND p.program IS NOT NULL` + revealedCond + `
				AND NOT EXISTS (SELECT 1 FROM solana_watch sw WHERE sw.chain = p.chain AND sw.session_id = s.id
					AND sw.kind = 'deposit' AND sw.asset = p.asset AND sw.next_check_at IS NOT NULL))))
	OR EXISTS (SELECT 1 FROM chain_tx_jobs j WHERE j.session_id = s.id AND j.kind = 'settle'
		AND j.status IN ('pending', 'blocked', 'submitted', 'mined')))`

// SessionsDueForExpiry lists open or partially paid sessions whose expiry time has passed, oldest
// first. A session with a detected payment is left out: Expire refuses it until the payment
// resolves, and listing it let such sessions fill every batch and stall all other expiries
// (ADR-0008 A-2). With hold > 0 a session is also left out while expiryHeldExpr is true, for at
// most hold past its expires_at (a chain whose scanner is off must not keep sessions open).
// after, when not empty, lists only the sessions after that one in this order (ExpireDue pages
// past sessions it could not expire).
func (s *Store) SessionsDueForExpiry(ctx context.Context, now time.Time, hold time.Duration, after string, limit int) ([]string, error) {
	ids, _, err := s.ExpiryScan(ctx, now, hold, after, limit)
	return ids, err
}

// openGrace is how long past its expiry an open session still counts for ExpiryScan. SQL
// interval literal.
const openGrace = "1 hour"

// ExpiryScan is SessionsDueForExpiry that also reports, in the same statement, whether any
// session is open or partially paid and not past its expiry by more than openGrace: the worker
// stays active while one is (plan B). A session stuck open long after expiry (a detected payment
// never confirmed, an expiry that keeps failing) does not keep it active forever; the chain state
// covers the pending payment.
func (s *Store) ExpiryScan(ctx context.Context, now time.Time, hold time.Duration, after string, limit int) ([]string, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT d.id, o.open
		FROM (SELECT EXISTS (SELECT 1 FROM sessions WHERE status IN ('open', 'partially_paid')
			AND expires_at > $1::timestamptz - interval '`+openGrace+`') AS open) o
		LEFT JOIN LATERAL (SELECT s.id, s.expires_at FROM sessions s WHERE s.status IN ('open', 'partially_paid') AND s.expires_at <= $1
		AND NOT EXISTS (SELECT 1 FROM payments y WHERE y.session_id = s.id AND y.status = 'detected')
		AND ($3::bigint <= 0 OR s.expires_at <= $1::timestamptz - $3::bigint * interval '1 microsecond'
			OR NOT `+expiryHeldExpr+`)
		AND ($4::text = '' OR (s.expires_at, s.id) > (SELECT a.expires_at, a.id FROM sessions a WHERE a.id = $4))
		ORDER BY s.expires_at, s.id LIMIT $2) d ON true
		ORDER BY d.expires_at, d.id`, now, limit, micros(hold), after)
	if err != nil {
		return nil, false, opErr("sessions due for expiry", err)
	}
	var out []string
	var id *string
	open := false
	_, err = pgx.ForEachRow(rows, []any{&id, &open}, func() error {
		if id != nil { // the one row of a scan with nothing due has no id
			out = append(out, *id)
		}
		return nil
	})
	if err != nil {
		return nil, false, opErr("sessions due for expiry", err)
	}
	return out, open, nil
}

// SessionsByAddress finds the sessions a deposit address belongs to (detector lookup).
func (s *Store) SessionsByAddress(ctx context.Context, chainID, address string) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT session_id FROM session_pay_to WHERE chain = $1 AND address = $2
		ORDER BY session_id`, chainID, address)
	if err != nil {
		return nil, opErr("sessions by address", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return out, opErr("sessions by address", err)
}

// conserved is the store's own invariant of a settlement: one amount per recipient of the
// snapshot, adding up to net (gross - fee).
func conserved(net money.Units, shares []money.Units, recipients int) error {
	if len(shares) != recipients {
		return fmt.Errorf("%d payouts for %d recipients", len(shares), recipients)
	}
	sum := money.Zero
	for _, a := range shares {
		var err error
		if sum, err = sum.Add(a); err != nil {
			return err
		}
	}
	if !sum.Equal(net) {
		return fmt.Errorf("payouts add up to %s, gross - fee is %s", sum, net)
	}
	return nil
}

// ErrSettlementMismatch is returned when a settlement does not match the fee and split rule of its address.
var ErrSettlementMismatch = errors.New("store: settlement does not match the fee and split rule for this address")

// SettlementRule checks a settlement against the session's pay_to entry for its chain and asset
// (its fee rule and payout snapshot); RecordSettlement refuses a settlement it rejects with
// ErrSettlementMismatch. chainsettle.Verify is the rule.
type SettlementRule func(pt PayToRecord, st Settlement) error

// RecordSettlement stores an observed on-chain settlement after rule accepted it against the pay_to
// entry and the payout snapshot the address was created with, read under the session's lock. A
// mismatch is refused: it would mean an unprovenanced number. Whatever the rule, a settlement
// must name one amount per recipient of the snapshot, and the payouts must add up to gross - fee.
func (s *Store) RecordSettlement(ctx context.Context, sessionID string, st Settlement, rule SettlementRule) (*Session, error) {
	if rule == nil {
		return nil, errors.New("store: RecordSettlement needs a settlement rule")
	}
	var out *Session
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := loadSession(ctx, tx, sessionID, true)
		if err != nil {
			return err
		}
		var pt *PayToRecord
		for i := range cur.PayTo {
			if cur.PayTo[i].Chain == st.Chain && cur.PayTo[i].Asset == st.Asset {
				pt = &cur.PayTo[i]
			}
		}
		if pt == nil {
			return fmt.Errorf("%w: no pay_to for %s/%s", ErrSettlementMismatch, st.Chain, st.Asset)
		}
		if err := rule(*pt, st); err != nil {
			return fmt.Errorf("%w: %v", ErrSettlementMismatch, err)
		}
		net, err := st.Gross.Sub(st.Fee)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrSettlementMismatch, err)
		}
		if !st.Net.IsZero() && !st.Net.Equal(net) {
			return fmt.Errorf("%w: observed net %s, gross - fee is %s", ErrSettlementMismatch, st.Net, net)
		}
		if err := conserved(net, st.Shares, len(pt.Recipients)); err != nil {
			return fmt.Errorf("%w: %v", ErrSettlementMismatch, err)
		}
		st.Net = net
		if st.ID == "" {
			st.ID = ids.New(ids.Settlement)
		}
		st.Decimals = pt.Decimals
		if _, err := tx.Exec(ctx, `INSERT INTO settlements (id, session_id, chain, asset, gross, fee, net, tx_hash, log_index, settled_at, simulated)
			VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7::numeric, $8, $9, $10, $11)`,
			st.ID, sessionID, st.Chain, st.Asset, st.Gross.String(), st.Fee.String(), st.Net.String(), st.TxHash,
			int32(st.LogIndex), st.SettledAt, st.Simulated); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		var snapshot []SessionPayout
		for _, p := range cur.Payouts {
			if p.Chain == pt.Chain {
				snapshot = append(snapshot, p)
			}
		}
		st.Payouts = nil
		for i, amount := range st.Shares {
			if amount.IsZero() {
				continue
			}
			r := snapshot[i]
			if _, err := tx.Exec(ctx, `INSERT INTO settlement_payouts (settlement_id, position, wallet_id, address, amount)
				VALUES ($1, $2, $3, $4, $5::numeric)`, st.ID, i, r.WalletID, r.Address, amount.String()); err != nil {
				return err
			}
			st.Payouts = append(st.Payouts, SettlementPayout{Position: i, WalletID: r.WalletID, Label: r.Label, Address: r.Address, Amount: amount})
		}
		now := s.now()
		// A tick or late check (EVM, per token) and the Solana warm trigger rescan an address whose
		// balance rose above the one seen last. A settlement empties the address for its asset, so the
		// next deposit counts as a change even when it equals the balance seen before the flush. A
		// settlement with its block stores that zero only over a balance read before the block: a
		// later reading is the address's balance (a deposit after the flush), and a zero stamped
		// over it would leave that deposit out of the blocks a rescan must explain the next reading
		// with (ADR-0008 H-1: a false balance_unexplained). As in FlushMined, checked_block moves
		// back to the block of the balance the zero replaces, by at most st.Reach blocks.
		// Moving checked_at tells a check that read the row before this point not to write its stale
		// balance back. The rows are the address's, whoever holds them: on a customer address a newer
		// session may own the watch.
		const older = `coalesce((w.token_blocks ->> lower(p.token))::bigint, -1) < $5::bigint`
		// A Direct split never touched the deposit address (Settlement.Direct).
		if !st.Direct {
			if _, err := tx.Exec(ctx, `UPDATE chain_watch w SET checked_at = $3,
					token_balances = CASE WHEN $5::bigint = 0 OR `+older+` THEN coalesce(w.token_balances, '{}'::jsonb) ||
						jsonb_build_object(lower(p.token), '0') ELSE w.token_balances END,
					token_blocks = CASE WHEN $5::bigint > 0 AND `+older+` THEN coalesce(w.token_blocks, '{}'::jsonb) ||
						jsonb_build_object(lower(p.token), $5::bigint) ELSE w.token_blocks END,
					checked_block = CASE WHEN $5::bigint > 0 AND `+older+` AND w.checked_block IS NOT NULL
						AND w.token_blocks ->> lower(p.token) IS NOT NULL
						THEN least(w.checked_block, greatest((w.token_blocks ->> lower(p.token))::bigint, w.checked_block - $6::bigint))
						ELSE w.checked_block END
				FROM session_pay_to p WHERE p.session_id = $2 AND p.chain = $1 AND p.asset = $4 AND w.chain = p.chain
					AND w.address = lower(p.address)`, st.Chain, sessionID, now, st.Asset, int64(st.Block), int64(st.Reach)); err != nil {
				return err
			}
		}
		if ata := pt.TokenAccount(); ata != "" {
			if _, err := tx.Exec(ctx, `UPDATE solana_watch SET last_balance = 0, checked_at = $3 WHERE chain = $1 AND address = $2
				AND kind = 'deposit'`, st.Chain, ata, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET version = version + 1, updated_at = $2 WHERE id = $1`, sessionID, now); err != nil {
			return err
		}
		cur.State.Version++
		cur.UpdatedAt = now
		cur.Settlements = append(cur.Settlements, st)
		out = cur
		return nil
	})
	return out, opErr("record settlement", err)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
