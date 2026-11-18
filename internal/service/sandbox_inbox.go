package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

const (
	// InboxRetention is how long an inbox keeps a request.
	InboxRetention = 7 * 24 * time.Hour
	// InboxMaxRequests is how many requests an inbox keeps; older ones are dropped first.
	InboxMaxRequests = 500
	// InboxMaxBodyBytes is the largest request body an inbox accepts (413 above it).
	InboxMaxBodyBytes = 64 << 10
	inboxMaxHeaders   = 64
	inboxMaxHeaderLen = 4 << 10
	// InboxPath is where inboxes receive, followed by the token.
	InboxPath = "/v1/sandbox/inbox/"
)

// Signature check outcomes of a received request.
const (
	SigMissing   = "missing"
	SigMalformed = "malformed"
	SigNoMatch   = "no_match"
	SigTimestamp = "timestamp_outside_tolerance"
)

// SandboxInboxView is a merchant's sandbox webhook inbox as the dashboard shows it.
type SandboxInboxView struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	EndpointID    string `json:"endpoint_id"`
	CreatedAt     string `json:"created_at"`
	RetentionDays int    `json:"retention_days"`
	MaxRequests   int    `json:"max_requests"`
	MaxBodyBytes  int    `json:"max_body_bytes"`
	// Secret is the endpoint's signing secret, present only in the response that created it.
	Secret string `json:"secret,omitempty"`
}

func inboxView(i store.SandboxInbox, secret string) SandboxInboxView {
	return SandboxInboxView{ID: i.ID, URL: i.Endpoint.URL, EndpointID: i.Endpoint.ID, CreatedAt: rfc3339(i.CreatedAt),
		RetentionDays: int(InboxRetention / (24 * time.Hour)), MaxRequests: InboxMaxRequests, MaxBodyBytes: InboxMaxBodyBytes, Secret: secret}
}

func inboxNotFound() *Error {
	return newErr(KindNotFound, CodeNotFound, "no sandbox inbox: create one first")
}

func (s *Service) inboxURL(token string) string {
	return s.inboxBase() + token
}

func (s *Service) inboxBase() string {
	base := s.cfg.APIBaseURL
	if base == "" {
		base = s.cfg.CheckoutBaseURL
	}
	return strings.TrimRight(base, "/") + InboxPath
}

// SandboxInbox returns the merchant's active inbox, or nil.
func (s *Service) SandboxInbox(ctx context.Context, a Auth) (*SandboxInboxView, error) {
	if err := RequireTestMode(a); err != nil {
		return nil, err
	}
	i, err := s.store.ActiveSandboxInbox(ctx, a.MerchantID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := inboxView(i, "")
	return &v, nil
}

// CreateSandboxInbox gives the merchant a hosted test endpoint, registered as a test-mode
// webhook endpoint for every event type. It is idempotent: an active inbox is returned as is
// (without its secret).
func (s *Service) CreateSandboxInbox(ctx context.Context, a Auth) (SandboxInboxView, error) {
	if err := RequireTestMode(a); err != nil {
		return SandboxInboxView{}, err
	}
	for range 2 {
		cur, err := s.SandboxInbox(ctx, a)
		if err != nil || cur != nil {
			if cur != nil {
				return *cur, nil
			}
			return SandboxInboxView{}, err
		}
		var tok [32]byte
		if _, err := rand.Read(tok[:]); err != nil {
			return SandboxInboxView{}, err
		}
		secret, err := newSecret()
		if err != nil {
			return SandboxInboxView{}, err
		}
		now := s.store.Now()
		in := store.SandboxInbox{ID: ids.NewAt(ids.SandboxInbox, now), MerchantID: a.MerchantID, Token: base58.Encode(tok[:])}
		in.Endpoint = store.WebhookEndpoint{ID: ids.NewAt(ids.WebhookEndpoint, now), MerchantID: a.MerchantID, URL: s.inboxURL(in.Token)}
		if in.Endpoint.SecretSealed, err = s.sealSecret(in.Endpoint.ID, secret); err != nil {
			return SandboxInboxView{}, err
		}
		created, err := s.store.CreateSandboxInbox(ctx, in, a.actor())
		if errors.Is(err, store.ErrInboxExists) {
			continue
		}
		if err != nil {
			return SandboxInboxView{}, err
		}
		return inboxView(created, secret), nil
	}
	return SandboxInboxView{}, newErr(KindConflict, CodeInvalidRequest, "the inbox changed while it was created; try again")
}

// DeleteSandboxInbox closes the inbox, deletes its endpoint and its requests.
func (s *Service) DeleteSandboxInbox(ctx context.Context, a Auth) error {
	if err := RequireTestMode(a); err != nil {
		return err
	}
	err := s.store.DisableSandboxInbox(ctx, a.MerchantID, a.actor())
	if errors.Is(err, store.ErrNotFound) {
		return inboxNotFound()
	}
	return err
}

// SignatureCheck is the result of verifying a captured request's webhook signature.
type SignatureCheck struct {
	Valid bool `json:"valid"`
	// Error is missing, malformed, no_match or timestamp_outside_tolerance.
	Error     *string `json:"error"`
	Timestamp *string `json:"timestamp"`
	// SkewSeconds is the arrival time minus the signed timestamp.
	SkewSeconds *int64 `json:"skew_seconds"`
}

// HeaderView is one HTTP header of a captured request.
type HeaderView struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SandboxRequestView is one request the sandbox inbox captured, with its headers, body and
// signature check.
type SandboxRequestView struct {
	ID         string         `json:"id"`
	ReceivedAt string         `json:"received_at"`
	Method     string         `json:"method"`
	Headers    []HeaderView   `json:"headers"`
	Body       string         `json:"body"`
	Size       int            `json:"size"`
	EventID    *string        `json:"event_id"`
	EventType  *string        `json:"event_type"`
	Signature  SignatureCheck `json:"signature"`
}

// SandboxRequestList is one page of captured sandbox requests and the cursor of the next page, if
// any.
type SandboxRequestList struct {
	Data       []SandboxRequestView `json:"data"`
	NextCursor *string              `json:"next_cursor"`
}

func requestView(r store.SandboxRequest) SandboxRequestView {
	v := SandboxRequestView{ID: r.ID, ReceivedAt: r.ReceivedAt.UTC().Format(time.RFC3339Nano), Method: r.Method, Headers: []HeaderView{},
		Body: strings.ToValidUTF8(string(r.Body), string(utf8.RuneError)), Size: len(r.Body), Signature: SignatureCheck{Valid: r.SignatureValid}}
	for _, h := range r.Headers {
		v.Headers = append(v.Headers, HeaderView{Name: h[0], Value: h[1]})
	}
	if r.SignatureError != "" {
		e := r.SignatureError
		v.Signature.Error = &e
	}
	if r.SignatureTimestamp != nil {
		ts := rfc3339(*r.SignatureTimestamp)
		skew := int64(r.ReceivedAt.Sub(*r.SignatureTimestamp) / time.Second)
		v.Signature.Timestamp, v.Signature.SkewSeconds = &ts, &skew
	}
	var env struct {
		ID   *string `json:"id"`
		Type *string `json:"type"`
	}
	if json.Unmarshal(r.Body, &env) == nil {
		v.EventID, v.EventType = env.ID, env.Type
	}
	return v
}

// SandboxRequests pages the inbox's requests, newest first, within the retention window.
func (s *Service) SandboxRequests(ctx context.Context, a Auth, cursor string, limit int) (SandboxRequestList, error) {
	if err := RequireTestMode(a); err != nil {
		return SandboxRequestList{}, err
	}
	var before int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n <= 0 {
			return SandboxRequestList{}, invalid("cursor is not valid")
		}
		before = n
	}
	i, err := s.store.ActiveSandboxInbox(ctx, a.MerchantID)
	if errors.Is(err, store.ErrNotFound) {
		return SandboxRequestList{}, inboxNotFound()
	}
	if err != nil {
		return SandboxRequestList{}, err
	}
	list, more, err := s.store.ListSandboxRequests(ctx, i.ID, before, s.store.Now().Add(-InboxRetention), limit)
	if err != nil {
		return SandboxRequestList{}, err
	}
	out := SandboxRequestList{Data: []SandboxRequestView{}}
	for _, r := range list {
		out.Data = append(out.Data, requestView(r))
	}
	if more && len(list) > 0 {
		next := strconv.FormatInt(list[len(list)-1].Seq, 10)
		out.NextCursor = &next
	}
	return out, nil
}

