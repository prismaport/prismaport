//go:build integration

package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solanaderive"
	"github.com/prismaport/prismaport/internal/store"
)

const testProgram = "3DHLtkMYKFmxLbQRHkHcNPibfaKYJNntQEnJvUvcnPya"

func solanaApp(t *testing.T) *apptest.App {
	t.Helper()
	return apptest.New(t, apptest.Options{
		Deriver: derive.PerChain{Chains: map[string]derive.AddressDeriver{"solana-devnet": derive.SolanaForwarders{Program: solanaderive.MustParse(testProgram)}},
			Fallback: derive.ByFamily{chain.EVM: derive.Stub{}}},
		Solana: map[string]service.SolanaChain{"solana-devnet": {Program: testProgram, RentReceiver: apptest.SolanaTreasury}},
	})
}

func TestSolanaPayToCommitsProgramAndRentReceiver(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	r := app.Session(app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "a")
	if r.Status != 200 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body, &v)
	ses, err := app.Store.LoadSession(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sol *store.PayToRecord
	for i := range ses.PayTo {
		switch ses.PayTo[i].Chain {
		case "solana-devnet":
			sol = &ses.PayTo[i]
		default:
			if ses.PayTo[i].Program != "" || ses.PayTo[i].RentReceiver != "" {
				t.Fatalf("EVM entry carries Solana params: %+v", ses.PayTo[i])
			}
		}
	}
	if sol == nil || sol.Program != testProgram || sol.RentReceiver != apptest.SolanaTreasury {
		t.Fatalf("solana pay_to %+v", sol)
	}
	var mints []string
	for _, pt := range ses.PayTo {
		if pt.Chain == sol.Chain {
			mints = append(mints, pt.Token)
		}
	}
	mints, err = derive.SortTokens(chain.Solana, mints)
	if err != nil {
		t.Fatal(err)
	}
	want, err := derive.SolanaForwarders{Program: solanaderive.MustParse(testProgram)}.Address(chain.Chain{Family: chain.Solana}, ses.Salt,
		derive.Params{Recipients: sol.Recipients, Treasury: sol.Treasury, RentReceiver: sol.RentReceiver, Fee: sol.Fee, Tokens: mints})
	if err != nil || want != sol.Address {
		t.Fatalf("address %s does not re-derive (%s, %v)", sol.Address, want, err)
	}

	// Nothing is watched until the network is revealed (D48).
	if entries, err := app.Store.SolanaWatchSet(ctx, "solana-devnet", app.Clock.Now(), store.SolanaWalletWindow); err != nil || len(entries) != 0 {
		t.Fatalf("watch set before the reveal %+v %v", entries, err)
	}
	app.Reveal(app.TestKey, v.ID, "solana-devnet")
	entries, err := app.Store.SolanaWatchSet(ctx, "solana-devnet", app.Clock.Now(), store.SolanaWalletWindow)
	if err != nil || len(entries) != 1 || entries[0].Program != testProgram || entries[0].Salt != ses.Salt {
		t.Fatalf("watch set %+v %v", entries, err)
	}
	const ata, ref = "7UeZhudGACwJb3TSY8hdo4bJTpHiCTFv2wsmLGVP9kQz", "CQvVLXhVD546pp9cwbdt5E1Ea5AVFfg1ujsQCnrkuLwm"
	rows := []store.SolanaWatch{{Chain: "solana-devnet", Address: ata, Kind: store.SolanaDeposit, SessionID: v.ID, Asset: "USDC"},
		{Chain: "solana-devnet", Address: ref, Kind: store.SolanaReference, SessionID: v.ID}}
	got, err := app.Store.EnsureSolanaWatch(ctx, rows)
	if err != nil || len(got) != 2 || got[ata].CursorSignature != "" || got[ref].Asset != "" {
		t.Fatalf("ensure %+v %v", got, err)
	}
	if err := app.Store.SetSolanaCursor(ctx, "solana-devnet", ata, "sig1", 77); err != nil {
		t.Fatal(err)
	}
	if got, _ = app.Store.EnsureSolanaWatch(ctx, rows); got[ata].CursorSignature != "sig1" || got[ata].CursorSlot != 77 {
		t.Fatalf("cursor not kept: %+v", got[ata])
	}
	if err := app.Store.ResetSolanaCursors(ctx, "solana-devnet", v.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = app.Store.EnsureSolanaWatch(ctx, rows); got[ata].CursorSignature != "" {
		t.Fatal("cursor not reset")
	}
	if _, err := app.Store.EnsureSolanaWatch(ctx, []store.SolanaWatch{{Chain: "solana-devnet", Address: "0xnotbase58", Kind: store.SolanaDeposit,
		SessionID: v.ID, Asset: "USDC"}}); err == nil {
		t.Fatal("malformed address accepted")
	}

	// Expired an hour ago: the deposit entry is due its first late check (20 min after expiry).
	app.Clock.Advance(2 * time.Hour)
	q := store.ScanQuery{Chain: "solana-devnet", Family: chain.Solana, Now: app.Clock.Now(), LateFirst: 20 * time.Minute, LateWatch: 23 * time.Hour}
	cands, err := app.Store.LateEntries(ctx, q, 10)
	if err != nil || len(cands) != 1 || cands[0].Asset != "USDC" {
		t.Fatalf("late entries %+v %v", cands, err)
	}
	next := app.Clock.Now().Add(time.Hour)
	bal := money.FromUint64(5)
	if err := app.Store.MarkSolanaChecked(ctx, "solana-devnet", ata, store.SolanaCheck{SessionID: v.ID, Balance: &bal, Next: &next}); err != nil {
		t.Fatal(err)
	}
	if cands, _ = app.Store.LateEntries(ctx, q, 10); len(cands) != 0 {
		t.Fatal("checked account listed again before its next check")
	}
	q.Now = next
	if cands, _ = app.Store.LateEntries(ctx, q, 10); len(cands) != 1 {
		t.Fatal("account not due at its next check")
	}
}
