package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/service"
)

const payoutShape = `payout must be an object keyed by network such as {"base": [{"wallet_id": "wal_...", "share": "70.00"}, ...], "solana": []}`

// productPayout decodes a product's `payout` object: per network (chain id) a list of
// {wallet_id, share} in payout order, or [] to take no payments on that network. A network that
// is present is set; an absent one is left alone (on create: its family's default wallet at
// 100%). The service checks the keys against the chains of the mode.
func productPayout(raw json.RawMessage) (map[string][]service.PayoutItemInput, error) {
	if isNull(raw) {
		return nil, badParam("payout", "%s", payoutShape)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, badParam("payout", "%s", payoutShape)
	}
	out := map[string][]service.PayoutItemInput{}
	for k, v := range m {
		field := "payout." + k
		if isNull(v) {
			return nil, badParam(field, "%s must be a list of {wallet_id, share}; [] takes no payments on %s", field, k)
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(v, &items); err != nil {
			return nil, badParam(field, "%s must be a list of {wallet_id, share}", field)
		}
		list := []service.PayoutItemInput{}
		for i, it := range items {
			at := fmt.Sprintf("%s[%d]", field, i)
			for name := range it {
				if name != "wallet_id" && name != "share" {
					return nil, badParam(at+"."+name, "%s has an unknown field %q", at, name)
				}
			}
			id, err := decodeField[string](it, "wallet_id", "a wallet id")
			if err != nil {
				return nil, badParam(at+".wallet_id", "%s.wallet_id must be a wallet id", at)
			}
			share, err := decodeField[string](it, "share", "a percentage string")
			if err != nil {
				return nil, badParam(at+".share", "%s.share must be a percentage string such as \"70.00\"", at)
			}
			if id == nil {
				return nil, badParam(at+".wallet_id", "%s needs wallet_id and share", at)
			}
			if share == nil {
				return nil, badParam(at+".share", "%s needs wallet_id and share", at)
			}
			list = append(list, service.PayoutItemInput{WalletID: *id, Share: *share})
		}
		out[k] = list
	}
	return out, nil
}

const networksShape = `networks must be an object keyed by network such as {"base": {"accept": "all"}, "solana": {"accept": "some", "assets": ["USDC"]}}`

// productNetworks decodes a product's `networks` object (D60): per network (chain id)
// {accept, assets}, accept being all, some or none and assets a list of symbols (null is
// absent). An absent network is left alone. The service checks the keys, the accept values and
// the assets against the chains of the mode.
func productNetworks(raw json.RawMessage) (map[string]service.NetworkInput, error) {
	if isNull(raw) {
		return nil, badParam("networks", "%s", networksShape)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, badParam("networks", "%s", networksShape)
	}
	out := map[string]service.NetworkInput{}
	for k, v := range m {
		field := "networks." + k
		var obj map[string]json.RawMessage
		if isNull(v) || json.Unmarshal(v, &obj) != nil {
			return nil, badParam(field, "%s must be {\"accept\": \"all\" | \"some\" | \"none\", \"assets\": [...]}", field)
		}
		for name := range obj {
			if name != "accept" && name != "assets" {
				return nil, badParam(field+"."+name, "%s has an unknown field %q", field, name)
			}
		}
		accept, err := decodeField[string](obj, "accept", "all, some or none")
		if err != nil || accept == nil {
			return nil, badParam(field+".accept", "%s.accept must be all, some or none", field)
		}
		in := service.NetworkInput{Accept: *accept}
		if a, ok := obj["assets"]; ok && !isNull(a) {
			if err := json.Unmarshal(a, &in.Assets); err != nil {
				return nil, badParam(field+".assets", "%s.assets must be a list of stablecoin symbols such as [\"USDC\"]", field)
			}
			in.AssetsSet = true
		}
		out[k] = in
	}
	return out, nil
}

func walletInput(m map[string]json.RawMessage) (service.WalletInput, error) {
	var in service.WalletInput
	for _, f := range []struct {
		name string
		dst  *string
	}{{"family", &in.Family}, {"label", &in.Label}, {"address", &in.Address}} {
		v, err := decodeField[string](m, f.name, "a string")
		if err != nil {
			return in, err
		}
		if v != nil {
			*f.dst = *v
		}
	}
	if in.Family == "" || in.Address == "" {
		return in, badRequest("family and address are required")
	}
	return in, nil
}

// mountPayouts serves wallets read-only: adding or archiving one decides where money goes, so it
// takes the dashboard's confirmation code in Live mode, which no API key can give.
func (s *Server) mountPayouts() {
	s.route("GET /v1/wallets", apikey.WalletsRead, func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		q := r.URL.Query()
		list, err := s.svc.ListWallets(r.Context(), a, q.Get("family"), q.Get("include_archived") == "true")
		return respond(w, list, err)
	})
	s.route("GET /v1/wallets/{id}", apikey.WalletsRead, func(w http.ResponseWriter, r *http.Request, a service.Auth) error {
		v, err := s.svc.GetWallet(r.Context(), a, r.PathValue("id"))
		return respond(w, v, err)
	})
}

