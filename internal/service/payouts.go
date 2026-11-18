package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// Limits and error codes of the wallet endpoints.
const (
	// MaxActiveWallets is how many wallets of one network family may be active at once.
	MaxActiveWallets = 20
	maxLabelLen      = 100

	CodeWalletInUse       = "wallet_in_use"
	CodeWalletBlacklisted = "wallet_blacklisted"
	CodeUnknownChain      = "unknown_chain"
)

// WalletView is a payout wallet as the API returns it.
type WalletView struct {
	ID             string  `json:"id"`
	Livemode       bool    `json:"livemode"`
	Family         string  `json:"family"`
	Label          string  `json:"label"`
	Address        string  `json:"address"`
	Verified       bool    `json:"verified"`
	UsedByProducts int     `json:"used_by_products"`
	Default        bool    `json:"default"`
	ArchivedAt     *string `json:"archived_at"`
	CreatedAt      string  `json:"created_at"`
}

// WalletList is the list of wallets the API returns.
type WalletList struct {
	Data []WalletView `json:"data"`
}

func walletView(w store.Wallet) WalletView {
	return WalletView{ID: w.ID, Livemode: w.Livemode, Family: string(w.Family), Label: w.Label, Address: w.Address,
		Verified: w.Verified, UsedByProducts: w.UsedByProducts, Default: w.Default, ArchivedAt: optRFC3339(w.ArchivedAt),
		CreatedAt: rfc3339(w.CreatedAt)}
}

func parseFamily(field, raw string) (chain.Family, error) {
	f := chain.Family(raw)
	if !f.Valid() {
		return "", invalid("%s must be evm or solana", field)
	}
	return f, nil
}

func familyName(f chain.Family) string {
	if f == chain.Solana {
		return "Solana"
	}
	return "EVM"
}

func walletNotFound() *Error {
	return newErr(KindNotFound, CodeNotFound, "no such wallet")
}

func walletInUse(n int) *Error {
	who := "1 product pays"
	if n != 1 {
		who = fmt.Sprintf("%d products pay", n)
	}
	return newErr(KindConflict, CodeWalletInUse, "%s this wallet. Change where those products pay before archiving it.", who)
}

// blacklistError explains an issuer-blacklisted or unverifiable payout address.
func blacklistError(err error, kind Kind, label, address string) error {
	switch {
	case errors.Is(err, destination.ErrBlacklisted):
		e := newErr(kind, CodeWalletBlacklisted,
			"The USDC issuer has blacklisted the wallet %s (%s), so payments to it can never settle. Use another wallet.", label, address)
		e.Cause = err
		return e
	case errors.Is(err, destination.ErrUnverifiable):
		e := newErr(KindUnavailable, CodeServiceUnavailable,
			"The USDC blacklist could not be checked right now. Try again in a few minutes.")
		e.Cause = err
		return e
	}
	return err
}

// WalletInput is a new wallet. An empty Label defaults to "Wallet" and the address's last six
// characters.
type WalletInput struct {
	Family  string
	Label   string
	Address string
}

func cleanLabel(field, label, fallback string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = fallback
	}
	if n := utf8.RuneCountInString(label); n == 0 || n > maxLabelLen {
		return "", invalid("%s must have 1 to %d characters", field, maxLabelLen)
	}
	return label, nil
}

func lastSix(addr string) string {
	if len(addr) <= 6 {
		return addr
	}
	return addr[len(addr)-6:]
}

// checkWallet validates a new wallet's address with the same rules a destination always had
// (format, checksum, zero, token and protocol denylist, contract code on every enabled EVM,
// Solana wallet with token accounts for every enabled mint) plus the USDC issuer blacklist. It
// returns the canonical address and whether the on-chain checks ran.
//
// Live mode fails closed unless allowUnverified is set. In test mode a check
// that cannot confirm the address (no or failing providers, or a Solana wallet without a token
// account yet) does not refuse it: the wallet is stored with verified=false, since test tokens
// have no value and a test payout to it simply waits. A definite refusal (format, checksum,
// denylist, token account or mint, blacklist) applies in both modes.
// familyServed reports whether the registry has a chain of the family in either mode: a family with
// no chain left here (PRISMAPORT_CHAINS_OFF, D62) takes no wallet.
func (s *Service) familyServed(fam chain.Family) bool {
	return slices.ContainsFunc(append(s.cfg.Registry.ForMode(true), s.cfg.Registry.ForMode(false)...), func(c chain.Chain) bool { return c.Family == fam })
}

