package service

import (
	"slices"
	"testing"
)

func TestOnboardingSteps(t *testing.T) {
	first := []string{OnboardingChoose, OnboardingWallets, OnboardingProduct}
	later := []string{OnboardingWallets, OnboardingProduct}
	cases := []struct {
		name                                         string
		hasProduct, anyProduct, hasWallet, anyWallet bool
		visible, firstRun                            bool
		steps                                        []string
		step                                         string
		done                                         int
	}{
		{name: "new account", visible: true, firstRun: true, steps: first, step: OnboardingChoose, done: 0},
		{name: "wallet in the other mode only", anyWallet: true, visible: true, firstRun: true, steps: first, step: OnboardingWallets, done: 1},
		{name: "wallet here", hasWallet: true, anyWallet: true, visible: true, firstRun: true, steps: first, step: OnboardingProduct, done: 2},
		{name: "product in the other mode", anyProduct: true, anyWallet: true, visible: true, steps: later, step: OnboardingWallets, done: 0},
		{name: "product elsewhere, wallet here", anyProduct: true, hasWallet: true, anyWallet: true, visible: true, steps: later,
			step: OnboardingProduct, done: 1},
		{name: "product here", hasProduct: true, anyProduct: true, steps: later, step: OnboardingDone, done: 2},
		{name: "product here, wallet archived", hasProduct: true, anyProduct: true, hasWallet: false, steps: later, step: OnboardingDone, done: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := onboarding(c.hasProduct, c.anyProduct, c.hasWallet, c.anyWallet, nil)
			if o.Visible != c.visible || o.FirstRun != c.firstRun || !slices.Equal(o.Steps, c.steps) || o.Step != c.step || o.Done != c.done ||
				o.HasProduct != c.hasProduct || o.HasWallet != c.hasWallet {
				t.Errorf("%s: %+v", c.name, o)
			}
		})
	}
}