// stepUpProof reads the confirmation of a sensitive dashboard action (D58): "email_code", a
// code emailed for the action, or "totp", the authenticator's code. "password" is still accepted
// in the body and ignored: no such action takes the password any more.
func stepUpProof(m map[string]json.RawMessage) (service.StepUpProof, error) {
	var p service.StepUpProof
	var err error
	if p.EmailCode, err = decodeField[string](m, "email_code", "a string"); err != nil {
		return p, err
	}
	if p.TOTP, err = decodeField[string](m, "totp", "a string"); err != nil {
		return p, err
	}
	return p, nil
}

// withStepUp is a body's own fields plus the confirmation fields, for readFields. It copies
// fields, so a shared list passed in is never written to.
func withStepUp(fields ...string) []string {
	return append(append([]string(nil), fields...), "email_code", "totp", "password")
}

func (s *Server) mountDashboardPayouts() {
	limiter := s.publicLimiter()
	s.modeRoute("GET", "/wallets", func(w http.ResponseWriter, r *http.Request, _ service.DashSession, a service.Auth) error {
		q := r.URL.Query()
		list, err := s.svc.ListWallets(r.Context(), a, q.Get("family"), q.Get("include_archived") == "true")
		return respond(w, list, err)
	})
	s.modeRoute("POST", "/wallets", func(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
		if err := limiter.allowKey(w, r, RouteDashboardTOTP, ds.User.ID); err != nil {
			return err
		}
		m, err := readFields(r, withStepUp("family", "label", "address")...)
		if err != nil {
			return err
		}
		in, err := walletInput(m)
		if err != nil {
			return err
		}
		proof, err := stepUpProof(m)
		if err != nil {
			return err
		}
		v, err := s.svc.DashCreateWallet(r.Context(), ds.User, a.Livemode, in, proof)
		return respond(w, v, err)
	})
	s.modeRoute("POST", "/wallets/sample", func(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
		if err := service.RequireTestMode(a); err != nil {
			return err
		}
		// Test mode asks for no code; the confirmation fields are accepted and ignored.
		m, err := readFields(r, withStepUp("family")...)
		if err != nil {
			return err
		}
		family, err := decodeField[string](m, "family", "evm or solana")
		if err != nil {
			return err
		}
		fam := "evm"
		if family != nil {
			fam = *family
		}
		v, err := s.svc.DashSampleWallet(r.Context(), ds.User, a.Livemode, fam)
		return respond(w, v, err)
	})
	s.modeRoute("POST", "/wallets/{id}/archive", func(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
		if err := limiter.allowKey(w, r, RouteDashboardTOTP, ds.User.ID); err != nil {
			return err
		}
		m, err := readFields(r, withStepUp()...)
		if err != nil {
			return err
		}
		proof, err := stepUpProof(m)
		if err != nil {
			return err
		}
		v, err := s.svc.DashArchiveWallet(r.Context(), ds.User, a.Livemode, r.PathValue("id"), proof)
		return respond(w, v, err)
	})
	// Making a wallet the default moves no money of existing products: no code (D57). Body ignored.
	s.modeRoute("POST", "/wallets/{id}/default", func(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
		v, err := s.svc.DashSetDefaultWallet(r.Context(), ds.User, a.Livemode, r.PathValue("id"))
		return respond(w, v, err)
	})
}