func (s *Service) checkWallet(ctx context.Context, livemode bool, in WalletInput, allowUnverified bool) (store.Wallet, error) {
	fam, err := parseFamily("family", in.Family)
	if err != nil {
		return store.Wallet{}, err
	}
	if !s.familyServed(fam) {
		return store.Wallet{}, at(invalid("%s is not available here yet.", familyName(fam)), "family")
	}
	lenient := allowUnverified || !livemode
	canonical, err := s.cfg.Validator.Validate(ctx, fam, in.Address, destination.Options{Livemode: livemode})
	verified := err == nil
	if err != nil && lenient && (errors.Is(err, destination.ErrUnverifiable) || (!livemode && errors.Is(err, destination.ErrTokenAccountMissing))) {
		canonical, err = s.cfg.Validator.Validate(ctx, fam, in.Address, destination.Options{Livemode: livemode, AllowUnverified: true,
			AllowMissingTokenAccounts: !livemode})
	}
	if err != nil {
		return store.Wallet{}, destinationError(err)
	}
	label, err := cleanLabel("label", in.Label, "Wallet "+lastSix(canonical))
	if err != nil {
		return store.Wallet{}, err
	}
	if err := s.cfg.Validator.CheckBlacklist(ctx, livemode, fam, canonical); err != nil {
		if !(lenient && errors.Is(err, destination.ErrUnverifiable)) {
			return store.Wallet{}, blacklistError(err, KindInvalid, label, canonical)
		}
		verified = false
	}
	return store.Wallet{Livemode: livemode, Family: fam, Label: label, Address: canonical, Verified: verified}, nil
}

func (s *Service) createWallet(ctx context.Context, merchantID string, w store.Wallet, actor string) (store.Wallet, error) {
	w.MerchantID = merchantID
	created, err := s.store.CreateWallet(ctx, w, MaxActiveWallets, actor)
	switch {
	case errors.Is(err, store.ErrWalletExists):
		return created, invalid("An active %s wallet already has the address %s.", familyName(w.Family), w.Address)
	case errors.Is(err, store.ErrWalletLimit):
		return created, invalid("A network family holds at most %d active wallets. Archive one first.", MaxActiveWallets)
	}
	return created, err
}

// ListWallets returns the merchant's wallets, optionally for one network family and including
// archived ones; it needs the wallets read scope.
func (s *Service) ListWallets(ctx context.Context, a Auth, family string, includeArchived bool) (WalletList, error) {
	if err := a.Require(apikey.WalletsRead); err != nil {
		return WalletList{}, err
	}
	var fam chain.Family
	if family != "" {
		f, err := parseFamily("family", family)
		if err != nil {
			return WalletList{}, err
		}
		fam = f
	}
	list, err := s.store.ListWallets(ctx, a.MerchantID, a.Livemode, store.WalletFilter{Family: fam, IncludeArchived: includeArchived})
	if err != nil {
		return WalletList{}, err
	}
	out := WalletList{Data: []WalletView{}}
	for _, w := range list {
		out.Data = append(out.Data, walletView(w))
	}
	return out, nil
}

// GetWallet returns one of the merchant's wallets, or a not-found error; it needs the wallets read
// scope.
func (s *Service) GetWallet(ctx context.Context, a Auth, id string) (WalletView, error) {
	if err := a.Require(apikey.WalletsRead); err != nil {
		return WalletView{}, err
	}
	w, err := s.store.GetWallet(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return WalletView{}, walletNotFound()
	}
	if err != nil {
		return WalletView{}, err
	}
	return walletView(w), nil
}

