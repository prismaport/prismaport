//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

const solPayChain = "solana-devnet"

type solPayEnv struct {
	t        *testing.T
	app      *apptest.App
	cluster  *solfake.Cluster
	feePayer *solana.Keypair
	mint     solana.PublicKey
	ctl      *providerCtl
}

// providerCtl breaks the in-memory providers the ways a real node can: getSignatureStatuses
// failing, or a blockhash claiming a last valid height far ahead.
type providerCtl struct {
	statusesDown atomic.Bool
	extraBlocks  atomic.Uint64
}

type ctlProvider struct {
	*solfake.Provider
	ctl *providerCtl
}

func (p ctlProvider) SignatureStatuses(ctx context.Context, sigs []string) ([]*solrpc.SignatureStatus, error) {
	if p.ctl.statusesDown.Load() {
		return nil, solrpc.ErrTransport
	}
	return p.Provider.SignatureStatuses(ctx, sigs)
}

func (p ctlProvider) LatestBlockhash(ctx context.Context, cm solrpc.Commitment) (solrpc.Blockhash, error) {
	bh, err := p.Provider.LatestBlockhash(ctx, cm)
	bh.LastValidBlockHeight += p.ctl.extraBlocks.Load()
	return bh, err
}

func solKey(t *testing.T, b byte) *solana.Keypair {
	t.Helper()
	k, err := solana.KeypairFromSeed(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newSolPay wires the connected-wallet path on solana-devnet over an in-memory cluster in which
// the merchant's USDC token account exists.
func newSolPay(t *testing.T, public *api.PublicOptions) *solPayEnv {
	t.Helper()
	e := &solPayEnv{t: t, cluster: solfake.New(), feePayer: solKey(t, 41), ctl: &providerCtl{}}
	e.cluster.MineN(5)
	c, ok := chain.Pilot().Get(solPayChain)
	if !ok {
		t.Fatal("no solana-devnet in the pilot registry")
	}
	a, _ := c.Asset("USDC")
	e.mint = solana.MustPublicKey(a.Token)
	q := solwatch.NewQuorum(solPayChain, []solwatch.Provider{ctlProvider{e.cluster.Provider("a"), e.ctl},
		ctlProvider{e.cluster.Provider("b"), e.ctl}}, chainwatch.Options{})
	e.app = apptest.New(t, apptest.Options{
		Solana: map[string]service.SolanaChain{solPayChain: {Program: solKey(t, 42).PublicKey().String(), RentReceiver: e.feePayer.PublicKey().String()}},
		Checkout: func(svc *service.Service, st *store.Store) *checkout.Checkout {
			return checkout.New(svc, st, nil).WithSolana(map[string]checkout.SolanaChain{solPayChain: {Chain: c, Quorum: q, FeePayer: e.feePayer, PriorityFee: 1_000}})
		},
		Public: public,
	})
	dest := solana.MustPublicKey(apptest.SolanaDestination)
	e.cluster.SetTokenAccount(e.ata(dest), e.mint, dest, 0)
	return e
}

func (e *solPayEnv) ata(owner solana.PublicKey) solana.PublicKey {
	e.t.Helper()
	a, err := solana.AssociatedTokenAddress(owner, e.mint)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *solPayEnv) session(amount string) string {
	e.t.Helper()
	r := e.app.Session(e.app.TestKey, map[string]any{"amount": amount}, idem("solpay-"+amount+"-"+e.t.Name())...)
	if r.Status != http.StatusOK {
		e.t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	return r.JSON(e.t)["id"].(string)
}

func payPath(id, chainID, asset string) string {
	return "/v1/checkout/sessions/" + id + "/solana/pay?chain=" + chainID + "&asset=" + asset
}

type solPayTx struct {
	Transaction string `json:"transaction"`
	Message     string `json:"message"`
}

// transfers sums the token TransferChecked amounts of tx and reports whether any carries ref.
func transfers(tx *solana.Transaction, ref solana.PublicKey) (total uint64, hasRef bool) {
	m := tx.Message
	for _, ix := range m.Instructions {
		if m.AccountKeys[ix.ProgramIndex] != solana.TokenProgramID || len(ix.Data) != 10 || ix.Data[0] != 12 {
			continue
		}
		total += binary.LittleEndian.Uint64(ix.Data[1:9])
		for _, a := range ix.Accounts[4:] {
			hasRef = hasRef || m.AccountKeys[a] == ref
		}
	}
	return total, hasRef
}

type publicPayTo struct {
	Chain     string `json:"chain"`
	Asset     string `json:"asset"`
	Reference string `json:"reference"`
}

func (e *solPayEnv) payTo(id string) []publicPayTo {
	e.t.Helper()
	r := e.app.Do("GET", "/v1/checkout/sessions/"+id, "", nil)
	var v struct {
		PayTo []publicPayTo `json:"pay_to"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil || r.Status != http.StatusOK {
		e.t.Fatalf("session view: %d %s", r.Status, r.Body)
	}
	return v.PayTo
}

func TestSolanaPayLabelAndIcon(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	r := e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil, "Origin", "https://phantom.app")
	if r.Status != http.StatusOK {
		t.Fatalf("GET: %d %s", r.Status, r.Body)
	}
	var info checkout.SolanaPayInfo
	_ = json.Unmarshal(r.Body, &info)
	if info.Label != "Acme Store via Prismaport" || info.Icon != apptest.CheckoutBase+"/pay/favicon.svg" {
		t.Fatalf("info %+v", info)
	}
	if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content type %q", got)
	}
	if r.Header.Get("Access-Control-Allow-Origin") != "*" || r.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", r.Header)
	}
	// The GET builds nothing: asking again changes nothing.
	for range 3 {
		if r := e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil); r.Status != http.StatusOK {
			t.Fatalf("GET again: %d", r.Status)
		}
	}
}

func TestSolanaPayBuildsTheWalletTransaction(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	payer := solKey(t, 43).PublicKey()
	// Fields of later spec versions are ignored, as the spec asks.
	r := e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", `{"account":"`+payer.String()+`","future_field":1}`)
	if r.Status != http.StatusOK {
		t.Fatalf("POST: %d %s", r.Status, r.Body)
	}
	if r.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("no CORS on the POST: %v", r.Header)
	}
	var out solPayTx
	_ = json.Unmarshal(r.Body, &out)
	if out.Message != "Pay 25.00 USDC to Acme Store" {
		t.Errorf("message %q", out.Message)
	}
	tx, err := solana.ParseTransactionBase64(out.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	m := tx.Message
	if m.Header.RequiredSignatures != 2 || m.AccountKeys[0] != e.feePayer.PublicKey() || m.AccountKeys[1] != payer {
		t.Fatalf("signers %d %v", m.Header.RequiredSignatures, m.AccountKeys[:2])
	}
	if !tx.VerifySignature(e.feePayer.PublicKey()) || tx.VerifySignature(payer) {
		t.Fatal("the gateway must sign as fee payer and leave the payer's signature to the wallet")
	}
	var pt publicPayTo
	for _, p := range e.payTo(id) {
		if p.Chain == solPayChain && p.Asset == "USDC" {
			pt = p
		}
	}
	ref, err := solana.ParsePublicKey(pt.Reference)
	if err != nil {
		t.Fatalf("pay_to reference %q: %v", pt.Reference, err)
	}
	total, hasRef := transfers(tx, ref)
	if total != 25_000_000 || !hasRef {
		t.Fatalf("transfers %d, reference %v", total, hasRef)
	}
	// The same builder as solana/transaction: the same payer asking there gets the transaction in
	// flight back, with the same reference, fee payer and amount.
	w := e.app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "", map[string]string{"chain": solPayChain, "asset": "USDC", "account": payer.String()})
	var wt checkout.WalletTxResponse
	_ = json.Unmarshal(w.Body, &wt)
	if w.Status != http.StatusOK || wt.Reference != pt.Reference || wt.FeePayer != e.feePayer.PublicKey().String() || wt.Amount != "25.000000" ||
		wt.Transaction != out.Transaction {
		t.Fatalf("solana/transaction: %d %+v", w.Status, wt)
	}
}

// The amount is what is still due when the wallet posts, after a partial payment.
func TestSolanaPayChargesWhatIsStillDue(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	simulate(t, e.app, id, map[string]any{"action": "pay_partial", "amount": "10.00", "chain": solPayChain, "asset": "USDC"})
	payer := solKey(t, 43).PublicKey()
	r := e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": payer.String()})
	if r.Status != http.StatusOK {
		t.Fatalf("POST: %d %s", r.Status, r.Body)
	}
	var out solPayTx
	_ = json.Unmarshal(r.Body, &out)
	tx, err := solana.ParseTransactionBase64(out.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	if total, _ := transfers(tx, solana.PublicKey{}); total != 15_000_000 || out.Message != "Pay 15.00 USDC to Acme Store" {
		t.Fatalf("after 10.00 of 25.00: transfers %d, message %q", total, out.Message)
	}
}

func TestSolanaPayPreflight(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	req, _ := http.NewRequest("OPTIONS", e.app.Server.URL+payPath("ses_01M5MYPW80FMXFE81YHNB8MPR8", solPayChain, "USDC"), nil)
	req.Header.Set("Origin", "https://wallet.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := resp.Header
	if resp.StatusCode != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") || !strings.Contains(h.Get("Access-Control-Allow-Methods"), "GET") ||
		!strings.Contains(strings.ToLower(h.Get("Access-Control-Allow-Headers")), "content-type") || h.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight %d %v", resp.StatusCode, h)
	}
}

// Every refusal is readable by itself: wallets show the top-level message.
func TestSolanaPayErrors(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	payer := solKey(t, 43).PublicKey().String()
	dust := e.session("25.01")
	simulate(t, e.app, dust, map[string]any{"action": "pay_partial", "amount": "24.96", "chain": solPayChain, "asset": "USDC"})

	for _, c := range []struct {
		name, method, path string
		body               any
		status             int
		code               string
	}{
		{"unknown session GET", "GET", payPath("ses_01M5MYPW80FMXFE81YHNB8MPR8", solPayChain, "USDC"), nil, 404, "session_not_found"},
		{"unknown session POST", "POST", payPath("ses_01M5MYPW80FMXFE81YHNB8MPR8", solPayChain, "USDC"), map[string]string{"account": payer}, 404, "session_not_found"},
		{"no chain", "GET", "/v1/checkout/sessions/" + id + "/solana/pay?asset=USDC", nil, 400, "invalid_request"},
		{"no asset", "POST", "/v1/checkout/sessions/" + id + "/solana/pay?chain=" + solPayChain, map[string]string{"account": payer}, 400, "invalid_request"},
		{"unknown chain", "GET", payPath(id, "solana-mainnet-beta", "USDC"), nil, 400, "invalid_request"},
		{"EVM chain", "POST", payPath(id, "base-sepolia", "USDC"), map[string]string{"account": payer}, 400, "invalid_request"},
		{"unknown asset", "POST", payPath(id, solPayChain, "USDT"), map[string]string{"account": payer}, 400, "invalid_request"},
		{"invalid account", "POST", payPath(id, solPayChain, "USDC"), map[string]string{"account": "not-a-key"}, 400, "invalid_request"},
		{"fee payer as account", "POST", payPath(id, solPayChain, "USDC"), map[string]string{"account": e.feePayer.PublicKey().String()}, 400, "invalid_request"},
		{"no account", "POST", payPath(id, solPayChain, "USDC"), map[string]string{}, 400, "invalid_request"},
		{"account not a string", "POST", payPath(id, solPayChain, "USDC"), map[string]any{"account": 7}, 400, "invalid_request"},
		{"body not an object", "POST", payPath(id, solPayChain, "USDC"), "[]", 400, "invalid_request"},
		{"below the minimum", "POST", payPath(dust, solPayChain, "USDC"), map[string]string{"account": payer}, 409, "amount_below_minimum"},
		{"below the minimum GET", "GET", payPath(dust, solPayChain, "USDC"), nil, 409, "amount_below_minimum"},
	} {
		r := e.app.Do(c.method, c.path, "", c.body)
		checkSolPayError(t, c.name, r, c.status, c.code)
	}

	// A recipient token account that does not exist: the gateway never pays its rent.
	e.cluster.CloseAccount(e.ata(solana.MustPublicKey(apptest.SolanaDestination)))
	r := e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": payer})
	checkSolPayError(t, "recipient token account missing", r, 409, "destination_not_ready")
	checkSolPayError(t, "recipient token account missing GET", e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil), 409, "destination_not_ready")
	e.cluster.SetTokenAccount(e.ata(solana.MustPublicKey(apptest.SolanaDestination)), e.mint, solana.MustPublicKey(apptest.SolanaDestination), 0)

	// Expiring within the window a transaction stays valid: one built now could land late.
	e.app.Clock.Advance(30*time.Minute - checkout.WalletWindow + time.Second)
	for _, r := range []apptest.Response{
		e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil),
		e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": payer}),
		e.app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "", map[string]string{"chain": solPayChain, "asset": "USDC", "account": payer}),
	} {
		checkSolPayErrorCode(t, "expiring soon", r, 409, "session_not_payable")
		if !strings.Contains(string(r.Body), "expires in less than two minutes") {
			t.Errorf("expiring soon: %s", r.Body)
		}
	}

	// Expired: the request is gone (D45), GET and POST; the connected-wallet build keeps 409.
	e.app.Clock.Advance(25 * time.Hour)
	checkSolPayError(t, "expired GET", e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil), 410, "session_expired")
	checkSolPayError(t, "expired POST", e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": payer}), 410, "session_expired")
	checkSolPayErrorCode(t, "expired transaction", e.app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "",
		map[string]string{"chain": solPayChain, "asset": "USDC", "account": payer}), 409, "session_not_payable")
}

// checkSolPayErrorCode checks a refusal of either route by its code.
func checkSolPayErrorCode(t *testing.T, name string, r apptest.Response, status int, code string) {
	t.Helper()
	if r.Status != status || r.ErrorCode(t) != code {
		t.Errorf("%s: %d %s, want %d %s", name, r.Status, r.Body, status, code)
	}
}

func checkSolPayError(t *testing.T, name string, r apptest.Response, status int, code string) {
	t.Helper()
	var body api.SolanaPayError
	if err := json.Unmarshal(r.Body, &body); err != nil || r.Status != status || body.Error.Code != code {
		t.Errorf("%s: %d %s, want %d %s", name, r.Status, r.Body, status, code)
		return
	}
	first := []rune(body.Message)
	if len(first) == 0 || !unicode.IsUpper(first[0]) || !strings.HasSuffix(body.Message, ".") || body.Error.Message == "" {
		t.Errorf("%s: message %q is not a sentence a wallet can show", name, body.Message)
	}
	if r.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%s: an error without CORS cannot be read by a browser wallet", name)
	}
}

// A session whose cluster has no fee payer configured is a 503, not a malformed request.
func TestSolanaPayUnavailableCluster(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{
		Solana:   map[string]service.SolanaChain{solPayChain: {Program: solKey(t, 42).PublicKey().String(), RentReceiver: solKey(t, 41).PublicKey().String()}},
		Checkout: func(svc *service.Service, st *store.Store) *checkout.Checkout { return checkout.New(svc, st, nil) },
	})
	r := app.Session(app.TestKey, map[string]any{"amount": "25.00"}, idem("unavailable")...)
	id := r.JSON(t)["id"].(string)
	checkSolPayError(t, "no fee payer", app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil), 503, "service_unavailable")
}

// Both Solana Pay routes take tokens of the solana_transaction bucket, so scanning does not add
// to what one IP can build; the 429 keeps the wallet shape.
func TestSolanaPaySharesTheTransactionBucket(t *testing.T) {
	t.Parallel()
	if _, ok := api.DefaultLimits()["solana_pay"]; ok {
		t.Fatal("a solana_pay bucket of its own doubles what one IP can build")
	}
	limits := map[string]api.Limit{}
	for route := range api.DefaultLimits() {
		limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits[api.RouteSolanaTransaction] = api.Limit{PerMinute: 1, Burst: 3}
	e := newSolPay(t, &api.PublicOptions{Limits: limits})
	id := e.session("25.00")
	account := solKey(t, 43).PublicKey().String()
	if r := e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil); r.Status != http.StatusOK {
		t.Fatalf("GET: %d %s", r.Status, r.Body)
	}
	if r := e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": account}); r.Status != http.StatusOK {
		t.Fatalf("POST: %d %s", r.Status, r.Body)
	}
	if r := e.app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "", map[string]string{"chain": solPayChain, "asset": "USDC",
		"account": account}); r.Status != http.StatusOK {
		t.Fatalf("solana/transaction: %d %s", r.Status, r.Body)
	}
	r := e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": account})
	checkSolPayError(t, "over the shared limit", r, 429, "rate_limited")
	if r.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if w := e.app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "", map[string]string{"chain": solPayChain, "asset": "USDC",
		"account": account}); w.Status != http.StatusTooManyRequests {
		t.Fatalf("solana/transaction after the Solana Pay requests: %d", w.Status)
	}
}

func (e *solPayEnv) build(route, id string, payer solana.PublicKey) apptest.Response {
	e.t.Helper()
	if route == "pay" {
		return e.app.Do("POST", payPath(id, solPayChain, "USDC"), "", map[string]string{"account": payer.String()})
	}
	return e.app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "", map[string]string{"chain": solPayChain, "asset": "USDC",
		"account": payer.String()})
}

func builtTx(t *testing.T, r apptest.Response) string {
	t.Helper()
	if r.Status != http.StatusOK {
		t.Fatalf("build: %d %s", r.Status, r.Body)
	}
	var out struct {
		Transaction string `json:"transaction"`
	}
	_ = json.Unmarshal(r.Body, &out)
	return out.Transaction
}

// A session has at most one presigned transaction that can still land, whichever route built it:
// a second wallet (another device, or the page and a scan) is refused until the first expires,
// so the session is never paid twice and transactions cannot be minted in bulk at the fee
// payer's cost. The wallet it was built for gets it back.
func TestWalletTransactionOneInFlight(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"pay", "transaction"} {
		t.Run(first, func(t *testing.T) {
			e := newSolPay(t, nil)
			id := e.session("25.00")
			a, b := solKey(t, 43).PublicKey(), solKey(t, 44).PublicKey()
			txA := builtTx(t, e.build(first, id, a))
			for _, route := range []string{"pay", "transaction"} {
				r := e.build(route, id, b)
				checkSolPayErrorCode(t, "second wallet on "+route, r, 409, "payment_in_progress")
				if !strings.Contains(string(r.Body), "already in progress") {
					t.Errorf("second wallet on %s: %s", route, r.Body)
				}
				if again := builtTx(t, e.build(route, id, a)); again != txA {
					t.Errorf("the same wallet on %s got another transaction", route)
				}
			}
			checkSolPayError(t, "GET while in flight", e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil), 409, "payment_in_progress")

			// The window has passed but the chain has not passed its last valid height: still in flight.
			e.app.Clock.Advance(checkout.WalletWindow)
			checkSolPayError(t, "past the window, still valid", e.build("pay", id, b), 409, "payment_in_progress")
			// Past both: the next wallet gets a transaction of its own.
			e.cluster.MineN(151)
			if r := e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil); r.Status != http.StatusOK {
				t.Fatalf("GET after expiry: %d %s", r.Status, r.Body)
			}
			txB := builtTx(t, e.build("transaction", id, b))
			tx, err := solana.ParseTransactionBase64(txB)
			if err != nil || txB == txA || tx.Message.AccountKeys[1] != b {
				t.Fatalf("after expiry: %v, same %v", err, txB == txA)
			}
		})
	}
}

// Past its last valid height but young, a transaction still counts as in flight; and the payer it
// was built for is refused too once what is due changed, since it would pay the old amount.
func TestWalletTransactionInFlightRules(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	a, b := solKey(t, 43).PublicKey(), solKey(t, 44).PublicKey()
	builtTx(t, e.build("pay", id, a))
	e.cluster.MineN(151)
	checkSolPayErrorCode(t, "past its height, inside the window", e.build("transaction", id, b), 409, "payment_in_progress")
	// The wallet it was built for does not get a transaction that can no longer land.
	checkSolPayError(t, "same wallet, transaction dead", e.build("pay", id, a), 409, "payment_in_progress")
	simulate(t, e.app, id, map[string]any{"action": "pay_partial", "amount": "10.00", "chain": solPayChain, "asset": "USDC"})
	checkSolPayErrorCode(t, "same wallet, amount changed", e.build("transaction", id, a), 409, "payment_in_progress")
}

// Builds racing on one session (any API instance) record exactly one transaction.
func TestWalletTransactionConcurrentBuilds(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	var mu sync.Mutex
	var wg sync.WaitGroup
	statuses := map[int]int{}
	for i := range 6 {
		payer := solKey(t, byte(50+i)).PublicKey()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.build([]string{"pay", "transaction"}[i%2], id, payer)
			mu.Lock()
			statuses[r.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 5 {
		t.Fatalf("statuses %v, want one 200 and five 409", statuses)
	}
}

// Solana entries of the payer view carry the session reference; EVM entries do not.
func TestPayToReference(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	e.app.Reveal(e.app.TestKey, id)
	var sol, evm int
	for _, p := range e.payTo(id) {
		switch {
		case strings.HasPrefix(p.Chain, "solana"):
			sol++
			if k, err := solana.ParsePublicKey(p.Reference); err != nil || k.IsZero() {
				t.Errorf("%s %s: reference %q", p.Chain, p.Asset, p.Reference)
			}
		default:
			evm++
			if p.Reference != "" {
				t.Errorf("%s %s: EVM entry with a reference %q", p.Chain, p.Asset, p.Reference)
			}
		}
	}
	if sol == 0 || evm == 0 {
		t.Fatalf("want both families in the session, got %d Solana and %d EVM entries", sol, evm)
	}
	other := e.session("26.00")
	e.app.Reveal(e.app.TestKey, other, solPayChain)
	if a, b := solanaReference(e.payTo(id)), solanaReference(e.payTo(other)); a == b {
		t.Errorf("two sessions share the reference %s", a)
	}
}

func solanaReference(entries []publicPayTo) string {
	for _, p := range entries {
		if p.Chain == solPayChain {
			return p.Reference
		}
	}
	return ""
}

func (e *solPayEnv) expireInFlight() {
	e.t.Helper()
	e.app.Clock.Advance(checkout.WalletWindow)
	e.cluster.MineN(151)
}

// An expired transaction that landed while the detector has not credited it yet keeps the
// session locked: another wallet paying now would pay the session twice. One that failed on
// chain releases it.
func TestWalletTransactionLandedIsNotReleased(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	a, b := solKey(t, 43).PublicKey(), solKey(t, 44).PublicKey()
	tx, err := solana.ParseTransactionBase64(builtTx(t, e.build("transaction", id, a)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tx.Serialize()
	onChain, err := solfake.FromRaw(raw, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.cluster.Mine(onChain)
	e.expireInFlight()
	checkSolPayError(t, "landed, not credited, GET", e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil), 409, "payment_in_progress")
	checkSolPayError(t, "landed, not credited", e.build("pay", id, b), 409, "payment_in_progress")
	checkSolPayErrorCode(t, "landed, same wallet", e.build("transaction", id, a), 409, "payment_in_progress")

	other := e.session("26.00")
	tx, err = solana.ParseTransactionBase64(builtTx(t, e.build("pay", other, a)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = tx.Serialize()
	failed, err := solfake.FromRaw(raw, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed.Err = `{"InstructionError":[1,{"Custom":1}]}`
	e.cluster.Mine(failed)
	e.expireInFlight()
	builtTx(t, e.build("pay", other, b))
}

// Unable to learn whether the expired transaction landed, the session stays locked.
func TestWalletTransactionUnknownStatusFailsClosed(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	builtTx(t, e.build("pay", id, solKey(t, 43).PublicKey()))
	e.expireInFlight()
	e.ctl.statusesDown.Store(true)
	checkSolPayError(t, "statuses unavailable", e.build("pay", id, solKey(t, 44).PublicKey()), 409, "payment_in_progress")
}

// A provider claiming a blockhash valid far beyond the usual ~150 blocks would lock the session
// until it expires; no transaction is issued with it.
func TestWalletTransactionBogusLastValidHeight(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	e.ctl.extraBlocks.Store(checkout.MaxValidBlocks)
	checkSolPayError(t, "bogus last valid height", e.build("pay", id, solKey(t, 43).PublicKey()), 503, "service_unavailable")
	e.ctl.extraBlocks.Store(0)
	builtTx(t, e.build("pay", id, solKey(t, 44).PublicKey()))
}
