package api

import (
	"encoding/json"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/service"
)

const (
	// SessionCookie holds the dashboard session token.
	SessionCookie = "pp_session"
	// CSRFHeader must carry the session's CSRF token on every dashboard request that changes
	// something.
	CSRFHeader = "X-CSRF-Token"
	dashPrefix = "/v1/dashboard"
)

type dashHandler func(w http.ResponseWriter, r *http.Request, ds service.DashSession) error

func dashHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func csrfFailed(msg string) error {
	return &service.Error{Kind: service.KindForbidden, Code: service.CodeCSRFFailed, Message: msg}
}

// sameSite refuses requests a browser marks as coming from another site, or from another host
// of this site: the landing page, the checkout and the API host are same-site with the
// dashboard's host. The embedded dashboard is same-origin only; it sends no CORS headers, so this
// and the CSRF token are what keep other origins out (fetchSite has the split mode's rule).
func sameSite(r *http.Request) error {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site":
		return csrfFailed("cross-site requests are not allowed")
	case "same-site":
		return csrfFailed("requests from another host are not allowed")
	}
	return nil
}

func changes(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead
}

// jsonBody requires a JSON content type on a request with a body: an HTML form on another site
// cannot send one without a CORS preflight, which the dashboard never answers.
func jsonBody(r *http.Request) error {
	if r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		return nil
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return refuse(http.StatusUnsupportedMediaType, "send the body as application/json")
	}
	return nil
}

// dash registers an authenticated dashboard route: a live session cookie, and on every request
// that changes something the X-CSRF-Token header matching that session.
func (s *Server) dash(method, path string, h dashHandler) {
	pattern := method + " " + dashPrefix + path
	s.dashRoutes = append(s.dashRoutes, pattern)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		dashHeaders(w)
		if err := s.dashAuth(w, r, h); err != nil {
			s.writeError(w, r, err)
		}
	})
}

// DashboardRoutes lists the pattern of every signed-in dashboard route.
func (s *Server) DashboardRoutes() []string { return append([]string(nil), s.dashRoutes...) }

func (s *Server) dashAuth(w http.ResponseWriter, r *http.Request, h dashHandler) error {
	if err := s.fetchSite(r); err != nil {
		return err
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return &service.Error{Kind: service.KindUnauthorized, Code: service.CodeUnauthenticated, Message: "Sign in to continue."}
	}
	ds, err := s.svc.DashboardSession(r.Context(), c.Value)
	if err != nil {
		if service.AsError(err).Code == service.CodeUnauthenticated {
			clearSessionCookie(w, r)
		}
		return err
	}
	if !ds.User.EmailVerified && !openBeforeVerification(r) {
		return service.EmailUnverified()
	}
	if changes(r) {
		if !dashauth.EqualTokens(r.Header.Get(CSRFHeader), ds.CSRFToken) {
			return csrfFailed("the X-CSRF-Token header is missing or does not match this session")
		}
		if err := jsonBody(r); err != nil {
			return err
		}
	}
	return h(w, r, ds)
}

// openBeforeVerification lists what a user who has not confirmed the email may do: read who
// they are and sign out. Confirming and resending the code are public routes.
func openBeforeVerification(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET " + dashPrefix + "/me", "POST " + dashPrefix + "/auth/logout":
		return true
	}
	return false
}

// localHost reports whether the request is addressed to this machine, where the session cookie
// may go without Secure (plain http during development).
func localHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: token, Path: dashPrefix, Expires: expires,
		MaxAge: int(service.DashboardLifetime / time.Second), HttpOnly: true, Secure: !localHost(r), SameSite: http.SameSiteLaxMode})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: dashPrefix, MaxAge: -1, HttpOnly: true,
		Secure: !localHost(r), SameSite: http.SameSiteLaxMode})
}

type dashUserView struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	TOTPEnabled   bool   `json:"totp_enabled"`
	EmailVerified bool   `json:"email_verified"`
	// RecoveryCodesLeft counts the unused recovery codes; TOTPRemovalAt is the time of a pending
	// removal of 2FA; RecoveryLockedUntil is the end of the lock after one (null when none).
	RecoveryCodesLeft   int        `json:"recovery_codes_left"`
	TOTPRemovalAt       *time.Time `json:"totp_removal_at"`
	RecoveryLockedUntil *time.Time `json:"recovery_locked_until"`
}