// archiveWallet retires a wallet no product pays. Open sessions keep paying it.
func (s *Service) archiveWallet(ctx context.Context, a Auth, id string) (WalletView, error) {
	w, err := s.store.ArchiveWallet(ctx, a.MerchantID, a.Livemode, id, a.actor())
	var inUse *store.WalletInUseError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return WalletView{}, walletNotFound()
	case errors.As(err, &inUse):
		return WalletView{}, walletInUse(inUse.Products)
	case err != nil:
		return WalletView{}, err
	}
	return walletView(w), nil
}

// PayoutItemInput is one recipient of a product payout request: a wallet and its share as a
// percentage.
type PayoutItemInput struct {
	WalletID string
	Share    string
}

// payoutItems checks the shape of one network's recipient list: none (the product does not take
// that network), or 1 to 5 distinct wallets, every share above zero with at most two decimals,
// adding up to exactly 100%.
func payoutItems(c chain.Chain, in []PayoutItemInput) ([]store.PayoutItem, error) {
	if len(in) == 0 {
		return nil, nil
	}
	field := "payout." + c.ID
	if len(in) > payout.MaxRecipients {
		return nil, at(invalid("A split pays 1 to %d wallets.", payout.MaxRecipients), field)
	}
	out := make([]store.PayoutItem, len(in))
	sum := 0
	for i, it := range in {
		if it.WalletID == "" {
			return nil, at(invalid("payout.%s[%d].wallet_id is required", c.ID, i), fmt.Sprintf("%s[%d].wallet_id", field, i))
		}
		for _, prev := range out[:i] {
			if prev.WalletID == it.WalletID {
				return nil, at(invalid("Each wallet can appear only once in a split."), fmt.Sprintf("%s[%d].wallet_id", field, i))
			}
		}
		bps, err := payout.ParseShare(it.Share)
		if err != nil {
			return nil, at(invalid("payout.%s[%d].share must be a percentage above 0 and at most 100 with up to two decimals, such as %q", c.ID, i, "70.00"),
				fmt.Sprintf("%s[%d].share", field, i))
		}
		out[i] = store.PayoutItem{WalletID: it.WalletID, Bps: bps, Family: c.Family}
		sum += int(bps)
	}
	if sum != payout.BpsTotal {
		return nil, at(invalid("Shares must add up to 100%%. Now %s%%.", payout.FormatShareSum(sum)), field)
	}
	return out, nil
}

// checkPayoutWallets fills in each item's wallet and refuses one that is not an active wallet of
// the item's family, or that the USDC issuer blacklisted.
func (s *Service) checkPayoutWallets(ctx context.Context, a Auth, chainID string, items []store.PayoutItem) error {
	for i := range items {
		fam := items[i].Family
		param := fmt.Sprintf("payout.%s[%d].wallet_id", chainID, i)
		w, err := s.store.GetWallet(ctx, a.MerchantID, a.Livemode, items[i].WalletID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (w.ArchivedAt != nil || w.Family != fam)) {
			return at(invalid("Wallet %s is not an active %s wallet of this account.", items[i].WalletID, familyName(fam)), param)
		}
		if err != nil {
			return err
		}
		items[i].Label, items[i].Address = w.Label, w.Address
		if err := s.cfg.Validator.CheckBlacklist(ctx, a.Livemode, fam, w.Address); err != nil {
			return at(blacklistError(err, KindInvalid, w.Label, w.Address), param)
		}
		if err := s.checkFrozen(ctx, a.Livemode, fam, w); err != nil {
			return at(err, param)
		}
	}
	return nil
}

