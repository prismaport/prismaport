//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// evmAddr is a distinct, valid, never denylisted EVM address per n, checksummed.
func evmAddr(n int) string {
	a, err := evm.ParseAddress(fmt.Sprintf("0x%040x", 0xa11ce000+n))
	if err != nil {
		panic(err)
	}
	return a.Hex()
}

// newWallet adds an EVM wallet to the key's merchant and mode and answers its id.
func newWallet(t *testing.T, app *apptest.App, key, label, address string) string {
	t.Helper()
	return app.Wallet(key, chain.EVM, label, address).ID
}

func walletID(t *testing.T, app *apptest.App, key, address string) string {
	t.Helper()
	for _, w := range app.Do("GET", "/v1/wallets?include_archived=true", key, nil).JSON(t)["data"].([]any) {
		if m := w.(map[string]any); m["address"] == address {
			return m["id"].(string)
		}
	}
	t.Fatalf("no wallet %s", address)
	return ""
}

// items is a payout list for one network: wallet id, share, wallet id, share, ...
func items(pairs ...any) []map[string]any {
	out := []map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"wallet_id": pairs[i], "share": pairs[i+1]})
	}
	return out
}

func newProduct(t *testing.T, app *apptest.App, key string, payout map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"}
	if payout != nil {
		body["payout"] = payout
	}
	r := app.Do("POST", "/v1/products", key, body)
	if r.Status != http.StatusOK {
		t.Fatalf("product: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)
}

// payoutOf renders one network of a payout (a product's or a session's) as
// "wallet_id:share,wallet_id:share", "[]" for an empty list and "-" for an absent key.
func payoutOf(m map[string]any, chainID string) string {
	list, ok := m["payout"].(map[string]any)[chainID]
	if !ok {
		return "-"
	}
	var parts []string
	for _, x := range list.([]any) {
		it := x.(map[string]any)
		parts = append(parts, fmt.Sprintf("%v:%v", it["wallet_id"], it["share"]))
	}
	if len(parts) == 0 {
		return "[]"
	}
	return strings.Join(parts, ",")
}

func errorOf(t *testing.T, r apptest.Response) (int, string, string) {
	t.Helper()
	m := r.JSON(t)
	e, ok := m["error"].(map[string]any)
	if !ok {
		return r.Status, "", ""
	}
	return r.Status, e["code"].(string), e["message"].(string)
}

// The API reads wallets; adding and archiving them is the dashboard's (behind a confirmation
// code in Live mode).
func TestWallets(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	key := app.TestKey
	add := func(mode string, body map[string]any) apptest.Response {
		b := map[string]any{}
		for k, v := range body {
			b[k] = v
		}
		return c.do("POST", "/v1/dashboard/wallets?mode="+mode, b)
	}
	archive := func(id string) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets/"+id+"/archive?mode=test", nil)
	}
	list := app.Do("GET", "/v1/wallets", key, nil).JSON(t)["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("seeded wallets %v", list)
	}
	for _, w := range list {
		m := w.(map[string]any)
		if m["label"] != "Main wallet" || m["used_by_products"] != float64(0) || m["archived_at"] != nil || m["verified"] != true || m["livemode"] != false {
			t.Fatalf("seeded wallet %v", m)
		}
		if _, ok := m["used_by_splits"]; ok {
			t.Fatalf("a wallet still says used_by_splits: %v", m)
		}
	}
	if evm := app.Do("GET", "/v1/wallets?family=evm", key, nil).JSON(t)["data"].([]any); len(evm) != 1 || evm[0].(map[string]any)["address"] != apptest.EVMDestination {
		t.Fatalf("family filter %v", evm)
	}

	usdc, _ := chain.Pilot().Get("base-sepolia")
	token, _ := usdc.Asset("USDC")
	for name, tc := range map[string]struct {
		body map[string]any
		code string
		msg  string
	}{
		"bad checksum":      {map[string]any{"family": "evm", "address": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD"}, "invalid_request", "checksum"},
		"zero address":      {map[string]any{"family": "evm", "address": "0x0000000000000000000000000000000000000000"}, "invalid_request", "zero address"},
		"a token contract":  {map[string]any{"family": "evm", "address": token.Token}, "invalid_request", "token or protocol contract"},
		"our treasury":      {map[string]any{"family": "evm", "address": apptest.EVMTreasury}, "invalid_request", "token or protocol contract"},
		"not an address":    {map[string]any{"family": "evm", "address": "0x12"}, "invalid_request", "not a valid address"},
		"a Solana address":  {map[string]any{"family": "evm", "address": apptest.SolanaDestination}, "invalid_request", "not a valid address"},
		"no family":         {map[string]any{"address": evmAddr(1)}, "invalid_request", "family and address are required"},
		"unknown family":    {map[string]any{"family": "tron", "address": "T"}, "invalid_request", "evm or solana"},
		"a label too long":  {map[string]any{"family": "evm", "address": evmAddr(1), "label": strings.Repeat("x", 101)}, "invalid_request", "1 to 100"},
		"an unknown field":  {map[string]any{"family": "evm", "address": evmAddr(1), "destination": "x"}, "invalid_request", "unknown field"},
		"an active address": {map[string]any{"family": "evm", "address": strings.ToLower(apptest.EVMDestination)}, "invalid_request", "already has the address"},
	} {
		t.Run(name, func(t *testing.T) {
			status, code, msg := errorOf(t, add("test", tc.body))
			if status != http.StatusBadRequest || code != tc.code || !strings.Contains(msg, tc.msg) {
				t.Errorf("%s: %d %s %q", name, status, code, msg)
			}
		})
	}
	created := add("test", map[string]any{"family": "evm", "address": strings.ToLower(evmAddr(1))})
	if created.Status != http.StatusOK {
		t.Fatalf("new wallet: %d %s", created.Status, created.Body)
	}
	w := created.JSON(t)
	if w["address"] != evmAddr(1) || w["label"] != "Wallet "+evmAddr(1)[36:] || w["used_by_products"] != float64(0) || w["family"] != "evm" {
		t.Fatalf("new wallet (canonical address, default label) %v", w)
	}
	if g := app.Do("GET", "/v1/wallets/"+w["id"].(string), key, nil); g.Status != 200 || g.JSON(t)["id"] != w["id"] {
		t.Fatalf("get: %d %s", g.Status, g.Body)
	}
	for i := 2; i <= service.MaxActiveWallets-1; i++ {
		newWallet(t, app, key, fmt.Sprintf("W%d", i), evmAddr(i))
	}
	if status, _, msg := errorOf(t, add("test", map[string]any{"family": "evm", "address": evmAddr(99)})); status != 400 ||
		!strings.Contains(msg, "at most 20 active wallets") {
		t.Fatalf("wallet 21: %d %s", status, msg)
	}
	// Archive is refused while a product pays the wallet, and says how many.
	main := walletID(t, app, key, apptest.EVMDestination)
	newProduct(t, app, key, nil)
	newProduct(t, app, key, nil)
	if g := app.Do("GET", "/v1/wallets/"+main, key, nil).JSON(t); g["used_by_products"] != float64(2) {
		t.Fatalf("used by %v products", g["used_by_products"])
	}
	if status, code, msg := errorOf(t, archive(main)); status != 409 || code != "wallet_in_use" ||
		!strings.Contains(msg, "2 products pay this wallet") {
		t.Fatalf("archiving the wallet two products pay: %d %s %s", status, code, msg)
	}
	a := archive(w["id"].(string)).JSON(t)
	if a["archived_at"] == nil {
		t.Fatalf("archived %v", a)
	}
	if again := archive(w["id"].(string)); again.Status != 200 {
		t.Fatalf("archiving twice: %d %s", again.Status, again.Body)
	}
	if active := app.Do("GET", "/v1/wallets?family=evm", key, nil).JSON(t)["data"].([]any); len(active) != service.MaxActiveWallets-1 {
		t.Fatalf("active wallets %d", len(active))
	}
	all := app.Do("GET", "/v1/wallets?family=evm&include_archived=true", key, nil).JSON(t)["data"].([]any)
	if len(all) != service.MaxActiveWallets || all[len(all)-1].(map[string]any)["id"] != w["id"] {
		t.Fatalf("archived wallets come last: %v", all)
	}
	newWallet(t, app, key, "Back", evmAddr(1))
	if r := app.Do("GET", "/v1/wallets/wal_01JBNOPE", key, nil); r.Status != 404 {
		t.Fatalf("unknown wallet: %d", r.Status)
	}
	if r := app.Do("GET", "/v1/wallets/"+w["id"].(string), app.LiveKey, nil); r.Status != 404 {
		t.Fatalf("a test wallet in live mode: %d", r.Status)
	}

	app.Blacklist.List(evmAddr(77), true)
	if status, code, _ := errorOf(t, add("test", map[string]any{"family": "evm", "address": evmAddr(77)})); status != 400 || code != "wallet_blacklisted" {
		t.Fatalf("blacklisted wallet: %d %s", status, code)
	}
	app.Blacklist.Down(true)
	if status, code, _ := errorOf(t, add("test", map[string]any{"family": "solana", "address": "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx"})); status != 200 && code != "" {
		t.Fatalf("a Solana wallet does not need the USDC blacklist: %d %s", status, code)
	}
	if status, code, _ := errorOf(t, add("live", map[string]any{"family": "evm", "address": evmAddr(78)})); status != 503 || code != "service_unavailable" {
		t.Fatalf("an unreachable blacklist must fail closed: %d %s", status, code)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "wallet.created", "wallet.archived") {
		t.Fatalf("audit %v", auditActions(t, app, app.MerchantID))
	}
	for _, path := range []string{"/v1/splits", "/v1/splits/spl_01JBNOPE", "/v1/splits/defaults"} {
		if r := app.Do("GET", path, key, nil); r.Status != 404 {
			t.Errorf("%s still answers: %d %s", path, r.Status, r.Body)
		}
	}
}

// A product names where its money goes on each network of its mode; absent networks pay the
// family's oldest wallet at 100% on create and keep their items on a patch.
func TestProductPayoutAPI(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.TestKey
	main := walletID(t, app, key, apptest.EVMDestination)
	sol := walletID(t, app, key, apptest.SolanaDestination)
	partner := newWallet(t, app, key, "Partner", evmAddr(1))

	// The default: every network of the mode, the family's oldest active wallet at 100%, written
	// out (a wallet added later changes nothing).
	def := newProduct(t, app, key, nil)
	if payoutOf(def, "base-sepolia") != main+":100.00" || payoutOf(def, "solana-devnet") != sol+":100.00" || len(def["payout"].(map[string]any)) != 2 {
		t.Fatalf("default payout %v", def["payout"])
	}
	it := def["payout"].(map[string]any)["base-sepolia"].([]any)[0].(map[string]any)
	if it["label"] != "Main wallet" || it["address"] != apptest.EVMDestination || it["bps"] != float64(10_000) {
		t.Fatalf("payout item %v", it)
	}
	newWallet(t, app, key, "Newer", evmAddr(2))
	if g := app.Do("GET", "/v1/products/"+def["id"].(string), key, nil).JSON(t); payoutOf(g, "base-sepolia") != main+":100.00" {
		t.Fatalf("a new wallet changed a product: %v", g["payout"])
	}

	// Explicit: order kept (never sorted), [] takes no payments on that network.
	p := newProduct(t, app, key, map[string]any{"base-sepolia": items(partner, "70", main, "30.00"), "solana-devnet": []any{}})
	if payoutOf(p, "base-sepolia") != partner+":70.00,"+main+":30.00" || payoutOf(p, "solana-devnet") != "[]" {
		t.Fatalf("explicit payout %v", p["payout"])
	}
	id := p["id"].(string)
	// A patch replaces the networks it names and keeps the others.
	up := app.Do("PATCH", "/v1/products/"+id, key, map[string]any{"payout": map[string]any{"solana-devnet": items(sol, "100")}}).JSON(t)
	if payoutOf(up, "base-sepolia") != partner+":70.00,"+main+":30.00" || payoutOf(up, "solana-devnet") != sol+":100.00" {
		t.Fatalf("patched payout %v", up["payout"])
	}
	up = app.Do("PATCH", "/v1/products/"+id, key, map[string]any{"name": "Renamed"}).JSON(t)
	if payoutOf(up, "base-sepolia") != partner+":70.00,"+main+":30.00" {
		t.Fatalf("a patch without payout changed it %v", up["payout"])
	}
	if status, _, msg := errorOf(t, app.Do("PATCH", "/v1/products/"+id, key, map[string]any{"payout": map[string]any{"base-sepolia": []any{}, "solana-devnet": []any{}}})); status != 400 ||
		!strings.Contains(msg, "at least one network") {
		t.Fatalf("a product paying nothing: %d %s", status, msg)
	}

	six := []map[string]any{}
	for i := 0; i < 6; i++ {
		six = append(six, map[string]any{"wallet_id": fmt.Sprintf("wal_%d", i), "share": "10"})
	}
	archived := newWallet(t, app, key, "Gone", evmAddr(3))
	app.ArchiveWallet(key, archived)
	for name, c := range map[string]struct {
		payout any
		code   string
		msg    string
	}{
		"shares below 100%":     {map[string]any{"base-sepolia": items(main, "60", partner, "30")}, "invalid_request", "Shares must add up to 100%. Now 90%."},
		"shares above 100%":     {map[string]any{"base-sepolia": items(main, "70.5", partner, "30")}, "invalid_request", "Now 100.5%."},
		"six wallets":           {map[string]any{"base-sepolia": six}, "invalid_request", "A split pays 1 to 5 wallets."},
		"a wallet twice":        {map[string]any{"base-sepolia": items(main, "50", main, "50")}, "invalid_request", "Each wallet can appear only once in a split."},
		"a zero share":          {map[string]any{"base-sepolia": items(main, "100", partner, "0")}, "invalid_request", "payout.base-sepolia[1].share"},
		"three decimals":        {map[string]any{"base-sepolia": items(main, "99.999", partner, "0.001")}, "invalid_request", "payout.base-sepolia[0].share"},
		"a Solana wallet":       {map[string]any{"base-sepolia": items(sol, "100")}, "invalid_request", "not an active EVM wallet"},
		"an EVM wallet":         {map[string]any{"solana-devnet": items(main, "100")}, "invalid_request", "not an active Solana wallet"},
		"an unknown wallet":     {map[string]any{"base-sepolia": items("wal_nope", "100")}, "invalid_request", "not an active EVM wallet"},
		"an archived wallet":    {map[string]any{"base-sepolia": items(archived, "100")}, "invalid_request", "not an active EVM wallet"},
		"a live network":        {map[string]any{"base": items(main, "100")}, "unknown_chain", `unknown network "base"`},
		"an unknown network":    {map[string]any{"tron": items(main, "100")}, "unknown_chain", "base-sepolia, solana-devnet"},
		"a family key":          {map[string]any{"evm": items(main, "100")}, "unknown_chain", `"evm"`},
		"nothing at all":        {map[string]any{"base-sepolia": []any{}, "solana-devnet": []any{}}, "invalid_request", "at least one network"},
		"a null payout":         {nil, "invalid_request", "keyed by network"},
		"a null network":        {map[string]any{"base-sepolia": nil}, "invalid_request", "[] takes no payments"},
		"a list of strings":     {map[string]any{"base-sepolia": []string{main}}, "invalid_request", "list of {wallet_id, share}"},
		"an item without id":    {map[string]any{"base-sepolia": []map[string]any{{"share": "100"}}}, "invalid_request", "needs wallet_id and share"},
		"a numeric share":       {map[string]any{"base-sepolia": []map[string]any{{"wallet_id": main, "share": 100}}}, "invalid_request", "share must be"},
		"an unknown item field": {map[string]any{"base-sepolia": []map[string]any{{"wallet_id": main, "share": "100", "bps": 10000}}}, "invalid_request", `unknown field "bps"`},
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00", "payout": c.payout}
			status, code, msg := errorOf(t, app.Do("POST", "/v1/products", key, body))
			if status != 400 || code != c.code || !strings.Contains(msg, c.msg) {
				t.Errorf("%s: %d %s %q", name, status, code, msg)
			}
		})
	}
	// Errors name the field they are about.
	for name, c := range map[string]struct {
		payout any
		param  string
	}{
		"a share":         {map[string]any{"base-sepolia": items(main, "60", partner, "x")}, "payout.base-sepolia[1].share"},
		"the sum":         {map[string]any{"base-sepolia": items(main, "60", partner, "30")}, "payout.base-sepolia"},
		"a wallet":        {map[string]any{"solana-devnet": items(sol, "50", main, "50")}, "payout.solana-devnet[1].wallet_id"},
		"a repeat":        {map[string]any{"base-sepolia": items(main, "50", main, "50")}, "payout.base-sepolia[1].wallet_id"},
		"a network":       {map[string]any{"base": items(main, "100")}, "payout.base"},
		"no network":      {map[string]any{"base-sepolia": []any{}, "solana-devnet": []any{}}, "payout"},
		"a missing share": {map[string]any{"base-sepolia": []map[string]any{{"wallet_id": main}}}, "payout.base-sepolia[0].share"},
	} {
		t.Run(name, func(t *testing.T) {
			r := app.Do("POST", "/v1/products", key, map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00", "payout": c.payout})
			if e, _ := r.JSON(t)["error"].(map[string]any); e == nil || e["param"] != c.param {
				t.Errorf("%s: param %s", name, r.Body)
			}
		})
	}
	if r := app.Do("POST", "/v1/products", key, map[string]any{"name": "x", "price_type": "fixed", "amount": "0"}); strings.Contains(string(r.Body), `"param"`) {
		t.Errorf("an error about no field carries a param: %s", r.Body)
	}

	// The live mode has its own networks.
	live := newProduct(t, app, app.LiveKey, map[string]any{"solana": []any{}})
	if payoutOf(live, "base") != walletID(t, app, app.LiveKey, apptest.EVMDestination)+":100.00" || payoutOf(live, "solana") != "[]" ||
		payoutOf(live, "base-sepolia") != "-" {
		t.Fatalf("live payout %v", live["payout"])
	}

	// A merchant without wallets in a mode cannot make a product there.
	m, err := app.Store.CreateMerchant(t.Context(), "Empty", "test")
	if err != nil {
		t.Fatal(err)
	}
	_, emptyKey, err := app.Svc.CreateAPIKey(t.Context(), m.ID, false, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if status, _, msg := errorOf(t, app.Do("POST", "/v1/products", emptyKey, map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00"})); status != 400 ||
		!strings.Contains(msg, "Add a wallet first") {
		t.Fatalf("a product without any wallet: %d %s", status, msg)
	}

	// The USDC blacklist is checked on every save, the default included.
	app.Blacklist.List(evmAddr(1), true)
	if status, code, msg := errorOf(t, app.Do("PATCH", "/v1/products/"+def["id"].(string), key, map[string]any{"payout": map[string]any{"base-sepolia": items(partner, "100")}})); status != 400 ||
		code != "wallet_blacklisted" || !strings.Contains(msg, "Partner") {
		t.Fatalf("saving a blacklisted wallet: %d %s %s", status, code, msg)
	}
	app.Blacklist.List(evmAddr(1), false)
	app.Blacklist.List(apptest.EVMDestination, true)
	if status, code, _ := errorOf(t, app.Do("POST", "/v1/products", key, map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00"})); status != 400 ||
		code != "wallet_blacklisted" {
		t.Fatalf("a default payout to a blacklisted wallet: %d %s", status, code)
	}
	app.Blacklist.List(apptest.EVMDestination, false)

	if !hasAll(auditActions(t, app, app.MerchantID), "product.created", "product.payout_changed", "product.updated") {
		t.Fatalf("audit %v", auditActions(t, app, app.MerchantID))
	}
}

func payToAddress(m map[string]any, chainID string) string {
	for _, p := range m["pay_to"].([]any) {
		if p.(map[string]any)["chain"] == chainID {
			a, _ := p.(map[string]any)["address"].(string)
			return a
		}
	}
	return ""
}

// Every session comes from a product and commits to the recipients its product paid when it
// opened: a later change to the product never reaches it, and its deposit address re-derives
// from them.
func TestSessionPayoutSnapshot(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	key := app.TestKey
	main := walletID(t, app, key, apptest.EVMDestination)
	sol := walletID(t, app, key, apptest.SolanaDestination)
	partner := newWallet(t, app, key, "Partner", evmAddr(1))

	for name, c := range map[string]struct {
		body any
		msg  string
	}{
		"a bare amount":        {map[string]any{"amount": "25.00"}, "product_id is required"},
		"nothing":              {map[string]any{}, "product_id is required"},
		"a session payout":     {map[string]any{"product_id": app.Link(key), "amount": "25.00", "payout": map[string]any{"base-sepolia": items(main, "100")}}, `unknown field "payout"`},
		"an unknown product":   {map[string]any{"product_id": "prod_01JBNOPE", "amount": "25.00"}, "no such product"},
		"a product of a mode":  {map[string]any{"product_id": app.Link(app.LiveKey), "amount": "25.00"}, "no such product"},
		"an empty product id":  {map[string]any{"product_id": "", "amount": "25.00"}, "no such product"},
		"a product id of null": {map[string]any{"product_id": nil, "amount": "25.00"}, "product_id is required"},
	} {
		t.Run(name, func(t *testing.T) {
			status, _, msg := errorOf(t, app.Do("POST", "/v1/sessions", key, c.body, idem("bad-"+name)...))
			if (status != 400 && status != 404) || !strings.Contains(msg, c.msg) {
				t.Errorf("%s: %d %q", name, status, msg)
			}
		})
	}

	prod := newProduct(t, app, key, map[string]any{"base-sepolia": items(main, "70", partner, "30")})
	first := app.RevealedSession(key, map[string]any{"product_id": prod["id"]}, idem("s1")...).JSON(t)
	if payoutOf(first, "base-sepolia") != main+":70.00,"+partner+":30.00" || payoutOf(first, "solana-devnet") != sol+":100.00" {
		t.Fatalf("snapshot %v", first["payout"])
	}
	ses, err := app.Store.LoadSession(t.Context(), first["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := app.Registry.Get("base-sepolia")
	recipients := ses.Recipients("base-sepolia")
	var symbols []string
	for _, pt := range ses.PayTo {
		if pt.Chain == c.ID {
			symbols = append(symbols, pt.Asset)
		}
	}
	tokens, err := derive.BoundTokens(c, symbols)
	if err != nil {
		t.Fatal(err)
	}
	want, err := apptest.AnyMode{}.Address(c, ses.Salt, derive.Params{Treasury: apptest.EVMTreasury, Fee: c.Fee, Recipients: recipients, Tokens: tokens})
	if err != nil || payToAddress(first, "base-sepolia") != want || len(recipients) != 2 || recipients[1].Address != evmAddr(1) || recipients[1].Bps != 3000 {
		t.Fatalf("address %s does not re-derive from the snapshot (%s, %v)", payToAddress(first, "base-sepolia"), want, err)
	}

	app.Do("PATCH", "/v1/products/"+prod["id"].(string), key, map[string]any{"payout": map[string]any{"base-sepolia": items(partner, "50", main, "50")}})
	again := app.Do("GET", "/v1/sessions/"+first["id"].(string), key, nil).JSON(t)
	if fmt.Sprint(again["payout"]) != fmt.Sprint(first["payout"]) || payToAddress(again, "base-sepolia") != payToAddress(first, "base-sepolia") {
		t.Fatalf("an open session changed with its product:\n%v\n%v", first["payout"], again["payout"])
	}
	second := app.Session(key, map[string]any{"product_id": prod["id"]}, idem("s2")...).JSON(t)
	if payoutOf(second, "base-sepolia") != partner+":50.00,"+main+":50.00" {
		t.Fatalf("a new session after the change %v", second["payout"])
	}

	// A session offers only the networks its product pays.
	evmOnly := newProduct(t, app, key, map[string]any{"solana-devnet": []any{}})
	s := app.RevealedSession(key, map[string]any{"product_id": evmOnly["id"]}, idem("s3")...).JSON(t)
	if payoutOf(s, "solana-devnet") != "-" || payToAddress(s, "solana-devnet") != "" || payToAddress(s, "base-sepolia") == "" {
		t.Fatalf("a session of an EVM-only product %v %v", s["payout"], s["pay_to"])
	}

	prodLink := apiLink(t, app, key, prod["id"].(string), nil)
	public := openLink(app, prodLink, "pub-1", nil)
	if public.Status != 200 {
		t.Fatalf("public open: %d %s", public.Status, public.Body)
	}
	pubSes, _ := app.Store.LoadSession(t.Context(), public.JSON(t)["id"].(string))
	if rs := pubSes.Recipients("base-sepolia"); len(rs) != 2 || rs[0].Address != evmAddr(1) || rs[0].Bps != 5000 {
		t.Fatalf("public link session snapshot %v", rs)
	}
	if bytes := string(public.Body); strings.Contains(bytes, "payout") || strings.Contains(bytes, evmAddr(1)) {
		t.Fatalf("the payer view shows the split: %s", bytes)
	}

	// The USDC blacklist is read when an EVM network is first revealed (D48): the session opens,
	// the address of a network paying a blacklisted wallet is never shown, other networks are.
	app.Blacklist.List(evmAddr(1), true)
	blocked := app.Session(key, map[string]any{"product_id": prod["id"]}, idem("s8")...)
	if blocked.Status != 200 {
		t.Fatalf("create: %d %s", blocked.Status, blocked.Body)
	}
	blockedID := blocked.JSON(t)["id"].(string)
	reveal := func(id, chainID string) apptest.Response {
		return app.Do("POST", "/v1/sessions/"+id+"/reveal", key, map[string]any{"chain": chainID})
	}
	status, code, msg := errorOf(t, reveal(blockedID, "base-sepolia"))
	if status != 409 || code != "wallet_blacklisted" || !strings.Contains(msg, evmAddr(1)) {
		t.Fatalf("revealing a network paying a blacklisted wallet: %d %s %s", status, code, msg)
	}
	if r := reveal(blockedID, "solana-devnet"); r.Status != 200 || payToAddress(r.JSON(t), "base-sepolia") != "" {
		t.Fatalf("the Solana network of the session: %d %s", r.Status, r.Body)
	}
	pub := openLink(app, prodLink, "pub-2", nil)
	if pub.Status != 200 {
		t.Fatalf("public open: %d %s", pub.Status, pub.Body)
	}
	if r := app.CheckoutReveal(pub.JSON(t)["id"].(string), "base-sepolia"); r.Status != 409 || r.ErrorCode(t) != "wallet_blacklisted" {
		t.Fatalf("a payer revealing a network paying a blacklisted wallet: %d %s", r.Status, r.Body)
	}
	app.Blacklist.List(evmAddr(1), false)
	app.Blacklist.Down(true)
	r := app.Session(key, map[string]any{"product_id": prod["id"]}, idem("s9")...)
	if r.Status != 200 {
		t.Fatalf("create while the blacklist is unreachable: %d %s", r.Status, r.Body)
	}
	downID := r.JSON(t)["id"].(string)
	if status, code, _ := errorOf(t, reveal(downID, "base-sepolia")); status != 503 || code != "service_unavailable" {
		t.Fatalf("an unreachable blacklist must fail closed: %d %s", status, code)
	}
	app.Blacklist.Down(false)
	if r := reveal(downID, "base-sepolia"); r.Status != 200 || payToAddress(r.JSON(t), "base-sepolia") == "" {
		t.Fatalf("after the provider is back: %d %s", r.Status, r.Body)
	}

	// A product that pays none of the networks the account accepts has no payment method.
	if err := app.Store.SetAcceptedAssets(t.Context(), app.MerchantID, false, []store.ChainAsset{{Chain: "solana-devnet", Asset: "USDC"}}, "test"); err != nil {
		t.Fatal(err)
	}
	if status, _, msg := errorOf(t, app.Session(key, map[string]any{"product_id": evmOnly["id"]}, idem("s10")...)); status != 400 || !strings.Contains(msg, "no payment methods") {
		t.Fatalf("a product paying no accepted network: %d %s", status, msg)
	}
	if r := openLink(app, apiLink(t, app, key, evmOnly["id"].(string), nil), "pub-3", nil); r.Status != 400 {
		t.Fatalf("a public link paying no accepted network: %d %s", r.Status, r.Body)
	}
}

// One merchant's wallets are invisible and unusable for another merchant.
func TestPayoutsAcrossMerchants(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	main := walletID(t, app, app.TestKey, apptest.EVMDestination)
	_, otherKey := secondMerchant(t, app, "other@example.com")
	if r := app.Do("GET", "/v1/wallets/"+main, otherKey, nil); r.Status != 404 {
		t.Errorf("another merchant's wallet: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/products", otherKey, map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00",
		"payout": map[string]any{"base-sepolia": items(main, "100")}}); r.Status != 400 {
		t.Errorf("a product paying another merchant's wallet: %d %s", r.Status, r.Body)
	}
	mine := newProduct(t, app, app.TestKey, nil)
	if r := app.Session(otherKey, map[string]any{"product_id": mine["id"]}, idem("x")...); r.Status != 404 {
		t.Errorf("a session of another merchant's product: %d %s", r.Status, r.Body)
	}
	if body := string(app.Do("GET", "/v1/wallets?include_archived=true", otherKey, nil).Body); strings.Contains(body, main) || strings.Contains(body, apptest.EVMDestination) {
		t.Errorf("another merchant's wallets are listed: %s", body)
	}
}

// D58: Test mode wallets take no code; a Live wallet takes a confirmation code to be added or
// archived, checked after the request so a refused change never spends it. A product's payout
// and the default wallet change without one. The confirmation window (D59) is off here, so
// every Live change takes its own code; TestConfirmWindow covers the window.
func TestDashboardPayoutsStepUp(t *testing.T) {
	t.Parallel()
	off := time.Duration(0)
	app, _ := dashAppWith(t, apptest.Options{ConfirmWindow: &off}, nil)
	c := login(t, app, "owner@example.com")
	wallets := c.get("/v1/dashboard/wallets?mode=test")["data"].([]any)
	if len(wallets) != 2 {
		t.Fatalf("wallets %v", wallets)
	}
	w := c.send("POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "evm", "label": "Partner", "address": evmAddr(1)})
	main := walletID(t, app, app.TestKey, apptest.EVMDestination)
	liveBody := map[string]any{"family": "evm", "label": "Treasury cold", "address": evmAddr(4)}
	for name, extra := range map[string]map[string]any{"no code": {}, "the password": {"password": ownerPassword}, "a wrong code": {"email_code": "000000"}} {
		t.Run(name, func(t *testing.T) {
			if r := c.do("POST", "/v1/dashboard/wallets?mode=live", with(liveBody, extra)); r.Status != 403 ||
				(r.ErrorCode(t) != "confirmation_required" && r.ErrorCode(t) != "invalid_code") {
				t.Fatalf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	live := c.send("POST", "/v1/dashboard/wallets?mode=live", with(liveBody, map[string]any{"email_code": c.emailCode(service.ConfirmWalletCreate)}))

	secret := c.enable2FA()
	app.Clock.Advance(30 * 1e9)

	// A product's payout changes without a code, even with 2FA on.
	l := dashProduct(t, c, "test", "pay-1", map[string]any{"name": "Shared", "price_type": "fixed", "amount": "25.00",
		"payout": map[string]any{"base-sepolia": items(main, "70", w["id"], "30")}})
	if payoutOf(l, "base-sepolia") != main+":70.00,"+w["id"].(string)+":30.00" {
		t.Fatalf("product payout %v", l["payout"])
	}
	if r := c.do("POST", "/v1/dashboard/wallets/"+main+"/archive?mode=test", nil); r.Status != 409 ||
		r.ErrorCode(t) != "wallet_in_use" || !strings.Contains(string(r.Body), "1 product pays this wallet") {
		t.Fatalf("archiving a used wallet: %d %s", r.Status, r.Body)
	}
	c.send("PATCH", "/v1/dashboard/products/"+l["id"].(string)+"?mode=test", map[string]any{"payout": map[string]any{"base-sepolia": items(w["id"], "100")}})
	old := c.send("POST", "/v1/dashboard/wallets/"+main+"/archive?mode=test", nil)
	if old["archived_at"] == nil {
		t.Fatalf("archive %v", old)
	}

	// Live: a refused archive (here: an unknown wallet) does not spend the code.
	code := totpCode(t, app, secret)
	if r := c.do("POST", "/v1/dashboard/wallets/wal_01JBNOPE/archive?mode=live", map[string]any{"totp": code}); r.Status != 404 {
		t.Fatalf("archiving an unknown wallet: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/wallets/"+live["id"].(string)+"/archive?mode=live", nil); r.Status != 403 || r.ErrorCode(t) != "confirmation_required" {
		t.Fatalf("2FA on, no code: %d %s", r.Status, r.Body)
	}
	if a := c.send("POST", "/v1/dashboard/wallets/"+live["id"].(string)+"/archive?mode=live", map[string]any{"totp": code}); a["archived_at"] == nil {
		t.Fatalf("live archive %v", a)
	}
	st := c.get("/v1/dashboard/settings?mode=test")
	for _, gone := range []string{"destinations", "default_splits"} {
		if _, ok := st[gone]; ok {
			t.Fatalf("settings still carry %s: %v", gone, st)
		}
	}
	for _, path := range []string{"/v1/dashboard/splits?mode=test", "/v1/dashboard/splits/defaults?mode=test"} {
		if r := c.do("GET", path, nil); r.Status != 404 {
			t.Errorf("%s still answers: %d", path, r.Status)
		}
	}
	if r := c.do("PUT", "/v1/dashboard/destinations/evm?mode=test", map[string]any{"address": evmAddr(3)}); r.Status != 404 && r.Status != 405 {
		t.Errorf("PUT /destinations still answers: %d %s", r.Status, r.Body)
	}
	records, err := app.Store.AuditRecords(t.Context(), app.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	byUser := map[string]int{}
	for _, r := range records {
		if strings.HasPrefix(r.Actor, "dashboard:") {
			byUser[r.Action]++
		}
	}
	if byUser["wallet.created"] != 2 || byUser["wallet.archived"] != 2 || byUser["product.payout_changed"] != 1 || byUser["product.created"] != 1 ||
		!hasAll(auditActions(t, app, app.MerchantID), "user.step_up_failed") {
		t.Fatalf("audit (by the user %v) %v", byUser, auditActions(t, app, app.MerchantID))
	}
	// Each new Live wallet emails the owner the network, never the wallet.
	var notices []string
	for _, m := range app.Mail.To("owner@example.com") {
		if m.Kind == mail.KindWalletAdded {
			notices = append(notices, m.Text)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "Base: Live mode") || strings.Contains(notices[0], evmAddr(4)) || strings.Contains(notices[0], "Treasury cold") {
		t.Fatalf("wallet notices: %q", notices)
	}
}

// D57: the family's first wallet is its default, where new products pay; any active wallet can
// become the default without a code, in either mode, and existing products keep their payout.
func TestDefaultWallet(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	main := walletID(t, app, app.TestKey, apptest.EVMDestination)
	partner := c.send("POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "evm", "label": "Partner", "address": evmAddr(1)})["id"].(string)
	defaults := func(mode string) map[string]bool {
		out := map[string]bool{}
		for _, x := range c.get("/v1/dashboard/wallets?mode=" + mode + "&include_archived=true")["data"].([]any) {
			m := x.(map[string]any)
			out[m["id"].(string)] = m["default"].(bool)
		}
		return out
	}
	if d := defaults("test"); !d[main] || d[partner] {
		t.Fatalf("defaults: %v", d)
	}
	before := dashProduct(t, c, "test", "def-0", map[string]any{"name": "Before", "price_type": "fixed", "amount": "25.00"})
	set := c.send("POST", "/v1/dashboard/wallets/"+partner+"/default?mode=test", nil)
	if set["default"] != true {
		t.Fatalf("make default: %v", set)
	}
	if d := defaults("test"); d[main] || !d[partner] {
		t.Fatalf("defaults after: %v", d)
	}
	after := dashProduct(t, c, "test", "def-1", map[string]any{"name": "After", "price_type": "fixed", "amount": "25.00"})
	if payoutOf(after, "base-sepolia") != partner+":100.00" {
		t.Fatalf("a new product pays the default: %v", after["payout"])
	}
	if got := c.get("/v1/dashboard/products/" + before["id"].(string) + "?mode=test"); payoutOf(got, "base-sepolia") != main+":100.00" {
		t.Fatalf("an existing product changed: %v", got["payout"])
	}
	// The public API reads the flag too.
	for _, x := range app.Do("GET", "/v1/wallets", app.TestKey, nil).JSON(t)["data"].([]any) {
		if m := x.(map[string]any); m["default"] != (m["id"] == partner || m["family"] == "solana") {
			t.Fatalf("API wallet: %v", m)
		}
	}
	if r := c.do("POST", "/v1/dashboard/wallets/"+partner+"/default?mode=live", nil); r.Status != 404 {
		t.Fatalf("a wallet of the other mode: %d %s", r.Status, r.Body)
	}
	c.send("PATCH", "/v1/dashboard/products/"+after["id"].(string)+"?mode=test", map[string]any{"payout": map[string]any{"base-sepolia": items(main, "100")}})
	c.send("POST", "/v1/dashboard/wallets/"+partner+"/archive?mode=test", nil)
	if d := defaults("test"); !d[main] || d[partner] {
		t.Fatalf("archiving the default hands it on: %v", d)
	}
	if r := c.do("POST", "/v1/dashboard/wallets/"+partner+"/default?mode=test", nil); r.Status != 400 {
		t.Fatalf("an archived default: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "wallet.default_set") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

// The sandbox settles a paid test session as the flusher would: fee and payouts by the rule,
// no event, the session's version grows.
func TestSimulatorSettle(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.TestKey
	main := walletID(t, app, key, apptest.EVMDestination)
	partner := newWallet(t, app, key, "Partner", evmAddr(1))
	prod := newProduct(t, app, key, map[string]any{"base-sepolia": items(main, "70", partner, "30")})
	s := app.Session(key, map[string]any{"product_id": prod["id"]}, idem("sim")...).JSON(t)
	id := s["id"].(string)
	sim := func(action string) apptest.Response {
		return app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", key, map[string]any{"action": action, "chain": "base-sepolia"})
	}
	if r := sim("settle"); r.Status != 409 || r.ErrorCode(t) != "invalid_state" {
		t.Fatalf("settling an open session: %d %s", r.Status, r.Body)
	}
	paid := sim("pay_exact").JSON(t)
	r := sim("settle")
	if r.Status != 200 {
		t.Fatalf("settle: %d %s", r.Status, r.Body)
	}
	res := r.JSON(t)
	ses := res["session"].(map[string]any)
	st := ses["settlement"].(map[string]any)
	payouts := st["payouts"].([]any)
	if len(res["events"].([]any)) != 0 || ses["version"].(float64) <= paid["session"].(map[string]any)["version"].(float64) ||
		st["gross"] != "25.000000" || st["fee"] != "0.225000" || st["net"] != "24.775000" || st["simulated"] != true ||
		!strings.HasPrefix(st["tx_hash"].(string), "0x0000000000000000") || len(payouts) != 2 ||
		payouts[0].(map[string]any)["amount"] != "17.342500" || payouts[0].(map[string]any)["wallet_id"] != main ||
		payouts[1].(map[string]any)["amount"] != "7.432500" || payouts[1].(map[string]any)["label"] != "Partner" {
		t.Fatalf("settled %v", res)
	}
	if ses["payments"].([]any)[0].(map[string]any)["status"] != "finalized" {
		t.Fatalf("settled payments are final: %v", ses["payments"])
	}
	if r := sim("settle"); r.Status != 409 {
		t.Fatalf("settling twice: %d %s", r.Status, r.Body)
	}
	if r := sim("reverse_last_payment"); r.Status != 409 {
		t.Fatalf("reversing a settled payment: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.LiveKey, map[string]any{"action": "settle"}); r.Status != 403 {
		t.Fatalf("live mode: %d", r.Status)
	}
	got := app.Do("GET", "/v1/sessions/"+id, key, nil).JSON(t)
	if fmt.Sprint(got["settlement"]) != fmt.Sprint(st) {
		t.Fatalf("GET differs from the simulator answer:\n%v\n%v", got["settlement"], st)
	}
	o := app.Do("GET", "/v1/sessions/"+id, key, nil)
	if !strings.Contains(string(o.Body), `"payouts":[{"wallet_id":"`+main+`","label":"Main wallet","address":"`+apptest.EVMDestination+`","amount":"17.342500"}`) {
		t.Fatalf("settlement shape %s", o.Body)
	}
}

// The split rule the simulator used is the one the contracts and the program use.
func TestSimulatorSettleFollowsTheVectors(t *testing.T) {
	t.Parallel()
	rule, err := payout.Compute(money.MustParseInteger("25000000"), 6, chain.PilotFee(), []uint16{7000, 3000})
	if err != nil || rule.Shares[0].String() != "17342500" || rule.Shares[1].String() != "7432500" {
		t.Fatalf("rule %+v %v", rule, err)
	}
}

// Only the networks a session can be paid on are resolved and checked: a blacklisted EVM wallet
// does not stop a merchant that accepts Solana only.
func TestBlacklistOnlyMattersForNetworksPaid(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.TestKey
	prod := newProduct(t, app, key, nil)
	if err := app.Store.SetAcceptedAssets(t.Context(), app.MerchantID, false, []store.ChainAsset{{Chain: "solana-devnet", Asset: "USDC"}}, "test"); err != nil {
		t.Fatal(err)
	}
	app.Blacklist.List(apptest.EVMDestination, true)
	r := app.RevealedSession(key, map[string]any{"product_id": prod["id"]}, idem("sol-only")...)
	if r.Status != 200 || payoutOf(r.JSON(t), "base-sepolia") != "-" || payToAddress(r.JSON(t), "solana-devnet") == "" {
		t.Fatalf("a Solana-only session: %d %s", r.Status, r.Body)
	}
	if err := app.Store.SetAcceptedAssets(t.Context(), app.MerchantID, false, []store.ChainAsset{{Chain: "solana-devnet", Asset: "USDC"},
		{Chain: "base-sepolia", Asset: "USDC"}}, "test"); err != nil {
		t.Fatal(err)
	}
	both := app.Session(key, map[string]any{"product_id": prod["id"]}, idem("both")...)
	if both.Status != 200 {
		t.Fatalf("create: %d %s", both.Status, both.Body)
	}
	id := both.JSON(t)["id"].(string)
	if r := app.Do("POST", "/v1/sessions/"+id+"/reveal", key, map[string]any{"chain": "base-sepolia"}); r.Status != 409 || r.ErrorCode(t) != "wallet_blacklisted" {
		t.Fatalf("revealing an EVM network paying a blacklisted wallet: %d %s", r.Status, r.Body)
	}
	app.Reveal(key, id, "solana-devnet")
}

// Where the money goes is the product's; every link of the product pays it.
func TestDashboardProductPayout(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	main := walletID(t, app, app.TestKey, apptest.EVMDestination)
	sol := walletID(t, app, app.TestKey, apptest.SolanaDestination)
	partner := c.send("POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "evm", "label": "Partner", "address": evmAddr(1)})["id"]
	def := dashProduct(t, c, "test", "payout-0", map[string]any{"name": "Default", "price_type": "fixed", "amount": "25.00"})
	if payoutOf(def, "base-sepolia") != main+":100.00" || payoutOf(def, "solana-devnet") != sol+":100.00" {
		t.Fatalf("a new product pays the oldest wallets: %v", def["payout"])
	}
	p := dashProduct(t, c, "test", "payout-1", map[string]any{"name": "Shared", "price_type": "fixed", "amount": "25.00",
		"payout": map[string]any{"base-sepolia": items(main, "70", partner, "30"), "solana-devnet": []any{}}})
	if payoutOf(p, "base-sepolia") != main+":70.00,"+partner.(string)+":30.00" || payoutOf(p, "solana-devnet") != "[]" {
		t.Fatalf("product payout %v", p["payout"])
	}
	listed := c.get("/v1/dashboard/products?mode=test")["data"].([]any)
	for _, x := range listed {
		if m := x.(map[string]any); m["id"] == p["id"] && fmt.Sprint(m["payout"]) != fmt.Sprint(p["payout"]) {
			t.Fatalf("the list shows another payout: %v", m["payout"])
		}
	}
	// Two links of the product snapshot the same payout; a link carries none of its own.
	for i, key := range []string{"link-1", "link-2"} {
		l := dashLink(t, c, "test", key, map[string]any{"product_id": p["id"]})
		if _, ok := l["payout"]; ok {
			t.Fatalf("a link has a payout: %v", l)
		}
		ses := openLink(app, l["id"].(string), fmt.Sprintf("open-%d", i), nil)
		if ses.Status != 200 {
			t.Fatalf("open: %d %s", ses.Status, ses.Body)
		}
		stored, _ := app.Store.LoadSession(t.Context(), ses.JSON(t)["id"].(string))
		if rs := stored.Recipients("base-sepolia"); len(rs) != 2 || rs[1].Address != evmAddr(1) || len(stored.Recipients("solana-devnet")) != 0 {
			t.Fatalf("link session snapshot %v", rs)
		}
	}
	if r := c.do("PATCH", "/v1/dashboard/links/"+c.get("/v1/dashboard/products/" + p["id"].(string) + "/links?mode=test")["data"].([]any)[0].(map[string]any)["id"].(string)+"?mode=test",
		map[string]any{"payout": map[string]any{"solana-devnet": items(sol, "100")}}); r.Status != 400 {
		t.Fatalf("a payout on a link: %d %s", r.Status, r.Body)
	}
	up := c.send("PATCH", "/v1/dashboard/products/"+p["id"].(string)+"?mode=test", map[string]any{"payout": map[string]any{"solana-devnet": items(sol, "100")},
		"links": "keep"})
	if payoutOf(up, "solana-devnet") != sol+":100.00" || payoutOf(up, "base-sepolia") != main+":70.00,"+partner.(string)+":30.00" {
		t.Fatalf("patched product payout %v", up["payout"])
	}
	for name, payout := range map[string]map[string]any{
		"an EVM wallet on Solana": {"solana-devnet": items(main, "100")},
		"a live network":          {"solana": items(sol, "100")},
	} {
		t.Run(name, func(t *testing.T) {
			if r := c.do("PATCH", "/v1/dashboard/products/"+p["id"].(string)+"?mode=test", map[string]any{"payout": payout}); r.Status != 400 {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "product.created", "product.updated", "product.payout_changed", "payment_link.created") {
		t.Fatalf("audit %v", auditActions(t, app, app.MerchantID))
	}
}
