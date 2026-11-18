package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/prismaport/prismaport/internal/service"
)

// emailBucket keys a per-email limit by the hash of the normalized address, so the limiter table
// holds no addresses and the key length is bounded.
func emailBucket(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
}

// requiredStrings reads the named string fields of a JSON body; every one is required.
func requiredStrings(r *http.Request, names ...string) ([]string, error) {
	m, err := readFields(r, names...)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(names))
	for i, n := range names {
		v, err := decodeField[string](m, n, "a string")
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, badRequest("%s are required", strings.Join(names, ", "))
		}
		out[i] = *v
	}
	return out, nil
}

// selfServe holds the public account routes: none needs a session, and each answers the same
// whether or not the email has an account.
type selfServe struct {
	s       *Server
	limiter *publicLimiter
}

// mountSelfServe adds the public account routes: sign-up, confirming the email, resending the
// code, and the password reset by emailed link. None of them needs a session; each answers the
// same whether or not the email has an account. Like login they refuse cross-site requests and non-JSON bodies.
func (s *Server) mountSelfServe(limiter *publicLimiter) {
	h := selfServe{s: s, limiter: limiter}
	h.post("/auth/signup", h.signup)
	h.post("/auth/resend-code", h.emailing(s.svc.ResendVerification))
	h.post("/auth/verify-email", h.verifyEmail)
	h.post("/auth/password/forgot", h.emailing(s.svc.ForgotPassword))
	// The reset page asks whether its link works before it shows the form; asking spends nothing.
	h.post("/auth/password/reset/check", h.coded(func(r *http.Request, token string) (any, error) {
		return s.svc.CheckResetLink(r.Context(), token)
	}))
	h.post("/auth/password/reset", h.resetPassword)
	h.post("/auth/totp/lost", h.emailing(s.svc.TOTPLost))
	// The confirm page asks whether its link works before it shows the form; asking spends nothing.
	h.post("/auth/totp/lost/check", h.coded(func(r *http.Request, token string) (any, error) {
		return s.svc.CheckTOTPLostLink(r.Context(), token)
	}))
	h.post("/auth/totp/lost/confirm", h.confirmTOTPLost)
	h.post("/auth/totp/removal/cancel_link", h.coded(func(r *http.Request, token string) (any, error) {
		return map[string]bool{"ok": true}, s.svc.CancelTOTPRemovalLink(r.Context(), token)
	}))
}

// post registers a public POST route under the dashboard prefix: it refuses cross-site requests
// and non-JSON bodies before h runs.
func (h selfServe) post(path string, f func(w http.ResponseWriter, r *http.Request) error) {
	h.s.mux.HandleFunc("POST "+dashPrefix+path, func(w http.ResponseWriter, r *http.Request) {
		dashHeaders(w)
		err := h.s.fetchSite(r)
		if err == nil {
			err = jsonBody(r)
		}
		if err == nil {
			err = f(w, r)
		}
		if err != nil {
			h.s.writeError(w, r, err)
		}
	})
}

// writeOK answers {"ok": true}.
func writeOK(w http.ResponseWriter) error {
	return writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sends takes the tokens of a request that emails someone: per client IP, then per email.
func (h selfServe) sends(w http.ResponseWriter, r *http.Request, email string) error {
	if err := h.limiter.allow(w, r, RouteDashboardSignup); err != nil {
		return err
	}
	return h.limiter.allowKey(w, r, RouteDashboardEmail, emailBucket(email))
}

// emailing adapts a route whose body is {"email"} and that emails it: it takes the sending
// tokens, runs send and answers {"ok": true}.
func (h selfServe) emailing(send func(ctx context.Context, email string) error) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		v, err := requiredStrings(r, "email")
		if err != nil {
			return err
		}
		if err := h.sends(w, r, v[0]); err != nil {
			return err
		}
		if err := send(r.Context(), v[0]); err != nil {
			return err
		}
		return writeOK(w)
	}
}

// coded adapts a route whose body is {"token"} from an emailed link: it takes a token of the
// per-IP code bucket and answers 200 with what f returns.
func (h selfServe) coded(f func(r *http.Request, token string) (any, error)) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := h.limiter.allow(w, r, RouteDashboardCode); err != nil {
			return err
		}
		v, err := requiredStrings(r, "token")
		if err != nil {
			return err
		}
		out, err := f(r, v[0])
		return respond(w, out, err)
	}
}

func (h selfServe) signup(w http.ResponseWriter, r *http.Request) error {
	v, err := requiredStrings(r, "business_name", "email", "password")
	if err != nil {
		return err
	}
	if err := h.sends(w, r, v[1]); err != nil {
		return err
	}
	if err := h.s.svc.Signup(r.Context(), service.SignupInput{BusinessName: v[0], Email: v[1], Password: v[2]}); err != nil {
		return err
	}
	return writeOK(w)
}

func (h selfServe) verifyEmail(w http.ResponseWriter, r *http.Request) error {
	if err := h.limiter.allow(w, r, RouteDashboardCode); err != nil {
		return err
	}
	m, err := readFields(r, "email", "code", "password")
	if err != nil {
		return err
	}
	d := &members{m: m}
	email := member[string](d, "email", "a string")
	code := member[string](d, "code", "a string")
	password := member[string](d, "password", "a string")
	if d.err != nil {
		return d.err
	}
	if !email.Given() || !code.Given() {
		return badRequest("email, code are required")
	}
	// The browser that enters the code stays signed in when it holds this user's session;
	// any other one sends the password (service.VerifyEmail).
	var keep *service.DashSession
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if ds, err := h.s.svc.DashboardSession(r.Context(), c.Value); err == nil {
			keep = &ds
		}
	}
	if err := h.s.svc.VerifyEmail(r.Context(), email.Value, code.Value, password.Ptr(), keep); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]bool{"verified": true})
}

func (h selfServe) resetPassword(w http.ResponseWriter, r *http.Request) error {
	if err := h.limiter.allow(w, r, RouteDashboardCode); err != nil {
		return err
	}
	m, err := readFields(r, "token", "new_password", "totp", "recovery_code")
	if err != nil {
		return err
	}
	d := &members{m: m}
	token := member[string](d, "token", "a string")
	pw := member[string](d, "new_password", "a string")
	totp := member[string](d, "totp", "a string")
	recovery := member[string](d, "recovery_code", "a string")
	if d.err != nil {
		return d.err
	}
	if !token.Given() || !pw.Given() {
		return badRequest("token and new_password are required")
	}
	// A null or absent totp or recovery_code is sent as "", which the service reads as none.
	if err := h.s.svc.ResetPassword(r.Context(), token.Value, pw.Value, totp.Value, recovery.Value); err != nil {
		return err
	}
	// Every session of the account ended, this browser's included.
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		clearSessionCookie(w, r)
	}
	return writeOK(w)
}

func (h selfServe) confirmTOTPLost(w http.ResponseWriter, r *http.Request) error {
	if err := h.limiter.allow(w, r, RouteDashboardCode); err != nil {
		return err
	}
	v, err := requiredStrings(r, "token", "password")
	if err != nil {
		return err
	}
	// One bucket per link as well: the password is guessed against the link's account.
	if err := h.limiter.allowKey(w, r, RouteDashboardTOTP, emailBucket(v[0])); err != nil {
		return err
	}
	out, err := h.s.svc.ConfirmTOTPLost(r.Context(), v[0], v[1])
	return respond(w, out, err)
}