// checkFrozen refuses binding a Solana wallet whose token account for an accepted mint the issuer
// froze: the forwarder pays every recipient in one transaction, so one frozen recipient blocks the
// whole split's flushes of that mint (ADR 0008 I-2). The wallet passed the full checks when it was
// added; this re-check is advisory, so a lookup that cannot run is logged and the split is saved.
func (s *Service) checkFrozen(ctx context.Context, livemode bool, fam chain.Family, w store.Wallet) error {
	ctx, cancel := context.WithTimeout(ctx, blacklistCheckTimeout)
	defer cancel()
	err := s.cfg.Validator.CheckFrozen(ctx, livemode, fam, w.Address)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, destination.ErrTokenAccountFrozen):
		return frozenError(err, fmt.Sprintf("the wallet %s (%s)", w.Label, w.Address))
	case errors.Is(err, destination.ErrUnverifiable):
		// Test mode often runs without Solana providers; only a live miss is worth a warning.
		log := s.Logger().Debug
		if livemode {
			log = s.Logger().Warn
		}
		log("frozen token account check unavailable; payout saved unchecked", "wallet", w.ID, "err", err)
		return nil
	}
	return err
}

// frozenError explains a Solana wallet with a token account the issuer froze; who names it.
func frozenError(err error, who string) *Error {
	assets := "an accepted asset"
	var frozen *destination.FrozenTokenAccountsError
	if errors.As(err, &frozen) {
		assets = frozen.Assets()
	}
	e := newErr(KindInvalid, CodeWalletBlacklisted,
		"The token issuer froze the %s token account of %s, so payments to it can never settle. Use another wallet.", assets, who)
	e.Cause = err
	return e
}

// NetworkInput is a product's stablecoin choice on one network in a create or patch request
// (D60): Accept is all, some or none; Assets (AssetsSet when the request has a non-null list) only
// goes with some.
type NetworkInput struct {
	Accept    string
	Assets    []string
	AssetsSet bool
}

// networkChoices validates a request's networks (keys already checked against the mode): accept
// is all, some or none; some lists 1 or more distinct stablecoins the platform offers on that
// chain in this mode (OfferedAssets; one the merchant has off in Settings is allowed and stays
// stored), stored in the registry's order; all and none take no list.
func (s *Service) networkChoices(livemode bool, in map[string]NetworkInput) (map[string]store.NetworkChoice, error) {
	offered := map[string][]string{}
	for _, o := range s.OfferedAssets(livemode) {
		offered[o.Chain] = append(offered[o.Chain], o.Asset)
	}
	out := map[string]store.NetworkChoice{}
	for _, id := range slices.Sorted(maps.Keys(in)) {
		n, field := in[id], "networks."+id
		switch store.Accept(n.Accept) {
		case store.AcceptAll, store.AcceptNone:
			if n.AssetsSet {
				return nil, at(invalid("%s.assets goes only with accept some", field), field+".assets")
			}
			out[id] = store.NetworkChoice{Accept: store.Accept(n.Accept)}
		case store.AcceptSome:
			if len(n.Assets) == 0 {
				return nil, at(invalid("%s.assets must list at least one stablecoin with accept some", field), field+".assets")
			}
			want := map[string]bool{}
			for _, x := range n.Assets {
				if want[x] {
					return nil, at(invalid("%s.assets lists %s twice", field, x), field+".assets")
				}
				if !slices.Contains(offered[id], x) {
					return nil, at(invalid("%s.assets: %s is not offered on %s in %s mode; use %s", field, x, id, modeName(livemode),
						strings.Join(offered[id], ", ")), field+".assets")
				}
				want[x] = true
			}
			var assets []string
			for _, x := range offered[id] {
				if want[x] {
					assets = append(assets, x)
				}
			}
			out[id] = store.NetworkChoice{Accept: store.AcceptSome, Assets: assets}
		default:
			return nil, at(invalid("%s.accept must be all, some or none", field), field+".accept")
		}
	}
	return out, nil
}

