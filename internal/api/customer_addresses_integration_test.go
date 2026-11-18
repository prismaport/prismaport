//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/store"
)

func newAddress(t *testing.T, app *apptest.App, key string, body map[string]any, want int) map[string]any {
	t.Helper()
	r := app.Do("POST", "/v1/customer_addresses", key, body)
	if r.Status != want {
		t.Fatalf("customer address %v: %d %s", body, r.Status, r.Body)
	}
	return r.JSON(t)
}

func openOn(t *testing.T, app *apptest.App, key, addr string, body map[string]any, idemKey string) apptest.Response {
	t.Helper()
	m := map[string]any{"customer_address_id": addr}
	for k, v := range body {
		m[k] = v
	}
	return app.Do("POST", "/v1/sessions", key, m, idem(idemKey)...)
}

func mustOpenOn(t *testing.T, app *apptest.App, key, addr string, body map[string]any, idemKey string) map[string]any {
	t.Helper()
	r := openOn(t, app, key, addr, body, idemKey)
	if r.Status != http.StatusOK {
		t.Fatalf("session on %s: %d %s", addr, r.Status, r.Body)
	}
	return r.JSON(t)
}

// addressesOf maps "chain/asset" to the deposit address of a pay_to list.
func addressesOf(t *testing.T, obj map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, x := range obj["pay_to"].([]any) {
		p := x.(map[string]any)
		out[p["chain"].(string)+"/"+p["asset"].(string)] = p["address"].(string)
	}
	if len(out) == 0 {
		t.Fatalf("no pay_to in %v", obj)
	}
	return out
}

func payoutWallets(t *testing.T, obj map[string]any, chainID string) []string {
	t.Helper()
	var out []string
	for _, x := range obj["payout"].(map[string]any)[chainID].([]any) {
		out = append(out, x.(map[string]any)["wallet_id"].(string))
	}
	return out
}

// rederive derives every pay_to address of a stored session again from the inputs stored with
// it (salt, treasury, fee, recipients, bound tokens), with the deriver the app runs.
func rederive(t *testing.T, app *apptest.App, ses *store.Session) {
	t.Helper()
	for _, p := range ses.PayTo {
		c, ok := app.Registry.Get(p.Chain)
		if !ok {
			t.Fatalf("unknown chain %s", p.Chain)
		}
		got, err := apptest.AnyMode{}.Address(c, ses.Salt, derive.Params{Treasury: p.Treasury, Fee: p.Fee, Recipients: p.Recipients,
			RentReceiver: p.RentReceiver, Tokens: p.BoundTokens})
		if err != nil || got != p.Address {
			t.Fatalf("%s/%s of %s re-derives to %s (%v), stored %s", p.Chain, p.Asset, ses.ID, got, err, p.Address)
		}
	}
}