func userView(u service.DashUser) dashUserView {
	return dashUserView{ID: u.ID, Email: u.Email, TOTPEnabled: u.TOTPEnabled, EmailVerified: u.EmailVerified,
		RecoveryCodesLeft: u.RecoveryCodesLeft, TOTPRemovalAt: u.TOTPRemovalAt, RecoveryLockedUntil: u.RecoveryLockedUntil}
}

// dashMode reads the required mode query parameter.
func dashMode(r *http.Request) (bool, error) {
	switch r.URL.Query().Get("mode") {
	case "live":
		return true, nil
	case "test":
		return false, nil
	}
	return false, badRequest("mode must be live or test")
}

// dashAuthRoutes are the signed-in account routes. Those that check a code take a token of the
// user's step-up bucket first, against guessing.
type dashAuthRoutes struct {
	s       *Server
	limiter *publicLimiter
}

func (s *Server) mountDashboardAuth() {
	limiter := s.publicLimiter()
	s.mux.HandleFunc("POST "+dashPrefix+"/auth/login", func(w http.ResponseWriter, r *http.Request) {
		dashHeaders(w)
		if err := s.login(w, r, limiter); err != nil {
			s.writeError(w, r, err)
		}
	})
	s.mountSelfServe(limiter)
	h := dashAuthRoutes{s: s, limiter: limiter}
	s.dash("POST", "/auth/logout", h.logout)
	s.dash("GET", "/me", h.me)
	// Unlock (D61): a right code opens the session's confirmation window without an action. It
	// takes a code like any Live action, so it takes their per-user bucket too.
	s.dash("POST", "/confirmation_window", h.unlock)
	// Lock now (D61): ends the session's confirmation window; nothing open is fine.
	s.dash("DELETE", "/confirmation_window", h.lock)
	// Emailed codes that confirm a sensitive action (D58). The Live actions take ?mode=live;
	// totp_enroll takes no mode. Each request emails the owner, so it takes the per-email bucket
	// of the other emailing routes; the service also sends one code per action a minute at most.
	s.dash("POST", "/confirmations", h.sendConfirmation)
	s.dash("POST", "/auth/totp/enroll", h.enrollTOTP)
	s.dash("POST", "/auth/totp/verify", h.verifyTOTP)
	s.dash("POST", "/auth/totp/disable", h.disableTOTP)
	s.dash("POST", "/auth/password/change", h.changePassword)
	s.dash("POST", "/auth/totp/recovery_codes", h.recoveryCodes)
	// Body ignored: the session is the whole request.
	s.dash("POST", "/auth/totp/removal/cancel", h.cancelTOTPRemoval)
}

// stepUpBucket takes a token of the user's step-up bucket.
func (h dashAuthRoutes) stepUpBucket(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	return h.limiter.allowKey(w, r, RouteDashboardTOTP, ds.User.ID)
}

// currentUser answers {"user": ...} with the user as stored now, after a change to it.
func (h dashAuthRoutes) currentUser(w http.ResponseWriter, r *http.Request, ds service.DashSession, extra map[string]any) error {
	u, err := h.s.svc.CurrentDashUser(r.Context(), ds.User.ID)
	if err != nil {
		return err
	}
	out := map[string]any{"user": userView(u)}
	for k, v := range extra {
		out[k] = v
	}
	return writeJSON(w, http.StatusOK, out)
}