// applyPayout sets a product's payout and networks (D60) from a create or patch request, both
// keyed by the chain ids of the product's mode (another key is 400 unknown_chain). Per chain:
//   - networks accept none, or an explicit empty payout list: the product does not take the
//     network (no payout, no choice). A non-empty payout with accept none, or [] with all or
//     some, contradict each other: 400.
//   - a payout list: those items; the choice is the request's, else the stored one, else all.
//   - accept all or some without a payout: the new choice; a network the product did not take
//     yet pays the default wallet of its family at 100%, and with no wallet the request is 400.
//   - neither: on create the default wallet of its family at 100% and all (stored like any
//     explicit choice, so adding wallets later changes nothing), or nothing without a wallet; on
//     patch the network keeps its payout and choice.
//
// The result must pay at least one network.
func (s *Service) applyPayout(ctx context.Context, a Auth, p *store.Product, in map[string][]PayoutItemInput, networks map[string]NetworkInput, creating bool) error {
	if err := s.checkKnownChains(a, in, networks); err != nil {
		return err
	}
	choices, err := s.networkChoices(a.Livemode, networks)
	if err != nil {
		return err
	}
	plan := payoutPlan{s: s, ctx: ctx, a: a, next: store.ClonePayout(p.Payout), nextNet: store.CloneNetworks(p.Networks),
		defaults: map[chain.Family]*store.Wallet{}}
	for _, c := range s.cfg.Registry.ForMode(a.Livemode) {
		req, hasPayout := in[c.ID]
		choice, hasChoice := choices[c.ID]
		var err error
		switch {
		case hasChoice && choice.Accept == store.AcceptNone:
			err = plan.refuse(c, req)
		case hasPayout && len(req) == 0:
			err = plan.drop(c, choice, hasChoice)
		case hasPayout:
			err = plan.pay(c, req, choice, hasChoice)
		case hasChoice:
			err = plan.choose(c, choice)
		case creating:
			err = plan.payDefault(c)
		}
		if err != nil {
			return err
		}
	}
	return plan.finish(p)
}

// checkKnownChains refuses a payout or networks key that is not a network of the mode, naming
// the first one in sorted order.
func (s *Service) checkKnownChains(a Auth, in map[string][]PayoutItemInput, networks map[string]NetworkInput) error {
	known := map[string]bool{}
	for _, c := range s.cfg.Registry.ForMode(a.Livemode) {
		known[c.ID] = true
	}
	for _, f := range []struct {
		field string
		keys  []string
	}{{"payout", slices.Collect(maps.Keys(in))}, {"networks", slices.Collect(maps.Keys(networks))}} {
		slices.Sort(f.keys)
		for _, id := range f.keys {
			if !known[id] {
				return at(newErr(KindInvalid, CodeUnknownChain, "%s has an unknown network %q: use one of %s", f.field, id,
					strings.Join(s.chainIDs(a.Livemode), ", ")), f.field+"."+id)
			}
		}
	}
	return nil
}

// payoutPlan is the payout and network choices applyPayout builds, network by network, from the
// product's current ones.
type payoutPlan struct {
	s       *Service
	ctx     context.Context
	a       Auth
	next    map[string][]store.PayoutItem
	nextNet map[string]store.NetworkChoice
	// defaults caches each family's default wallet (nil: the family has none).
	defaults map[chain.Family]*store.Wallet
}

// defaultItems is c's family's default wallet at 100%, checked like any payout; nil when the
// family has no wallet.
func (pl *payoutPlan) defaultItems(c chain.Chain) ([]store.PayoutItem, error) {
	w, seen := pl.defaults[c.Family]
	if !seen {
		found, err := pl.s.store.DefaultWallet(pl.ctx, pl.a.MerchantID, pl.a.Livemode, c.Family)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			w = &found
		}
		pl.defaults[c.Family] = w
	}
	if w == nil {
		return nil, nil
	}
	items := []store.PayoutItem{{WalletID: w.ID, Bps: payout.BpsTotal, Family: c.Family}}
	if err := pl.s.checkPayoutWallets(pl.ctx, pl.a, c.ID, items); err != nil {
		return nil, err
	}
	return items, nil
}

// refuse applies accept none: the product stops taking c, and a payout naming wallets
// contradicts it.
func (pl *payoutPlan) refuse(c chain.Chain, req []PayoutItemInput) error {
	if len(req) > 0 {
		return at(invalid("payout.%s names wallets but networks.%s.accept is none: send [] or leave payout.%s out", c.ID, c.ID, c.ID),
			"payout."+c.ID)
	}
	delete(pl.next, c.ID)
	return nil
}