func TestCustomerAddressGetOrCreate(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	recharge := app.Link(app.TestKey)
	founder := createProduct(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})

	a := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_8412", "product_id": recharge, "metadata": map[string]any{"tier": "gold"}}, http.StatusCreated)
	id := a["id"].(string)
	if !strings.HasPrefix(id, "caddr_") || a["status"] != "active" || a["replaced_by"] != nil || a["rotated_at"] != nil ||
		a["customer_id"] != "user_8412" || a["product_id"] != recharge || a["url"] != apptest.CheckoutBase+"/a/"+id ||
		a["metadata"].(map[string]any)["tier"] != "gold" {
		t.Fatalf("created %v", a)
	}
	addressesOf(t, a)
	// The same customer answers the same address, whatever the metadata.
	again := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_8412", "product_id": recharge}, http.StatusOK)
	if again["id"] != id || fmt.Sprint(addressesOf(t, again)) != fmt.Sprint(addressesOf(t, a)) {
		t.Fatalf("second create %v, first %v", again, a)
	}
	if r := app.Do("POST", "/v1/customer_addresses", app.TestKey, map[string]any{"customer_id": "user_8412", "product_id": founder}); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "conflict" {
		t.Fatalf("other product: %d %s", r.Status, r.Body)
	}
	for i, bad := range []map[string]any{
		{"product_id": recharge}, {"customer_id": "user 1", "product_id": recharge}, {"customer_id": strings.Repeat("a", 129), "product_id": recharge},
		{"customer_id": "u1"}, {"customer_id": "u1", "product_id": recharge, "amount": "5.00"},
	} {
		if r := app.Do("POST", "/v1/customer_addresses", app.TestKey, bad); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
			t.Errorf("bad %d %v: %d %s", i, bad, r.Status, r.Body)
		}
	}
	if r := app.Do("POST", "/v1/customer_addresses", app.TestKey, map[string]any{"customer_id": "u1", "product_id": "prod_01JBNOPE"}); r.Status != http.StatusNotFound ||
		r.ErrorCode(t) != "product_not_found" {
		t.Fatalf("unknown product: %d %s", r.Status, r.Body)
	}
	// The other mode has its own addresses: a test product does not exist for a live key.
	if r := app.Do("POST", "/v1/customer_addresses", app.LiveKey, map[string]any{"customer_id": "user_8412", "product_id": recharge}); r.Status != http.StatusNotFound {
		t.Fatalf("test product with a live key: %d %s", r.Status, r.Body)
	}

	// An Idempotency-Key replays the first answer, status included.
	body := map[string]any{"customer_id": "user_9", "product_id": founder}
	first := app.Do("POST", "/v1/customer_addresses", app.TestKey, body, idem("caddr-1")...)
	replay := app.Do("POST", "/v1/customer_addresses", app.TestKey, body, idem("caddr-1")...)
	if first.Status != http.StatusCreated || replay.Status != http.StatusCreated || replay.Header.Get("Idempotent-Replayed") != "true" ||
		replay.JSON(t)["id"] != first.JSON(t)["id"] {
		t.Fatalf("replay: %d %s / %d %s", first.Status, first.Body, replay.Status, replay.Body)
	}
	if r := app.Do("POST", "/v1/customer_addresses", app.TestKey, map[string]any{"customer_id": "user_10", "product_id": founder}, idem("caddr-1")...); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "idempotency_conflict" {
		t.Fatalf("key reuse: %d %s", r.Status, r.Body)
	}

	if got := app.Do("GET", "/v1/customer_addresses/"+id, app.TestKey, nil); got.Status != http.StatusOK || got.JSON(t)["id"] != id {
		t.Fatalf("get: %d %s", got.Status, got.Body)
	}
	if r := app.Do("GET", "/v1/customer_addresses/caddr_01JBNOPE", app.TestKey, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "customer_address_not_found" {
		t.Fatalf("get unknown: %d %s", r.Status, r.Body)
	}
	if r := app.Do("GET", "/v1/customer_addresses/"+id, app.LiveKey, nil); r.Status != http.StatusNotFound {
		t.Fatalf("get with the other mode: %d %s", r.Status, r.Body)
	}
	list := app.Do("GET", "/v1/customer_addresses?customer_id=user_8412", app.TestKey, nil).JSON(t)
	if data := list["data"].([]any); len(data) != 1 || data[0].(map[string]any)["id"] != id || list["next_cursor"] != nil {
		t.Fatalf("list of user_8412: %v", list)
	}
	// Two addresses, one per page.
	page := app.Do("GET", "/v1/customer_addresses?limit=1", app.TestKey, nil).JSON(t)
	next, _ := page["next_cursor"].(string)
	rest := app.Do("GET", "/v1/customer_addresses?limit=1&cursor="+next, app.TestKey, nil).JSON(t)
	seen := []string{page["data"].([]any)[0].(map[string]any)["id"].(string), rest["data"].([]any)[0].(map[string]any)["id"].(string)}
	if next == "" || len(page["data"].([]any)) != 1 || rest["next_cursor"] != nil || seen[0] <= seen[1] ||
		!slices.Contains(seen, id) || !slices.Contains(seen, first.JSON(t)["id"].(string)) {
		t.Fatalf("pages %v / %v", page, rest)
	}
}

