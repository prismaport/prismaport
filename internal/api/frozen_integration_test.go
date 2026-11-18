//go:build integration

package api_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solfake"
)

// frozenATAs is a Solana inspector whose wallets have every token account, except that a test
// can freeze a wallet's USDT account or make every lookup fail.
type frozenATAs struct {
	mu     sync.Mutex
	frozen map[string]bool
	down   bool
}

func (*frozenATAs) Inspect(context.Context, bool, string) (destination.SolanaAccountKind, error) {
	return destination.SolanaWallet, nil
}

func (f *frozenATAs) MissingTokenAccounts(_ context.Context, _ bool, w string) ([]destination.MissingTokenAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("solana provider down")
	}
	if !f.frozen[w] {
		return nil, nil
	}
	return []destination.MissingTokenAccount{{Chain: "solana-devnet", Asset: "USDT", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Account: "x", Frozen: true}}, nil
}

func (f *frozenATAs) set(w string, frozen, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen == nil {
		f.frozen = map[string]bool{}
	}
	f.frozen[w], f.down = frozen, down
}

// ADR 0008 I-2: a Solana wallet whose token account the issuer froze is refused when it is added
// and when it is bound into a product's payout (one frozen recipient blocks the whole split's
// flushes). The binding re-check is advisory: when it cannot run the payout is saved.
func TestFrozenSolanaWalletIsRefused(t *testing.T) {
	t.Parallel()
	insp := &frozenATAs{}
	app := apptest.New(t, apptest.Options{SolanaInspector: insp})
	key := app.TestKey
	frozen := solfake.Key("frozen wallet").String()
	insp.set(frozen, true, false)
	for _, allow := range []bool{false, true} {
		_, err := app.Svc.SetDestination(t.Context(), app.MerchantID, false, chain.Solana, frozen, allow, "test")
		e := service.AsError(err)
		if !errors.Is(err, destination.ErrTokenAccountFrozen) || e == nil || e.Kind != service.KindInvalid || e.Code != "wallet_blacklisted" ||
			!strings.Contains(e.Message, "froze the USDT on solana-devnet token account") {
			t.Fatalf("adding a frozen wallet (allowing unverified: %v): %v", allow, err)
		}
	}

	partner := solfake.Key("partner wallet").String()
	id := app.Wallet(key, chain.Solana, "Partner", partner).ID
	insp.set(partner, true, false) // frozen after it was added
	body := map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00", "payout": map[string]any{"solana-devnet": items(id, "100")}}
	status, code, msg := errorOf(t, app.Do("POST", "/v1/products", key, body))
	if status != 400 || code != "wallet_blacklisted" || !strings.Contains(msg, "Partner") {
		t.Fatalf("binding a frozen wallet: %d %s %s", status, code, msg)
	}

	insp.set(partner, true, true)
	if r := app.Do("POST", "/v1/products", key, body); r.Status != 200 {
		t.Fatalf("an unavailable frozen check must not block the payout: %d %s", r.Status, r.Body)
	}
}