// drop applies an explicit empty payout: the product stops taking c, which a choice of all or
// some contradicts.
func (pl *payoutPlan) drop(c chain.Chain, choice store.NetworkChoice, hasChoice bool) error {
	if hasChoice {
		return at(invalid("networks.%s.accept is %s but payout.%s is []: a network the product takes needs a payout", c.ID,
			choice.Accept, c.ID), "networks."+c.ID+".accept")
	}
	delete(pl.next, c.ID)
	return nil
}

// pay applies a payout list, and the request's choice when it has one.
func (pl *payoutPlan) pay(c chain.Chain, req []PayoutItemInput, choice store.NetworkChoice, hasChoice bool) error {
	items, err := payoutItems(c, req)
	if err != nil {
		return err
	}
	if err := pl.s.checkPayoutWallets(pl.ctx, pl.a, c.ID, items); err != nil {
		return err
	}
	pl.next[c.ID] = items
	if hasChoice {
		pl.nextNet[c.ID] = choice
	}
	return nil
}

// choose applies a choice of all or some without a payout: a network the product did not take
// yet pays its family's default wallet, and without one the request is refused.
func (pl *payoutPlan) choose(c chain.Chain, choice store.NetworkChoice) error {
	if len(pl.next[c.ID]) == 0 {
		items, err := pl.defaultItems(c)
		if err != nil {
			return err
		}
		if items == nil {
			return at(invalid("Add a %s wallet before taking payments on %s, or send payout.%s.", familyName(c.Family), view.ChainName(c.ID),
				c.ID), "networks."+c.ID)
		}
		pl.next[c.ID] = items
	}
	pl.nextNet[c.ID] = choice
	return nil
}

// payDefault is a create's network the request says nothing about: its family's default wallet,
// or nothing without one.
func (pl *payoutPlan) payDefault(c chain.Chain) error {
	items, err := pl.defaultItems(c)
	if err != nil {
		return err
	}
	if items == nil {
		delete(pl.next, c.ID)
		return nil
	}
	pl.next[c.ID] = items
	return nil
}

// finish checks that the plan pays at least one network, keys the choices like the payout (a
// network taken without a choice takes all) and sets both on p.
func (pl *payoutPlan) finish(p *store.Product) error {
	for id, items := range pl.next {
		if len(items) == 0 {
			delete(pl.next, id)
		}
	}
	if len(pl.next) == 0 {
		return at(invalid("A product must pay at least one network. Add a wallet first, or give one network of payout a wallet."), "payout")
	}
	for id := range pl.nextNet {
		if _, ok := pl.next[id]; !ok {
			delete(pl.nextNet, id)
		}
	}
	for id := range pl.next {
		if _, ok := pl.nextNet[id]; !ok {
			pl.nextNet[id] = store.NetworkChoice{Accept: store.AcceptAll}
		}
	}
	p.Payout, p.Networks = pl.next, pl.nextNet
	return nil
}

func storePayoutError(err error) error {
	if errors.Is(err, store.ErrWalletUnavailable) {
		return invalid("A wallet of this payout is no longer an active wallet of this account.")
	}
	return err
}

// Wallet writes exist only in the dashboard: each validates the whole request first, then, in
// Live mode, asks for a confirmation code (emailed, or the TOTP code with 2FA on; D58) and writes
// in the transaction that spends it, so a refused change (a duplicate address, the wallet limit)
// never spends the user's code. The audit entry names the dashboard user. Test mode asks for no
// code.

// DashCreateWallet adds a payout wallet. It changes nothing any product pays: a new product pays
// the family's default wallet unless told otherwise (the family's first wallet becomes it), and
// existing products keep their payout. A new Live wallet is emailed to the owner.
func (s *Service) DashCreateWallet(ctx context.Context, du DashUser, livemode bool, in WalletInput, proof StepUpProof) (WalletView, error) {
	if err := s.recoveryLock(du, livemode); err != nil {
		return WalletView{}, err
	}
	w, err := s.checkWallet(ctx, livemode, in, false)
	if err != nil {
		return WalletView{}, err
	}
	var created store.Wallet
	if err := s.windowConfirm(ctx, du, livemode, ConfirmWalletCreate, proof, func(b *Service) error {
		created, err = b.createWallet(ctx, du.MerchantID, w, du.actor())
		return err
	}); err != nil {
		return WalletView{}, err
	}
	if livemode {
		s.notifyWalletAdded(ctx, du, created.Family)
	}
	return walletView(created), nil
}