// A session on a customer address reuses the address's salt and inputs: every session on it
// derives exactly the address's deposit addresses, and one session is open at a time.
func TestSessionsOnACustomerAddress(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	recharge := app.Link(app.TestKey)
	a := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_8412", "product_id": recharge, "metadata": map[string]any{"tier": "gold"}}, http.StatusCreated)
	id := a["id"].(string)
	want := addressesOf(t, a)

	s1 := mustOpenOn(t, app, app.TestKey, id, nil, "on-1")
	if s1["customer_address_id"] != id || s1["amount"] != nil || s1["amount_remaining"] != "0.00" || s1["status"] != "open" ||
		s1["product_id"] != recharge || s1["metadata"].(map[string]any)["tier"] != "gold" {
		t.Fatalf("open-amount session %v", s1)
	}
	for _, x := range s1["pay_to"].([]any) {
		if x.(map[string]any)["amount"] != nil {
			t.Fatalf("open-amount pay_to with an amount: %v", x)
		}
	}
	if got := addressesOf(t, s1); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("session addresses %v, customer address %v", got, want)
	}
	// One open session per address: opening again answers it, whatever the request.
	same := mustOpenOn(t, app, app.TestKey, id, map[string]any{"amount": "10.00"}, "on-2")
	if same["id"] != s1["id"] {
		t.Fatalf("second open %v, first %v", same["id"], s1["id"])
	}

	app.Clock.Advance(2 * time.Hour)
	s2 := mustOpenOn(t, app, app.TestKey, id, map[string]any{"amount": "10.00", "product_id": recharge, "metadata": map[string]any{"order": "7"}}, "on-3")
	if s2["id"] == s1["id"] || s2["amount"] != "10.00" || s2["customer_address_id"] != id {
		t.Fatalf("session after expiry %v", s2)
	}
	if got := addressesOf(t, s2); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("second session addresses %v, customer address %v", got, want)
	}
	md := s2["metadata"].(map[string]any)
	if md["tier"] != "gold" || md["order"] != "7" {
		t.Fatalf("metadata %v", md)
	}
	for _, x := range s2["pay_to"].([]any) {
		if p := x.(map[string]any); p["amount"] == nil || !strings.HasPrefix(p["amount"].(string), "10.") {
			t.Fatalf("pay_to amount %v", p)
		}
	}
	// Both sessions carry the address's salt and re-derive to its addresses.
	ca, err := app.Store.GetCustomerAddress(t.Context(), app.MerchantID, false, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{s1["id"].(string), s2["id"].(string)} {
		ses, err := app.Store.LoadSession(t.Context(), sid)
		if err != nil {
			t.Fatal(err)
		}
		if ses.Salt != ca.Salt || ses.CustomerAddressID == nil || *ses.CustomerAddressID != id {
			t.Fatalf("session %s salt or address differs from the customer address's", sid)
		}
		rederive(t, app, ses)
	}

	if r := openOn(t, app, app.TestKey, id, map[string]any{"product_id": app.Link(app.LiveKey)}, "on-4"); r.Status != http.StatusBadRequest {
		t.Fatalf("another product: %d %s", r.Status, r.Body)
	}
	if r := openOn(t, app, app.TestKey, "caddr_01JBNOPE", nil, "on-5"); r.Status != http.StatusNotFound || r.ErrorCode(t) != "customer_address_not_found" {
		t.Fatalf("unknown address: %d %s", r.Status, r.Body)
	}
	if r := openOn(t, app, app.LiveKey, id, nil, "on-6"); r.Status != http.StatusNotFound {
		t.Fatalf("other mode: %d %s", r.Status, r.Body)
	}
	sessions := app.Do("GET", "/v1/customer_addresses/"+id+"/sessions", app.TestKey, nil).JSON(t)
	data := sessions["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["id"] != s2["id"] || data[1].(map[string]any)["id"] != s1["id"] {
		t.Fatalf("sessions of the address: %v", sessions)
	}
	if r := app.Do("GET", "/v1/customer_addresses/caddr_01JBNOPE/sessions", app.TestKey, nil); r.Status != http.StatusNotFound {
		t.Fatalf("sessions of an unknown address: %d %s", r.Status, r.Body)
	}

	// A fixed price sets the amount, and the range of an open price still applies to a sent amount.
	founder := createProduct(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	f := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_77", "product_id": founder}, http.StatusCreated)
	if r := openOn(t, app, app.TestKey, f["id"].(string), map[string]any{"amount": "5.00"}, "on-7"); r.Status != http.StatusBadRequest {
		t.Fatalf("amount on a fixed product: %d %s", r.Status, r.Body)
	}
	if s := mustOpenOn(t, app, app.TestKey, f["id"].(string), nil, "on-8"); s["amount"] != "25.00" {
		t.Fatalf("fixed session %v", s)
	}
	g := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_78", "product_id": recharge}, http.StatusCreated)
	if r := openOn(t, app, app.TestKey, g["id"].(string), map[string]any{"amount": "0.10"}, "on-9"); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != "amount_below_minimum" {
		t.Fatalf("amount below the range: %d %s", r.Status, r.Body)
	}
}

// Payout and wallet changes never reach an address: its sessions keep paying the recipients it
// was created with until it is rotated. Rotating gives a new address with the payout now and
// leaves the old sessions untouched.
func TestRotateCustomerAddress(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.TestKey
	evmTest := ""
	for _, c := range app.Registry.ForMode(false) {
		if c.Family == chain.EVM {
			evmTest = c.ID
			break
		}
	}
	w1 := walletID(t, app, key, apptest.EVMDestination)
	prod := newProduct(t, app, key, nil)["id"].(string)
	a := newAddress(t, app, key, map[string]any{"customer_id": "user_1", "product_id": prod}, http.StatusCreated)
	id := a["id"].(string)
	if got := payoutWallets(t, a, evmTest); !slices.Equal(got, []string{w1}) {
		t.Fatalf("address payout %v", got)
	}
	s1 := mustOpenOn(t, app, key, id, nil, "rot-1")

	w2 := app.Wallet(key, chain.EVM, "Second", "0x2546BcD3c84621e976D8185a91A922aE77ECEc30").ID
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"payout": map[string]any{evmTest: items(w2, "100")}}); r.Status != http.StatusOK {
		t.Fatalf("payout change: %d %s", r.Status, r.Body)
	}
	// The old wallet no longer pays any product, so it can be archived; the address keeps it.
	app.ArchiveWallet(key, w1)
	app.Clock.Advance(2 * time.Hour)
	s2 := mustOpenOn(t, app, key, id, nil, "rot-2")
	if got := payoutWallets(t, s2, evmTest); !slices.Equal(got, []string{w1}) || fmt.Sprint(addressesOf(t, s2)) != fmt.Sprint(addressesOf(t, a)) {
		t.Fatalf("session after the payout change pays %v at %v", got, addressesOf(t, s2))
	}

	r := app.Do("POST", "/v1/customer_addresses/"+id+"/rotate", key, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("rotate: %d %s", r.Status, r.Body)
	}
	b := r.JSON(t)
	bid := b["id"].(string)
	if bid == id || b["status"] != "active" || b["customer_id"] != "user_1" || b["product_id"] != prod {
		t.Fatalf("new address %v", b)
	}
	if got := payoutWallets(t, b, evmTest); !slices.Equal(got, []string{w2}) {
		t.Fatalf("new address payout %v", got)
	}
	for k, v := range addressesOf(t, b) {
		if addressesOf(t, a)[k] == v {
			t.Fatalf("rotated %s keeps the address %s", k, v)
		}
	}
	old := app.Do("GET", "/v1/customer_addresses/"+id, key, nil).JSON(t)
	if old["status"] != "rotated" || old["replaced_by"] != bid || old["rotated_at"] == nil ||
		fmt.Sprint(addressesOf(t, old)) != fmt.Sprint(addressesOf(t, a)) {
		t.Fatalf("old address after rotate %v", old)
	}
	// The session open on the old address runs on, unchanged; no new one opens there.
	for _, s := range []map[string]any{s1, s2} {
		got := app.Do("GET", "/v1/sessions/"+s["id"].(string), key, nil).JSON(t)
		if fmt.Sprint(addressesOf(t, got)) != fmt.Sprint(addressesOf(t, a)) || got["customer_address_id"] != id {
			t.Fatalf("session %s after rotate %v", s["id"], got)
		}
	}
	app.Clock.Advance(2 * time.Hour)
	if r := openOn(t, app, key, id, nil, "rot-3"); r.Status != http.StatusConflict || r.ErrorCode(t) != "address_rotated" || !strings.Contains(string(r.Body), bid) {
		t.Fatalf("session on a rotated address: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/customer_addresses/"+id+"/rotate", key, nil); r.Status != http.StatusConflict || r.ErrorCode(t) != "address_rotated" {
		t.Fatalf("rotate twice: %d %s", r.Status, r.Body)
	}
	if got := newAddress(t, app, key, map[string]any{"customer_id": "user_1", "product_id": prod}, http.StatusOK); got["id"] != bid {
		t.Fatalf("get-or-create after rotate answers %v, want %s", got["id"], bid)
	}
	s3 := mustOpenOn(t, app, key, bid, nil, "rot-4")
	if fmt.Sprint(addressesOf(t, s3)) != fmt.Sprint(addressesOf(t, b)) || !slices.Equal(payoutWallets(t, s3, evmTest), []string{w2}) {
		t.Fatalf("session on the new address %v", s3)
	}
	list := app.Do("GET", "/v1/customer_addresses?customer_id=user_1", key, nil).JSON(t)["data"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["id"] != bid || list[1].(map[string]any)["id"] != id {
		t.Fatalf("customer's addresses %v", list)
	}
	if r := app.Do("POST", "/v1/customer_addresses/caddr_01JBNOPE/rotate", key, nil); r.Status != http.StatusNotFound {
		t.Fatalf("rotate unknown: %d %s", r.Status, r.Body)
	}
}

// An open-amount session is paid by the first confirmed sum that settles after the fee and
// keeps crediting payments after that, with a single session.paid.
func TestOpenAmountSessionSimulated(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	a := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_1", "product_id": app.Link(app.TestKey)}, http.StatusCreated)
	s := mustOpenOn(t, app, app.TestKey, a["id"].(string), nil, "oa-1")
	sim := func(body map[string]any) apptest.Response {
		return app.Do("POST", "/v1/test_helpers/sessions/"+s["id"].(string)+"/simulate", app.TestKey, body)
	}
	if r := sim(map[string]any{"action": "pay_exact"}); r.Status != http.StatusBadRequest {
		t.Fatalf("pay_exact without an amount: %d %s", r.Status, r.Body)
	}
	if r := sim(map[string]any{"action": "pay_partial", "amount": "1"}); r.Status != http.StatusConflict {
		t.Fatalf("pay_partial: %d %s", r.Status, r.Body)
	}
	// Dust alone does not pay.
	r := sim(map[string]any{"action": "pay_exact", "amount": "0.01"})
	if r.Status != http.StatusOK || r.JSON(t)["session"].(map[string]any)["status"] != "open" {
		t.Fatalf("dust: %d %s", r.Status, r.Body)
	}
	r = sim(map[string]any{"action": "pay_exact", "amount": "40"})
	if r.Status != http.StatusOK {
		t.Fatalf("pay 40: %d %s", r.Status, r.Body)
	}
	got := r.JSON(t)
	ses := got["session"].(map[string]any)
	if ses["status"] != "paid" || ses["amount"] != nil || ses["amount_received"] != "40.01" {
		t.Fatalf("after 40: %v", ses)
	}
	var types []string
	for _, e := range got["events"].([]any) {
		types = append(types, e.(map[string]any)["type"].(string))
	}
	if !slices.Contains(types, "session.paid") {
		t.Fatalf("events %v", types)
	}
}

// Concurrent requests never make two active addresses for a customer or two open sessions on an
// address, and session opens, rotations and product payout edits never deadlock (lock order:
// product, then customer address).
func TestCustomerAddressConcurrency(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.TestKey
	prod := newProduct(t, app, key, nil)["id"].(string)
	evmTest := ""
	for _, c := range app.Registry.ForMode(false) {
		if c.Family == chain.EVM {
			evmTest = c.ID
			break
		}
	}
	w1 := walletID(t, app, key, apptest.EVMDestination)
	w2 := app.Wallet(key, chain.EVM, "Second", "0x2546BcD3c84621e976D8185a91A922aE77ECEc30").ID

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	got := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := app.Do("POST", "/v1/customer_addresses", key, map[string]any{"customer_id": "user_1", "product_id": prod})
			statuses[i] = r.Status
			if r.Status < 300 {
				got[i] = r.JSON(t)["id"].(string)
			}
		}()
	}
	wg.Wait()
	created := 0
	for i := range n {
		if statuses[i] == http.StatusCreated {
			created++
		}
		if (statuses[i] != http.StatusCreated && statuses[i] != http.StatusOK) || got[i] != got[0] {
			t.Fatalf("concurrent creates: %v %v", statuses, got)
		}
	}
	if created != 1 {
		t.Fatalf("%d creates answered 201", created)
	}
	addr := got[0]
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := openOn(t, app, key, addr, nil, fmt.Sprintf("conc-%d", i))
			statuses[i] = r.Status
			if r.Status == http.StatusOK {
				got[i] = r.JSON(t)["id"].(string)
			}
		}()
	}
	wg.Wait()
	for i := range n {
		if statuses[i] != http.StatusOK || got[i] != got[0] {
			t.Fatalf("concurrent opens: %v %v", statuses, got)
		}
	}

	// Opens on fresh addresses, rotations and a payout edit of the product, all at once. Kept
	// under the pool size (10): a product edit reads wallets through the pool while it holds the
	// product row, so more waiters than connections would starve it (not a lock-order question).
	for round := range 5 {
		var errs sync.Map
		for i := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cust := fmt.Sprintf("r%d-u%d", round, i)
				r := app.Do("POST", "/v1/customer_addresses", key, map[string]any{"customer_id": cust, "product_id": prod})
				if r.Status != http.StatusCreated {
					errs.Store(cust+" create", fmt.Sprintf("%d %s", r.Status, r.Body))
					return
				}
				id := r.JSON(t)["id"].(string)
				var inner sync.WaitGroup
				inner.Add(2)
				go func() {
					defer inner.Done()
					if r := openOn(t, app, key, id, nil, "edit-"+cust); r.Status != http.StatusOK && r.ErrorCode(t) != "address_rotated" {
						errs.Store(cust+" open", fmt.Sprintf("%d %s", r.Status, r.Body))
					}
				}()
				go func() {
					defer inner.Done()
					if r := app.Do("POST", "/v1/customer_addresses/"+id+"/rotate", key, nil); r.Status != http.StatusOK {
						errs.Store(cust+" rotate", fmt.Sprintf("%d %s", r.Status, r.Body))
					}
				}()
				inner.Wait()
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := w1
			if round%2 == 0 {
				w = w2
			}
			if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"payout": map[string]any{evmTest: items(w, "100")}}); r.Status != http.StatusOK {
				errs.Store("edit", fmt.Sprintf("%d %s", r.Status, r.Body))
			}
		}()
		wg.Wait()
		errs.Range(func(k, v any) bool {
			t.Errorf("round %d %v: %v", round, k, v)
			return true
		})
	}
}

