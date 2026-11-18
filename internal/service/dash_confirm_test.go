package service

import "testing"

// The confirmation answer shows where the code went without giving the address away.
func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"owner@acme.studio": "o•••@acme.studio",
		"o@acme.studio":     "o•••@acme.studio",
		"@acme.studio":      "•••",
		"no-at-sign":        "•••",
	} {
		t.Run(in, func(t *testing.T) {
			if got := MaskEmail(in); got != want {
				t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// Only the six actions have a code, and only the five Live ones need Live mode.
func TestConfirmActions(t *testing.T) {
	for _, a := range []string{ConfirmWalletCreate, ConfirmWalletArchive, ConfirmAPIKey, ConfirmWebhookEndpoint, ConfirmUnlock} {
		if !liveConfirmActions[a] {
			t.Errorf("%s is not a Live action", a)
		}
	}
	if liveConfirmActions[ConfirmTOTPEnroll] || len(liveConfirmActions) != 5 {
		t.Errorf("live actions: %v", liveConfirmActions)
	}
	if m := confirmationRequired(nil); m.Code != CodeConfirmationRequired || m.Kind != KindForbidden || len(m.Details["methods"].([]string)) != 0 {
		t.Errorf("no methods: %+v", m)
	}
	if m := confirmationRequired([]string{MethodEmail, MethodTOTP}); m.Message != "Confirm it's you with the code from your authenticator app or a code we email you." {
		t.Errorf("both methods: %s", m.Message)
	}
}