// notifyWalletAdded tells the owner a wallet that receives Live payments was added: the networks
// of its family with the stablecoins the merchant accepts there, never the address or the name.
func (s *Service) notifyWalletAdded(ctx context.Context, du DashUser, fam chain.Family) {
	accepted, err := s.store.AcceptedAssets(ctx, du.MerchantID, true)
	if err != nil {
		s.Logger().Error("wallet notice: accepted assets", "err", err)
	}
	var nets []mail.WalletNetwork
	for _, c := range s.cfg.Registry.ForMode(true) {
		if c.Family != fam {
			continue
		}
		n := mail.WalletNetwork{Chain: c.ID, Name: view.ChainName(c.ID)}
		for _, x := range accepted {
			if x.Chain == c.ID {
				n.Assets = append(n.Assets, x.Asset)
			}
		}
		nets = append(nets, n)
	}
	// Only the networks the merchant accepts payments on, when any; otherwise every network of
	// the family, so the notice always names one.
	var accepting []mail.WalletNetwork
	for _, n := range nets {
		if len(n.Assets) > 0 {
			accepting = append(accepting, n)
		}
	}
	if len(accepting) > 0 {
		nets = accepting
	}
	if len(nets) == 0 {
		nets = []mail.WalletNetwork{{Name: familyName(fam)}}
	}
	s.notify(ctx, mail.KindWalletAdded, func() (mail.Message, error) {
		return mail.WalletAdded(du.Email, mail.WalletAddedEmail{NoticeEmail: s.notice(du.Email), Networks: nets})
	})
}

// DashArchiveWallet archives a wallet no product pays; in Live mode after a confirmation code.
func (s *Service) DashArchiveWallet(ctx context.Context, du DashUser, livemode bool, id string, proof StepUpProof) (WalletView, error) {
	a := du.Auth(livemode)
	w, err := s.store.GetWallet(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return WalletView{}, walletNotFound()
	}
	if err != nil {
		return WalletView{}, err
	}
	if w.ArchivedAt == nil && w.UsedByProducts > 0 {
		return WalletView{}, walletInUse(w.UsedByProducts)
	}
	if w.ArchivedAt != nil {
		return s.archiveWallet(ctx, a, id)
	}
	var out WalletView
	err = s.windowConfirm(ctx, du, livemode, ConfirmWalletArchive, proof, func(b *Service) error {
		out, err = b.archiveWallet(ctx, a, id)
		return err
	})
	return out, err
}

// DashSetDefaultWallet makes an active wallet the default of its family: products created
// afterwards pay it unless told otherwise. It moves no money of existing products, so it takes
// no confirmation (D57).
func (s *Service) DashSetDefaultWallet(ctx context.Context, du DashUser, livemode bool, id string) (WalletView, error) {
	w, err := s.store.SetDefaultWallet(ctx, du.MerchantID, livemode, id, du.actor())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return WalletView{}, walletNotFound()
	case errors.Is(err, store.ErrWalletUnavailable):
		return WalletView{}, invalid("An archived wallet cannot be the default. Add the address again as a new wallet.")
	case err != nil:
		return WalletView{}, err
	}
	return walletView(w), nil
}

// resolvePayouts reads, per chain the session can be paid on (needed), the recipients the
// product pays there. A chain the product does not pay gets no pay_to entries. The USDC blacklist
// of the EVM recipients is checked when a network is first revealed (checkRevealBlacklist, D48),
// not here: a session nobody pays costs no RPC.
func (s *Service) resolvePayouts(product *store.Product, needed map[string]chain.Family) map[string][]store.PayoutItem {
	out := map[string][]store.PayoutItem{}
	for _, id := range store.PayoutChains(product.Payout) {
		_, ok := needed[id]
		items := product.Payout[id]
		if !ok || len(items) == 0 {
			continue
		}
		out[id] = items
	}
	return out
}

