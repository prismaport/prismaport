package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/view"
)

// MountSolanaCheckout adds the public connected-wallet endpoints for Solana. Like the rest of
// the checkout API they take no key: the session id is the only scope. Building and submitting
// are rate limited per client IP (see ConfigurePublic), and so is the Solana Pay POST.
func (s *Server) MountSolanaCheckout(c *checkout.Checkout) {
	s.mountSolanaPay(c)
	strs := func(r *http.Request, names ...string) (map[string]string, error) {
		body, err := readBody(r)
		if err != nil {
			return nil, err
		}
		m, err := fields(body, names...)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, name := range names {
			v, err := decodeField[string](m, name, "a string")
			if err != nil {
				return nil, err
			}
			if v == nil {
				return nil, badRequest("%s is required", name)
			}
			out[name] = *v
		}
		return out, nil
	}
	s.public("POST /v1/checkout/sessions/{id}/solana/transaction", RouteSolanaTransaction, func(w http.ResponseWriter, r *http.Request) error {
		in, err := strs(r, "chain", "asset", "account")
		if err != nil {
			return err
		}
		out, err := c.WalletTransaction(r.Context(), r.PathValue("id"), checkout.WalletTxRequest{Chain: in["chain"], Asset: in["asset"], Payer: in["account"]})
		return respond(w, out, err)
	})
	s.public("POST /v1/checkout/sessions/{id}/solana/submit", RouteSolanaSubmit, func(w http.ResponseWriter, r *http.Request) error {
		in, err := strs(r, "chain", "asset", "transaction")
		if err != nil {
			return err
		}
		out, err := c.WalletSubmit(r.Context(), r.PathValue("id"), checkout.WalletSubmitRequest{Chain: in["chain"], Asset: in["asset"],
			Transaction: in["transaction"]})
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusAccepted, out)
	})
}

// SolanaPayError is the error answer of the Solana Pay routes: wallets show the top-level
// message; error carries the usual code.
type SolanaPayError struct {
	Message string           `json:"message"`
	Error   view.ErrorDetail `json:"error"`
}

func (s *Server) writeSolanaPayError(w http.ResponseWriter, r *http.Request, err error) {
	status, e := s.errorOf(w, r, err)
	_ = writeJSON(w, status, SolanaPayError{Message: sentence(e.Message),
		Error: view.ErrorDetail{Code: e.Code, Message: e.Message, Param: e.Param}})
}

// sentence capitalizes a message and ends it with a period, as a wallet shows it alone.
func sentence(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return "The payment request failed."
	}
	first, n := utf8.DecodeRuneInString(m)
	m = string(unicode.ToUpper(first)) + m[n:]
	if !strings.HasSuffix(m, ".") && !strings.HasSuffix(m, "!") && !strings.HasSuffix(m, "?") {
		m += "."
	}
	return m
}

// mountSolanaPay adds the Solana Pay transaction request of a session: the checkout shows
// solana:<url-encoded https URL of this route with chain and asset> as a QR, the wallet GETs the
// label and icon, then POSTs its account and signs and sends the transaction it gets back. Any
// origin may call both (CORS * like the rest of the checkout API). The POST builds what
// solana/transaction builds and the GET reads accounts on chain, so both take their tokens from
// the solana_transaction bucket per client IP: scanning does not add to what one IP can build.
func (s *Server) mountSolanaPay(c *checkout.Checkout) {
	const path = "/v1/checkout/sessions/{id}/solana/pay"
	target := func(r *http.Request) (string, string, error) {
		q := r.URL.Query()
		chainID, asset := q.Get("chain"), q.Get("asset")
		if chainID == "" {
			return "", "", badParam("chain", "the payment link is missing its network (chain)")
		}
		if asset == "" {
			return "", "", badParam("asset", "the payment link is missing its token (asset)")
		}
		return chainID, asset, nil
	}
	s.publicWith("GET "+path, RouteSolanaTransaction, s.writeSolanaPayError, func(w http.ResponseWriter, r *http.Request) error {
		chainID, asset, err := target(r)
		if err != nil {
			return err
		}
		out, err := c.SolanaPayInfo(r.Context(), r.PathValue("id"), chainID, asset)
		return respond(w, out, err)
	})
	s.publicWith("POST "+path, RouteSolanaTransaction, s.writeSolanaPayError, func(w http.ResponseWriter, r *http.Request) error {
		chainID, asset, err := target(r)
		if err != nil {
			return err
		}
		body, err := readBody(r)
		if err != nil {
			return err
		}
		// The spec lets wallets send fields of later versions: only account is read.
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) != nil || m == nil {
			return badRequest("the request body must be a JSON object with the wallet's account")
		}
		account, err := decodeField[string](m, "account", "a string")
		if err != nil {
			return err
		}
		if account == nil || *account == "" {
			return badParam("account", "account is required")
		}
		out, err := c.SolanaPay(r.Context(), r.PathValue("id"), chainID, asset, *account)
		return respond(w, out, err)
	})
}
