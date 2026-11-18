package service

import "context"

// Get started steps (D57), in order. The first run shows "choose" (which networks to take)
// before the wallets.
const (
	OnboardingChoose  = "choose"
	OnboardingWallets = "wallets"
	OnboardingProduct = "product"
	OnboardingDone    = "done"
)

// OnboardingNetwork is a network the mode takes payments on and whether its family has an
// active wallet.
type OnboardingNetwork struct {
	Chain     string `json:"chain"`
	Family    string `json:"family"`
	HasWallet bool   `json:"has_wallet"`
}

// Onboarding is where the merchant stands in Get started for one mode: the dashboard shows the
// menu item while Visible, with Done of Steps behind it, and opens the page at Step.
type Onboarding struct {
	// Visible is false once the mode has a product: Get started is gone for good in that mode.
	Visible bool `json:"visible"`
	// FirstRun is an account with no product in either mode.
	FirstRun   bool                `json:"first_run"`
	HasWallet  bool                `json:"has_wallet"`
	HasProduct bool                `json:"has_product"`
	Networks   []OnboardingNetwork `json:"networks"`
	Steps      []string            `json:"steps"`
	Step       string              `json:"step"`
	Done       int                 `json:"done"`
}

// DashOnboarding answers GET /v1/dashboard/onboarding: one statement of existence checks and the
// accepted assets, whatever the account holds.
func (s *Service) DashOnboarding(ctx context.Context, a Auth) (Onboarding, error) {
	f, err := s.store.Onboarding(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return Onboarding{}, err
	}
	accepted, err := s.store.AcceptedAssets(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return Onboarding{}, err
	}
	families := map[string]bool{}
	for _, fam := range f.WalletFamilies {
		families[fam] = true
	}
	taken := map[string]bool{}
	for _, x := range accepted {
		taken[x.Chain] = true
	}
	networks := []OnboardingNetwork{}
	for _, c := range s.cfg.Registry.ForMode(a.Livemode) {
		if taken[c.ID] {
			networks = append(networks, OnboardingNetwork{Chain: c.ID, Family: string(c.Family), HasWallet: families[string(c.Family)]})
		}
	}
	return onboarding(f.HasProduct, f.AnyProduct, len(f.WalletFamilies) > 0, f.AnyWallet, networks), nil
}

// onboarding works out the steps from what the account holds: hasProduct and hasWallet in the
// mode, anyProduct and anyWallet in either.
func onboarding(hasProduct, anyProduct, hasWallet, anyWallet bool, networks []OnboardingNetwork) Onboarding {
	o := Onboarding{Visible: !hasProduct, FirstRun: !anyProduct, HasWallet: hasWallet, HasProduct: hasProduct, Networks: networks,
		Steps: []string{OnboardingWallets, OnboardingProduct}}
	if o.FirstRun {
		o.Steps = append([]string{OnboardingChoose}, o.Steps...)
	}
	switch {
	case hasProduct:
		o.Step = OnboardingDone
	case hasWallet:
		o.Step = OnboardingProduct
	case o.FirstRun && !anyWallet:
		o.Step = OnboardingChoose
	default:
		o.Step = OnboardingWallets
	}
	o.Done = len(o.Steps)
	for i, step := range o.Steps {
		if step == o.Step {
			o.Done = i
			break
		}
	}
	return o
}