func (h dashAuthRoutes) logout(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.s.svc.Logout(r.Context(), ds); err != nil {
		return err
	}
	clearSessionCookie(w, r)
	return writeJSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

func (h dashAuthRoutes) me(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	hasLiveWallet, err := h.s.svc.HasLiveWallet(r.Context(), ds.User.MerchantID)
	if err != nil {
		return err
	}
	pauses, err := h.s.svc.DashChainPauses(r.Context(), ds.User.MerchantID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"user": userView(ds.User),
		"merchant": map[string]any{"id": ds.User.MerchantID, "name": ds.User.MerchantName, "live_enabled": ds.User.LiveEnabled,
			"has_live_wallet": hasLiveWallet, "logo_url": ds.User.LogoURL()},
		"csrf_token": ds.CSRFToken,
		// The session's open confirmation window (D59), null when a covered Live action needs a code.
		"confirmation_window": ds.ConfirmWindow,
		// How long Unlock opens the window for (D61); 0 when the window is off on this server.
		"confirmation_window_seconds": h.s.svc.ConfirmWindowSeconds(),
		// The chain and asset pauses that affect this merchant, both modes (D64); empty when none.
		"chain_pauses": pauses,
	})
}

// liveMode reads the mode parameter; Live is refused while the merchant's live mode is closed.
func liveMode(r *http.Request, ds service.DashSession) (bool, error) {
	live, err := dashMode(r)
	if err != nil {
		return false, err
	}
	if live && !ds.User.LiveEnabled {
		return false, service.LiveReviewPending()
	}
	return live, nil
}

