package service

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// ProductInput is a create or patch request. A member not Set is absent. On a patch, null or ""
// clears description, max_amount and success_url, and null or [] clears suggested_amounts; on a
// create null is the same as leaving a member out.
type ProductInput struct {
	Name             Field[string]
	Description      Field[string]
	PriceType        Field[string]
	Amount           Field[string]
	MinAmount        Field[string]
	MaxAmount        Field[string]
	SuggestedAmounts Field[[]string]
	SuccessURL       Field[string]
	Metadata         Field[map[string]string]
	Active           Field[bool]
	// Payout sets where the money goes per network (chain id): a chain in the map gets that list
	// (empty: the product does not take that network); a chain not in the map keeps its items on
	// a patch and pays its family's oldest active wallet at 100% on create. Nil is an absent payout.
	Payout map[string][]PayoutItemInput
	// Networks sets which stablecoins the product takes per network (chain id, D60): all, some
	// (with a list) or none. A chain not in the map keeps its choice on a patch and takes all on
	// create; applyPayout reconciles it with Payout. Nil is absent.
	Networks map[string]NetworkInput
	// Links says what a patch that changes money does to the product's active links: LinksKeep or
	// LinksDeactivate. Null is absent.
	Links Field[string]
}

// Limits and choices of a product patch: LinksKeep and LinksDeactivate are the values
// ProductInput.Links accepts.
const (
	maxDescriptionLen = 500

	LinksKeep       = "keep"
	LinksDeactivate = "deactivate"

	CodeLinksDecisionRequired = "links_decision_required"
)