// blacklistCheckTimeout bounds the blacklist lookup of one payout address when a network is
// revealed: a slow provider answers "could not be checked, try again" rather than holding the
// request.
const blacklistCheckTimeout = 3 * time.Second

func (s *Service) checkPayoutBlacklist(ctx context.Context, livemode bool, fam chain.Family, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, blacklistCheckTimeout)
	defer cancel()
	return s.cfg.Validator.CheckBlacklist(ctx, livemode, fam, addr)
}

// SetDestination keeps the single-address operations working on top of wallets (seeding a
// merchant, setting a destination): the address becomes an active wallet of the family, or
// the active wallet with that address is answered. Like any new wallet it changes no product's
// payout; a product created later pays the family's default wallet unless told otherwise.
// allowUnverified skips only the on-chain checks that cannot run yet.
func (s *Service) SetDestination(ctx context.Context, merchantID string, livemode bool, family chain.Family, address string, allowUnverified bool, actor string) (store.Wallet, error) {
	w, err := s.checkWallet(ctx, livemode, WalletInput{Family: string(family), Address: address, Label: "Main wallet"}, allowUnverified)
	if err != nil {
		return store.Wallet{}, err
	}
	existing, err := s.store.ListWallets(ctx, merchantID, livemode, store.WalletFilter{Family: family})
	if err != nil {
		return store.Wallet{}, err
	}
	for _, e := range existing {
		if e.Address == w.Address {
			return e, nil
		}
	}
	return s.createWallet(ctx, merchantID, w, actor)
}

// Sample test wallets: addresses nobody holds a key for, so test payouts to them are burned.
// They let a new merchant try the whole flow before connecting a wallet of its own.
var sampleWallets = map[chain.Family]string{
	chain.EVM:    "0x000000000000000000000000000000000000dEaD",
	chain.Solana: "1nc1nerator11111111111111111111111111111111",
}

// SampleWalletLabel names a sample wallet everywhere it is shown.
const SampleWalletLabel = "Sample test wallet (demo, nobody holds its key)"

// SampleWalletAddress is the sample address of a family.
func SampleWalletAddress(f chain.Family) string { return sampleWallets[f] }

// DashSampleWallet adds the family's sample wallet in test mode only, which asks for no code.
// Like any wallet it changes no product's payout; when it is the family's first wallet, it becomes
// the default. Asking twice answers the wallet already there.
func (s *Service) DashSampleWallet(ctx context.Context, du DashUser, livemode bool, family string) (WalletView, error) {
	if livemode {
		return WalletView{}, newErr(KindForbidden, CodeTestModeOnly, "Sample wallets are for Test mode only. Add your own wallet for live payments.")
	}
	fam, err := parseFamily("family", family)
	if err != nil {
		return WalletView{}, err
	}
	list, err := s.store.ListWallets(ctx, du.MerchantID, false, store.WalletFilter{Family: fam})
	if err != nil {
		return WalletView{}, err
	}
	for _, w := range list {
		if w.Address == sampleWallets[fam] {
			return walletView(w), nil
		}
	}
	w, err := s.checkWallet(ctx, false, WalletInput{Family: string(fam), Label: SampleWalletLabel, Address: sampleWallets[fam]}, false)
	if err != nil {
		return WalletView{}, err
	}
	created, err := s.createWallet(ctx, du.MerchantID, w, du.actor())
	if err != nil {
		return WalletView{}, err
	}
	return walletView(created), nil
}

// HasLiveWallet reports whether the merchant has an active wallet to be paid in live mode. Until
// it does, the dashboard starts a browser that never chose a mode in Test mode.
func (s *Service) HasLiveWallet(ctx context.Context, merchantID string) (bool, error) {
	return s.store.HasActiveWallet(ctx, merchantID, true)
}