// D50: opening on a customer address answers the address's latest session only while it waits
// for payment (open or partially paid, not expired). Once it is paid, or expired, the next open
// makes a new session on the same deposit addresses, which becomes the address's current session.
func TestOpeningAfterTheSessionIsPaidOpensANewOne(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	a := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_46", "product_id": app.Link(app.TestKey)}, http.StatusCreated)
	id := a["id"].(string)
	sim := func(ses string, body map[string]any) {
		t.Helper()
		if r := app.Do("POST", "/v1/test_helpers/sessions/"+ses+"/simulate", app.TestKey, body); r.Status != http.StatusOK {
			t.Fatalf("simulate %v: %d %s", body, r.Status, r.Body)
		}
	}
	current := func() string {
		t.Helper()
		got, err := app.Store.CurrentAddressSession(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	s1 := mustOpenOn(t, app, app.TestKey, id, map[string]any{"amount": "10.00"}, "d46-1")["id"].(string)
	if current() != s1 {
		t.Fatalf("current session %s, want %s", current(), s1)
	}
	sim(s1, map[string]any{"action": "pay_partial", "amount": "4"})
	if again := mustOpenOn(t, app, app.TestKey, id, nil, "d46-2"); again["id"] != s1 || again["status"] != "partially_paid" {
		t.Fatalf("open while partially paid: %v", again)
	}
	sim(s1, map[string]any{"action": "pay_exact"})

	s2 := mustOpenOn(t, app, app.TestKey, id, map[string]any{"amount": "15.00"}, "d46-3")
	if s2["id"] == s1 || s2["status"] != "open" || s2["amount"] != "15.00" || fmt.Sprint(addressesOf(t, s2)) != fmt.Sprint(addressesOf(t, a)) {
		t.Fatalf("open after payment: %v", s2)
	}
	if current() != s2["id"] {
		t.Fatalf("current session %s, want %s", current(), s2["id"])
	}
	if paid := app.Do("GET", "/v1/sessions/"+s1, app.TestKey, nil).JSON(t); paid["status"] != "paid" {
		t.Fatalf("first session %v", paid)
	}
	// An open-amount session that is paid is closed to new opens too.
	app.Clock.Advance(2 * time.Hour)
	s3 := mustOpenOn(t, app, app.TestKey, id, nil, "d46-4")["id"].(string)
	if s3 == s2["id"] {
		t.Fatal("an expired session was answered")
	}
	sim(s3, map[string]any{"action": "pay_exact", "amount": "30"})
	s4 := mustOpenOn(t, app, app.TestKey, id, nil, "d46-5")["id"].(string)
	if s4 == s3 || current() != s4 {
		t.Fatalf("open after a paid open-amount session: %s (paid %s, current %s)", s4, s3, current())
	}
	list := app.Do("GET", "/v1/customer_addresses/"+id+"/sessions", app.TestKey, nil).JSON(t)["data"].([]any)
	if len(list) != 4 {
		t.Fatalf("%d sessions on the address, want 4", len(list))
	}
}

// Customers never share an address or a session: 50 customers of one merchant opening on the
// same product at once get 50 addresses and 50 sessions, each with its own metadata.
func TestFiftyCustomersOpeningAtOnce(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	prod := app.Link(app.TestKey)
	const n = 50
	type result struct {
		addr, ses map[string]any
		err       string
	}
	out := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cust := fmt.Sprintf("cust-%02d", i)
			r := app.Do("POST", "/v1/customer_addresses", app.TestKey, map[string]any{"customer_id": cust, "product_id": prod,
				"metadata": map[string]any{"customer": cust}})
			if r.Status != http.StatusCreated {
				out[i].err = fmt.Sprintf("create %s: %d %s", cust, r.Status, r.Body)
				return
			}
			out[i].addr = r.JSON(t)
			s := openOn(t, app, app.TestKey, out[i].addr["id"].(string), map[string]any{"metadata": map[string]any{"order": fmt.Sprint(i)}}, "fifty-"+cust)
			if s.Status != http.StatusOK {
				out[i].err = fmt.Sprintf("open %s: %d %s", cust, s.Status, s.Body)
				return
			}
			out[i].ses = s.JSON(t)
		}()
	}
	wg.Wait()
	addrs, sessions, deposits := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, r := range out {
		if r.err != "" {
			t.Fatal(r.err)
		}
		cust := fmt.Sprintf("cust-%02d", i)
		md := r.ses["metadata"].(map[string]any)
		if r.addr["customer_id"] != cust || r.ses["customer_address_id"] != r.addr["id"] || md["customer"] != cust || md["order"] != fmt.Sprint(i) {
			t.Fatalf("customer %s: address %v, session %v", cust, r.addr, r.ses)
		}
		addrs[r.addr["id"].(string)], sessions[r.ses["id"].(string)] = true, true
		for _, d := range addressesOf(t, r.ses) {
			deposits[d] = true
		}
	}
	chains := len(addressesOf(t, out[0].ses))
	if len(addrs) != n || len(sessions) != n || len(deposits) != n*chains {
		t.Fatalf("%d addresses, %d sessions, %d deposit addresses (%d chains), want %d each", len(addrs), len(sessions), len(deposits), chains, n)
	}
}