// acceptedChains is the set of chains the merchant takes payments on in this mode, falling back
// to every chain of the mode when nothing is configured yet.
func (s *Service) acceptedChains(ctx context.Context, a Auth) ([]chain.Chain, error) {
	assets, err := s.store.AcceptedAssets(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return s.cfg.Registry.ForMode(a.Livemode), nil
	}
	seen := map[string]bool{}
	var out []chain.Chain
	for _, x := range assets {
		c, ok := s.cfg.Registry.Get(x.Chain)
		if ok && c.Livemode == a.Livemode && !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func optAmount(field string, s *string) (*money.Units, error) {
	if s == nil {
		return nil, nil
	}
	u, err := parseAmount(field, *s)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// validateDescription accepts plain text of 1 to 500 characters: line breaks are kept, other
// control characters are refused.
func validateDescription(d string) error {
	if n := utf8.RuneCountInString(d); n == 0 || n > maxDescriptionLen {
		return invalid("description must have 1 to %d characters", maxDescriptionLen)
	}
	if strings.ContainsFunc(d, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		return invalid("description must be plain text")
	}
	return nil
}

// applyProductInput merges in into p and validates the result: the texts, the price, the success
// URL, the metadata and activity, then the payout, then the price against the accepted networks.
func (s *Service) applyProductInput(ctx context.Context, a Auth, p *store.Product, in ProductInput, creating bool) error {
	if err := applyProductTexts(p, in, creating); err != nil {
		return err
	}
	if err := applyProductPrice(p, in, creating); err != nil {
		return err
	}
	if err := applyProductSuccessURL(a, p, in, creating); err != nil {
		return err
	}
	if in.Metadata.Set {
		p.Metadata = in.Metadata.Value
	}
	if err := validateMetadata(p.Metadata); err != nil {
		return err
	}
	if in.Active.Given() {
		p.Active = in.Active.Value
	}

	activating := in.Active.Given() && in.Active.Value
	if creating || in.Payout != nil || in.Networks != nil || activating {
		if err := s.applyPayout(ctx, a, p, in.Payout, in.Networks, creating); err != nil {
			return err
		}
	}

	chains, err := s.acceptedChains(ctx, a)
	if err != nil {
		return err
	}
	if p.PriceType == store.PriceFixed {
		return checkFixedPrice(p, chains)
	}
	return checkOpenPrice(p, chains)
}

// applyProductTexts merges the name and the description.
func applyProductTexts(p *store.Product, in ProductInput, creating bool) error {
	if in.Name.Given() {
		p.Name = in.Name.Value
	}
	if n := utf8.RuneCountInString(p.Name); n == 0 || n > maxNameLen {
		return invalid("name must have 1 to %d characters", maxNameLen)
	}
	if desc := patchText(in.Description, creating); desc != nil {
		if *desc == "" && !creating {
			p.Description = nil
		} else {
			if err := validateDescription(*desc); err != nil {
				return at(err, "description")
			}
			p.Description = desc
		}
	}
	return nil
}

// applyProductSuccessURL merges the success URL: "" (or null on a patch) clears it.
func applyProductSuccessURL(a Auth, p *store.Product, in ProductInput, creating bool) error {
	u := patchText(in.SuccessURL, creating)
	switch {
	case u == nil:
	case *u == "":
		p.SuccessURL = nil
	default:
		if err := validateURL("success_url", *u, !a.Livemode); err != nil {
			return err
		}
		p.SuccessURL = u
	}
	return nil
}

// applyProductPrice merges the price type and its amounts; checkFixedPrice or checkOpenPrice
// validates them once the payout is known.
func applyProductPrice(p *store.Product, in ProductInput, creating bool) error {
	if in.PriceType.Given() {
		if !creating && in.PriceType.Value != string(p.PriceType) {
			return invalid("price_type cannot be changed")
		}
		p.PriceType = store.PriceType(in.PriceType.Value)
	}
	if p.PriceType != store.PriceFixed && p.PriceType != store.PriceOpen {
		return invalid("price_type must be fixed or open")
	}
	var err error
	if in.Amount.Given() {
		if p.AmountCents, err = optAmount("amount", in.Amount.Ptr()); err != nil {
			return err
		}
	}
	if in.MinAmount.Given() {
		if p.MinAmountCents, err = optAmount("min_amount", in.MinAmount.Ptr()); err != nil {
			return err
		}
	}
	if max := patchText(in.MaxAmount, creating); max != nil && *max == "" {
		p.MaxAmountCents = nil
	} else if max != nil {
		if p.MaxAmountCents, err = optAmount("max_amount", max); err != nil {
			return err
		}
	}
	if in.SuggestedAmounts.Set {
		p.SuggestedCents = nil
		for _, x := range in.SuggestedAmounts.Value {
			u, err := parseAmount("suggested_amounts", x)
			if err != nil {
				return err
			}
			p.SuggestedCents = append(p.SuggestedCents, u)
		}
	}
	return nil
}

// checkFixedPrice validates a fixed price: an amount, at least the fee floor, and none of the
// open price's members.
func checkFixedPrice(p *store.Product, chains []chain.Chain) error {
	if p.AmountCents == nil {
		return invalid("amount is required for a fixed price")
	}
	if p.MinAmountCents != nil || p.MaxAmountCents != nil || len(p.SuggestedCents) > 0 {
		return invalid("min_amount, max_amount and suggested_amounts apply only to open prices")
	}
	return checkFloor("amount", *p.AmountCents, chains)
}

// checkOpenPrice validates an open price: a minimum at least the fee floor, an optional maximum
// above it, and at most maxSuggested suggested amounts within the range.
func checkOpenPrice(p *store.Product, chains []chain.Chain) error {
	if p.AmountCents != nil {
		return invalid("amount applies only to fixed prices; use min_amount")
	}
	if p.MinAmountCents == nil {
		return invalid("min_amount is required for an open price")
	}
	if err := checkFloor("min_amount", *p.MinAmountCents, chains); err != nil {
		return err
	}
	if p.MaxAmountCents != nil && p.MaxAmountCents.Cmp(*p.MinAmountCents) < 0 {
		return invalid("max_amount must be at least min_amount")
	}
	if len(p.SuggestedCents) > maxSuggested {
		return invalid("suggested_amounts holds at most %d amounts", maxSuggested)
	}
	for _, x := range p.SuggestedCents {
		if x.Cmp(*p.MinAmountCents) < 0 || (p.MaxAmountCents != nil && x.Cmp(*p.MaxAmountCents) > 0) {
			return invalid("suggested_amounts must lie between min_amount and max_amount")
		}
	}
	if len(p.SuggestedCents) == 0 {
		p.SuggestedCents = nil
	}
	return nil
}

func newProduct(a Auth) store.Product {
	return store.Product{ID: ids.New(ids.Product), MerchantID: a.MerchantID, Livemode: a.Livemode, Active: true}
}

func checkProductCreate(in ProductInput) error {
	if !in.Name.Given() {
		return invalid("name is required")
	}
	if !in.PriceType.Given() {
		return invalid("price_type is required")
	}
	return nil
}

// CreateProduct validates the input and creates a product for the merchant; it needs the products
// write scope.
func (s *Service) CreateProduct(ctx context.Context, a Auth, in ProductInput) (view.Product, error) {
	if err := a.Require(apikey.ProductsWrite); err != nil {
		return view.Product{}, err
	}
	if err := checkProductCreate(in); err != nil {
		return view.Product{}, err
	}
	p := newProduct(a)
	if err := s.applyProductInput(ctx, a, &p, in, true); err != nil {
		return view.Product{}, err
	}
	created, err := s.store.CreateProduct(ctx, p, a.actor())
	if err != nil {
		return view.Product{}, storePayoutError(err)
	}
	return s.productView(ctx, a, created)
}

func productNotFound() *Error {
	return newErr(KindNotFound, CodeProductNotFound, "no such product")
}

// GetProduct returns one of the merchant's products, or a not-found error; it needs the products
// read scope.
func (s *Service) GetProduct(ctx context.Context, a Auth, id string) (view.Product, error) {
	if err := a.Require(apikey.ProductsRead); err != nil {
		return view.Product{}, err
	}
	p, err := s.store.GetProduct(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return view.Product{}, productNotFound()
	}
	if err != nil {
		return view.Product{}, err
	}
	return s.productView(ctx, a, p)
}

// ProductList is one page of products and whether more follow.
type ProductList struct {
	Data    []view.Product `json:"data"`
	HasMore bool           `json:"has_more"`
}

// ListProducts returns a page of the merchant's products after startingAfter; it needs the products
// read scope.
func (s *Service) ListProducts(ctx context.Context, a Auth, limit int, startingAfter string) (ProductList, error) {
	if err := a.Require(apikey.ProductsRead); err != nil {
		return ProductList{}, err
	}
	list, more, err := s.store.ListProducts(ctx, a.MerchantID, a.Livemode, limit, startingAfter)
	if err != nil {
		return ProductList{}, err
	}
	totals, err := s.linkTotals(ctx, list)
	if err != nil {
		return ProductList{}, err
	}
	methods, err := s.productMethods(ctx, a)
	if err != nil {
		return ProductList{}, err
	}
	out := ProductList{Data: []view.Product{}, HasMore: more}
	for _, p := range list {
		v := s.view.Product(p, totals[p.ID])
		v.PaymentMethods = methods(p)
		out.Data = append(out.Data, v)
	}
	return out, nil
}

// UpdateProduct applies a patch to one of the merchant's products; it needs the products write
// scope.
func (s *Service) UpdateProduct(ctx context.Context, a Auth, id string, in ProductInput) (view.Product, error) {
	if err := a.Require(apikey.ProductsWrite); err != nil {
		return view.Product{}, err
	}
	p, err := s.updateProduct(ctx, a, id, in)
	if err != nil {
		return view.Product{}, err
	}
	return s.productView(ctx, a, p)
}

// updateProduct applies a patch under the product lock. A patch that changes money (an amount or
// the payout, compared with what is stored) while the product has active links must say what
// happens to them (in.Links); deactivate turns them off in the same transaction. A metadata
// change must keep every link that stays active within the metadata limits once merged.
func (s *Service) updateProduct(ctx context.Context, a Auth, id string, in ProductInput) (store.Product, error) {
	if in.Links.Given() && in.Links.Value != LinksKeep && in.Links.Value != LinksDeactivate {
		return store.Product{}, at(invalid("links must be keep or deactivate"), "links")
	}
	p, err := s.store.EditProductAs(ctx, a.MerchantID, a.Livemode, id, a.actor(), func(p *store.Product, active []store.PaymentLink) (bool, error) {
		before := *p
		before.Payout = store.ClonePayout(p.Payout)
		before.Networks = store.CloneNetworks(p.Networks)
		if err := s.applyProductInput(ctx, a, p, in, false); err != nil {
			return false, err
		}
		deactivate := false
		if len(active) > 0 && !sameMoney(before, *p) {
			if !in.Links.Given() {
				return false, at(linksDecisionRequired(len(active)), "links")
			}
			deactivate = in.Links.Value == LinksDeactivate
		}
		if in.Metadata.Set && !deactivate {
			for _, l := range active {
				if err := validateMetadata(mergeMetadata(p.Metadata, l.Metadata)); err != nil {
					return false, at(newErr(KindInvalid, CodeInvalidRequest, "with the metadata of the active link %s, %s", l.ID,
						AsError(err).Message), "metadata")
				}
			}
		}
		return deactivate, nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.Product{}, productNotFound()
	}
	if err != nil {
		return store.Product{}, storePayoutError(err)
	}
	return p, nil
}

func linksDecisionRequired(n int) *Error {
	if n == 1 {
		return newErr(KindConflict, CodeLinksDecisionRequired,
			"This product has 1 active link. Choose whether to keep it active or deactivate it.")
	}
	return newErr(KindConflict, CodeLinksDecisionRequired,
		"This product has %d active links. Choose whether to keep them active or deactivate them.", n)
}

// sameMoney reports whether two versions of a product charge the same amounts and pay the same
// recipients: only such an edit leaves its links' checkouts as they were.
func sameMoney(a, b store.Product) bool {
	return sameUnits(a.AmountCents, b.AmountCents) && sameUnits(a.MinAmountCents, b.MinAmountCents) &&
		sameUnits(a.MaxAmountCents, b.MaxAmountCents) && store.SamePayout(a.Payout, b.Payout)
}

func sameUnits(a, b *money.Units) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (s *Service) productView(ctx context.Context, a Auth, p store.Product) (view.Product, error) {
	totals, err := s.linkTotals(ctx, []store.Product{p})
	if err != nil {
		return view.Product{}, err
	}
	methods, err := s.productMethods(ctx, a)
	if err != nil {
		return view.Product{}, err
	}
	v := s.view.Product(p, totals[p.ID])
	v.PaymentMethods = methods(p)
	return v, nil
}

// productMethods reads the merchant's accepted assets in a's mode once and returns, per product,
// the (chain, asset) pairs a session opened now would offer: effectiveMethods minus the paused
// ones (D64), as depositEntries picks them. The payout blacklist check is left to the session, so a frozen recipient still
// lists its chain here.
func (s *Service) productMethods(ctx context.Context, a Auth) (func(store.Product) []view.PaymentMethod, error) {
	assets, needed, err := s.payableChains(ctx, a)
	if err != nil {
		return nil, err
	}
	pauses, err := s.ChainPauses(ctx)
	if err != nil {
		return nil, err
	}
	return func(p store.Product) []view.PaymentMethod {
		out := []view.PaymentMethod{}
		for _, x := range unpaused(effectiveMethods(s.cfg.Registry, a.Livemode, assets, needed, &p), pauses) {
			out = append(out, view.PaymentMethod{Chain: x.Chain, ChainName: view.ChainName(x.Chain), Asset: x.Asset})
		}
		return out
	}, nil
}

// linkTotals counts, per product, its links and those live now.
func (s *Service) linkTotals(ctx context.Context, products []store.Product) (map[string]view.LinkTotals, error) {
	out := make(map[string]view.LinkTotals, len(products))
	byID := map[string]store.Product{}
	idList := make([]string, 0, len(products))
	for _, p := range products {
		out[p.ID] = view.LinkTotals{}
		byID[p.ID] = p
		idList = append(idList, p.ID)
	}
	if len(idList) == 0 {
		return out, nil
	}
	links, err := s.store.LinksOfProducts(ctx, idList)
	if err != nil {
		return nil, err
	}
	linkIDs := make([]string, len(links))
	for i, l := range links {
		linkIDs[i] = l.ID
	}
	counts, err := s.store.LinkCounts(ctx, linkIDs)
	if err != nil {
		return nil, err
	}
	now := s.store.Now()
	for _, l := range links {
		t := out[l.ProductID]
		t.Total++
		if live, _ := Liveness(l, byID[l.ProductID].Active, counts[l.ID], now); live {
			t.Live++
		}
		out[l.ProductID] = t
	}
	return out, nil
}