func (h dashAuthRoutes) unlock(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.stepUpBucket(w, r, ds); err != nil {
		return err
	}
	live, err := liveMode(r, ds)
	if err != nil {
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
	win, err := h.s.svc.Unlock(r.Context(), ds.User, live, proof)
	return respond(w, map[string]any{"confirmation_window": win}, err)
}

func (h dashAuthRoutes) lock(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	err := h.s.svc.Lock(r.Context(), ds.User)
	return respond(w, map[string]any{"confirmation_window": nil}, err)
}

func (h dashAuthRoutes) sendConfirmation(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.limiter.allowKey(w, r, RouteDashboardConfirm, ds.User.ID); err != nil {
		return err
	}
	m, err := readFields(r, "purpose")
	if err != nil {
		return err
	}
	purpose, err := decodeField[string](m, "purpose", "a string")
	if err != nil {
		return err
	}
	if purpose == nil {
		return badParam("purpose", "purpose is required: wallet_create, wallet_archive, api_key, webhook_endpoint, unlock or totp_enroll")
	}
	live := false
	if *purpose != service.ConfirmTOTPEnroll {
		if live, err = liveMode(r, ds); err != nil {
			return err
		}
	}
	sent, err := h.s.svc.SendConfirmationCode(r.Context(), ds.User, live, *purpose)
	return respond(w, sent, err)
}

func (h dashAuthRoutes) enrollTOTP(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	// A code is checked here, so this shares the step-up bucket against guessing.
	if err := h.stepUpBucket(w, r, ds); err != nil {
		return err
	}
	// Turning 2FA on takes email_code; replacing the authenticator takes code or
	// recovery_code. password is accepted and ignored (D58).
	m, err := readFields(r, "email_code", "code", "recovery_code", "password")
	if err != nil {
		return err
	}
	d := &members{m: m}
	emailCode := member[string](d, "email_code", "a string")
	code := member[string](d, "code", "a string")
	recovery := member[string](d, "recovery_code", "a string")
	if d.err != nil {
		return d.err
	}
	e, err := h.s.svc.EnrollTOTP(r.Context(), ds, emailCode.Ptr(), code.Ptr(), recovery.Ptr())
	return respond(w, e, err)
}

func (h dashAuthRoutes) verifyTOTP(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.stepUpBucket(w, r, ds); err != nil {
		return err
	}
	m, err := readFields(r, "code")
	if err != nil {
		return err
	}
	code, err := decodeField[string](m, "code", "a string")
	if err != nil {
		return err
	}
	if code == nil {
		return badRequest("code is required")
	}
	codes, err := h.s.svc.VerifyTOTP(r.Context(), ds, *code)
	if err != nil {
		return err
	}
	var extra map[string]any
	if codes != nil {
		extra = map[string]any{"recovery_codes": codes}
	}
	return h.currentUser(w, r, ds, extra)
}

// codeOrRecovery reads the code and recovery_code members of a body that may also hold password
// (accepted and ignored, D58).
func codeOrRecovery(r *http.Request) (code, recovery *string, err error) {
	m, err := readFields(r, "code", "recovery_code", "password")
	if err != nil {
		return nil, nil, err
	}
	d := &members{m: m}
	c := member[string](d, "code", "a string")
	rc := member[string](d, "recovery_code", "a string")
	return c.Ptr(), rc.Ptr(), d.err
}

func (h dashAuthRoutes) disableTOTP(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.stepUpBucket(w, r, ds); err != nil {
		return err
	}
	code, recovery, err := codeOrRecovery(r)
	if err != nil {
		return err
	}
	if code == nil && recovery == nil {
		return badRequest("code or recovery_code is required")
	}
	if err := h.s.svc.DisableTOTP(r.Context(), ds, code, recovery); err != nil {
		return err
	}
	return h.currentUser(w, r, ds, nil)
}

func (h dashAuthRoutes) changePassword(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.stepUpBucket(w, r, ds); err != nil {
		return err
	}
	m, err := readFields(r, "current_password", "new_password", "totp")
	if err != nil {
		return err
	}
	d := &members{m: m}
	current := member[string](d, "current_password", "a string")
	next := member[string](d, "new_password", "a string")
	totp := member[string](d, "totp", "a string")
	if d.err != nil {
		return d.err
	}
	if !current.Given() || !next.Given() {
		return badRequest("current_password and new_password are required")
	}
	if err := h.s.svc.ChangePassword(r.Context(), ds, current.Value, next.Value, totp.Ptr()); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h dashAuthRoutes) recoveryCodes(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.stepUpBucket(w, r, ds); err != nil {
		return err
	}
	code, recovery, err := codeOrRecovery(r)
	if err != nil {
		return err
	}
	codes, err := h.s.svc.CreateRecoveryCodes(r.Context(), ds.User, code, recovery)
	return respond(w, map[string]any{"recovery_codes": codes}, err)
}

func (h dashAuthRoutes) cancelTOTPRemoval(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	if err := h.s.svc.CancelTOTPRemoval(r.Context(), ds.User); err != nil {
		return err
	}
	return h.currentUser(w, r, ds, nil)
}

func readFields(r *http.Request, allowed ...string) (map[string]json.RawMessage, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	return fields(body, allowed...)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, limiter *publicLimiter) error {
	if err := s.fetchSite(r); err != nil {
		return err
	}
	if err := jsonBody(r); err != nil {
		return err
	}
	if err := limiter.allow(w, r, RouteDashboardLogin); err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	m, err := fields(body, "email", "password", "totp", "recovery_code")
	if err != nil {
		return err
	}
	d := &members{m: m}
	email := member[string](d, "email", "a string")
	pw := member[string](d, "password", "a string")
	totp := member[string](d, "totp", "a string")
	recovery := member[string](d, "recovery_code", "a string")
	if d.err != nil {
		return d.err
	}
	if !email.Given() || !pw.Given() {
		return badRequest("email and password are required")
	}
	// A tight bucket per account and client IP, then a loose one per account against one password
	// being guessed from many addresses. Neither bucket alone lets one address hold the owner's
	// sign-in at 429: it drains only its own tight bucket. Hashed: the bucket key has a bounded
	// length and the table holds no emails.
	account := emailBucket(email.Value)
	if err := limiter.allowKey(w, r, RouteDashboardLoginEmailIP, account+"|"+clientIP(r, limiter.header)); err != nil {
		return err
	}
	if err := limiter.allowKey(w, r, RouteDashboardLoginEmail, account); err != nil {
		return err
	}
	var previous string
	if c, err := r.Cookie(SessionCookie); err == nil {
		previous = c.Value
	}
	// A null or absent totp or recovery_code is sent as "", which the service reads as none.
	ds, token, err := s.svc.Login(r.Context(), email.Value, pw.Value, totp.Value, recovery.Value, previous)
	if err != nil {
		return err
	}
	setSessionCookie(w, r, token, ds.ExpiresAt)
	out := map[string]any{"user": userView(ds.User), "csrf_token": ds.CSRFToken}
	if ds.UsedRecoveryCode {
		out["used_recovery_code"] = true
	}
	return writeJSON(w, http.StatusOK, out)
}