// LookupSandboxInbox finds the active inbox a public request is addressed to.
func (s *Service) LookupSandboxInbox(ctx context.Context, token string) (store.SandboxInbox, error) {
	if token == "" || len(token) > 64 {
		return store.SandboxInbox{}, newErr(KindNotFound, CodeNotFound, "no such inbox")
	}
	i, err := s.store.SandboxInboxByToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return store.SandboxInbox{}, newErr(KindNotFound, CodeNotFound, "no such inbox")
	}
	return i, err
}

// ReceiveSandboxRequest stores a request an inbox received, with its signature checked now
// against the secrets its endpoint signs with at this moment.
func (s *Service) ReceiveSandboxRequest(ctx context.Context, i store.SandboxInbox, method string, header map[string][]string, body []byte) (string, error) {
	now := s.store.Now()
	r := store.SandboxRequest{ID: ids.NewAt(ids.SandboxRequest, now), InboxID: i.ID, ReceivedAt: now, Method: method, Body: body}
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range header[name] {
			if len(r.Headers) == inboxMaxHeaders {
				break
			}
			r.Headers = append(r.Headers, [2]string{name, clip(v, inboxMaxHeaderLen)})
		}
	}
	secrets, err := s.SigningSecrets(i.Endpoint, now)
	if err != nil {
		return "", err
	}
	check := checkSignature(body, textproto.MIMEHeader(header).Get(webhooksig.HeaderName), secrets, now)
	r.SignatureValid, r.SignatureError, r.SignatureTimestamp = check.err == "", check.err, check.ts
	if err := s.store.InsertSandboxRequest(ctx, r, InboxMaxRequests, InboxRetention); err != nil {
		return "", err
	}
	return r.ID, nil
}

type sigOutcome struct {
	err string
	ts  *time.Time
}

func checkSignature(body []byte, header string, secrets []string, now time.Time) sigOutcome {
	if strings.TrimSpace(header) == "" {
		return sigOutcome{err: SigMissing}
	}
	t, sigs, err := webhooksig.Parse(header)
	if err != nil {
		return sigOutcome{err: SigMalformed}
	}
	ts := time.Unix(t, 0).UTC()
	out := sigOutcome{ts: &ts, err: SigNoMatch}
	for _, sec := range secrets {
		if webhooksig.Matches(body, t, sigs, sec) {
			out.err = ""
			break
		}
	}
	if skew := now.Sub(ts); out.err == "" && (skew > webhooksig.DefaultTolerance || skew < -webhooksig.DefaultTolerance) {
		out.err = SigTimestamp
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// PurgeSandbox deletes inbox requests past the retention window, in batches.
func (s *Service) PurgeSandbox(ctx context.Context) (int, error) {
	return s.store.PurgeSandboxRequests(ctx, s.store.Now().Add(-InboxRetention), 1000)
}
