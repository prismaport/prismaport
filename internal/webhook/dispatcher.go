// Package webhook delivers outbox events to merchant endpoints: signed, retried with
// exponential backoff, dead-lettered after a bounded number of attempts, at least once.
package webhook

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/netguard"
	"github.com/prismaport/prismaport/internal/safe"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

// concurrency is how many endpoints are sent to at once. Each endpoint gets one request at a
// time, in claim order, so a slow endpoint holds up only its own deliveries.
const concurrency = 10

// Config tunes webhook delivery: attempts, backoff, timeout, lease and batch size; zero values get
// defaults.
type Config struct {
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Timeout     time.Duration
	// Lease is how long a claimed batch stays invisible to other workers. It is raised to cover
	// the batch's worst case (every delivery hitting Timeout and its outcome write taking
	// recordMargin, one after another): see leaseBudget.
	Lease time.Duration
	Batch int
	// AllowPrivateNetworks permits deliveries to loopback and private addresses (local runs
	// and tests only).
	AllowPrivateNetworks bool
	// Alerts receives webhook_dead_lettered for every delivery that runs out of attempts
	// (default: the log).
	Alerts alert.Sink
}

func (c *Config) defaults() {
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 12
	}
	if c.BaseBackoff == 0 {
		c.BaseBackoff = 30 * time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 6 * time.Hour
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Batch == 0 {
		c.Batch = 10
	}
	if budget := leaseBudget(c.Batch, c.Timeout); c.Lease < budget {
		c.Lease = budget
	}
}

// leaseBudget is the longest a claimed group can take: its deliveries go one after another, each
// bounded by the request timeout plus recordMargin (attempt), and leaseMargin on top for the
// claim itself and scheduling. Every job gets its own margin, so a big batch of slow endpoints
// never outlives its lease and gets claimed again by another events worker mid-send.
func leaseBudget(batch int, timeout time.Duration) time.Duration {
	return time.Duration(batch)*(timeout+recordMargin) + leaseMargin
}

// leaseMargin is the slack a whole claimed group gets on top of its per-job budget.
const leaseMargin = 30 * time.Second

// Dispatcher delivers queued webhook events to merchant endpoints and records each attempt.
type Dispatcher struct {
	svc    *service.Service
	st     *store.Store
	cfg    Config
	client *http.Client
	log    *slog.Logger
	jitter func(time.Duration) time.Duration

	// claimMu serializes claims, so an endpoint is never claimed for twice at once.
	claimMu sync.Mutex
	mu      sync.Mutex
	// busy holds the endpoints being sent to; idle is signalled when it empties.
	busy map[string]bool
	idle *sync.Cond
	// err joins the errors of background deliveries, returned by the next Start.
	err error
}

// NewDispatcher returns a dispatcher, filling unset config fields with their defaults.
func NewDispatcher(svc *service.Service, st *store.Store, cfg Config, log *slog.Logger) *Dispatcher {
	cfg.defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Alerts == nil {
		cfg.Alerts = alert.Log{Logger: log}
	}
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	if !cfg.AllowPrivateNetworks {
		dialer.Control = publicOnly
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
		MaxIdleConnsPerHost:   4,
	}
	client := &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
		// A redirect is treated as a failed delivery; following it would bypass URL validation.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	d := &Dispatcher{svc: svc, st: st, cfg: cfg, client: client, log: log, jitter: defaultJitter, busy: map[string]bool{}}
	d.idle = sync.NewCond(&d.mu)
	return d
}

func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - d/10 + time.Duration(rand.Int64N(int64(d/5)+1))
}

// Backoff is the wait after the given failed attempt (1-based): base * 2^(n-1), capped.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return min(d, max)
}

var errPrivateAddress = errors.New("webhook: refusing to connect to a non-public address")

// publicOnly runs on the resolved address of every connection, so a name that resolves to a
// non-public address is refused too.
func publicOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !netguard.Public(ap.Addr()) {
		return errPrivateAddress
	}
	return nil
}

// RunOnce claims due deliveries and attempts each once, the endpoints in parallel, and waits for
// them. It returns how many it attempted.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	groups, n, err := d.claim(ctx)
	if err != nil {
		return 0, err
	}
	errs := make([]error, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Go(func() { errs[i] = d.run(ctx, g) })
	}
	wg.Wait()
	return n, errors.Join(errs...)
}

// Start claims due deliveries for the endpoints not being sent to and attempts each once in the
// background, the worker's loop step: a slow endpoint never delays the next claim, so it holds
// up no other endpoint. It returns how many it claimed (0 when every slot is busy), and the error
// of a delivery started earlier. Wait waits for what it started.
func (d *Dispatcher) Start(ctx context.Context) (int, error) {
	prev := d.takeErr()
	groups, n, err := d.claim(ctx)
	for _, g := range groups {
		go func() { d.keepErr(d.run(ctx, g)) }()
	}
	return n, errors.Join(prev, err)
}

// keepErr keeps a background delivery's error for the next Start; every one is kept.
func (d *Dispatcher) keepErr(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	d.err = errors.Join(d.err, err)
	d.mu.Unlock()
}

// takeErr returns the kept errors and forgets them.
func (d *Dispatcher) takeErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.err
	d.err = nil
	return err
}

// Wait waits until no endpoint is being sent to.
func (d *Dispatcher) Wait() {
	d.mu.Lock()
	for len(d.busy) > 0 {
		d.idle.Wait()
	}
	d.mu.Unlock()
}

// claim leases due deliveries of endpoints not being sent to, at most one per free slot, and
// marks their endpoints busy. Each group is one endpoint's jobs in claim order. The lease still
// covers a group sent one after another, as a group is at most Batch jobs.
func (d *Dispatcher) claim(ctx context.Context) (groups [][]store.DeliveryJob, n int, err error) {
	d.claimMu.Lock()
	defer d.claimMu.Unlock()
	d.mu.Lock()
	free := concurrency - len(d.busy)
	skip := make([]string, 0, len(d.busy))
	for id := range d.busy {
		skip = append(skip, id)
	}
	d.mu.Unlock()
	if free <= 0 {
		return nil, 0, nil
	}
	jobs, err := d.st.ClaimDueDeliveries(ctx, min(d.cfg.Batch, free), d.cfg.Lease, skip...)
	if err != nil {
		return nil, 0, err
	}
	at := map[string]int{}
	for _, j := range jobs {
		i, ok := at[j.Endpoint.ID]
		if !ok {
			i = len(groups)
			at[j.Endpoint.ID] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], j)
	}
	d.mu.Lock()
	for id := range at {
		d.busy[id] = true
	}
	d.mu.Unlock()
	return groups, len(jobs), nil
}

// run sends one endpoint's jobs one after another, then frees the endpoint. An error recording
// an outcome stops the rest, whose leases lapse into a retry. A delivery that panics is alerted
// and left to its lease; the others go on.
func (d *Dispatcher) run(ctx context.Context, jobs []store.DeliveryJob) (err error) {
	defer func() {
		d.mu.Lock()
		delete(d.busy, jobs[0].Endpoint.ID)
		if len(d.busy) == 0 {
			d.idle.Broadcast()
		}
		d.mu.Unlock()
	}()
	for _, j := range jobs {
		if ctx.Err() != nil {
			// Shutting down: the rest of the group was never sent. Its lease lapses and the next
			// claim sends it.
			return err
		}
		e := safe.Call(func() error { return d.attempt(ctx, j) })
		if p, ok := e.(*safe.PanicError); ok {
			safe.Report(ctx, d.cfg.Alerts, d.log, "webhooks", p)
			err = cmp.Or(err, e)
			continue
		}
		if e != nil {
			return e
		}
	}
	return err
}

// recordMargin is how long an attempt may take past the request timeout to write its outcome.
const recordMargin = 10 * time.Second

// attempt delivers j once. An attempt in flight is finished even when ctx ends (shutdown):
// cutting its POST would also fail the outcome's write, and the endpoint, which may have got the
// event, would get it again once the lease lapsed. It is bounded by the request timeout plus
// recordMargin instead; Wait waits for it.
func (d *Dispatcher) attempt(ctx context.Context, j store.DeliveryJob) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.Timeout+recordMargin)
	defer cancel()
	return d.deliver(actx, j)
}

func (d *Dispatcher) deliver(ctx context.Context, j store.DeliveryJob) error {
	st := d.st
	now := st.Now()
	attempts := j.Attempts + 1
	code, sendErr := d.send(ctx, j, now)
	if sendErr == nil && code >= 200 && code < 300 {
		return d.record(ctx, j, attempts, store.DeliveryOutcome{StatusCode: code, Succeeded: true, StartedAt: now}, "")
	}
	msg := fmt.Sprintf("HTTP %d", code)
	cause := msg
	if sendErr != nil {
		msg, cause = errorText(sendErr), sendErr.Error()
		// The merchant sees the error on its own delivery (errorText); the alert leaves out
		// the endpoint URL, which may carry the merchant's tokens.
		var ue *url.Error
		if errors.As(sendErr, &ue) {
			cause = ue.Err.Error()
		}
	}
	out := store.DeliveryOutcome{StatusCode: code, Err: truncate(msg, 500), StartedAt: now, Reason: FailureReason(code, sendErr)}
	if attempts < d.cfg.MaxAttempts {
		next := now.Add(d.jitter(Backoff(attempts, d.cfg.BaseBackoff, d.cfg.MaxBackoff)))
		out.NextAttemptAt = &next
	}
	return d.record(ctx, j, attempts, out, truncate(cause, 500))
}

func (d *Dispatcher) record(ctx context.Context, j store.DeliveryJob, attempts int, out store.DeliveryOutcome, cause string) error {
	ok, err := d.st.RecordDeliveryAttempt(ctx, j, attempts, out)
	switch {
	case err != nil:
		return err
	case !ok:
		d.log.Info("webhook outcome dropped: the delivery was claimed again, resent or its endpoint deleted",
			"delivery", j.DeliveryID, "endpoint", j.Endpoint.ID, "event", j.EventID)
	case !out.Succeeded && out.NextAttemptAt == nil:
		d.cfg.Alerts.Alert(ctx, alert.Alert{Severity: alert.Warning, Kind: "webhook_dead_lettered",
			Message: fmt.Sprintf("a webhook delivery failed %d attempts and will not be retried", attempts),
			Fields: map[string]string{"merchant": j.Endpoint.MerchantID, "endpoint": j.Endpoint.ID, "delivery": j.DeliveryID,
				"event": j.EventID, "livemode": fmt.Sprint(j.Endpoint.Livemode), "err": cause}})
	}
	return nil
}

// refusedURL is a delivery the URL rules refused before any connection.
type refusedURL struct{ error }

func (e refusedURL) Unwrap() error { return e.error }

func (d *Dispatcher) send(ctx context.Context, j store.DeliveryJob, now time.Time) (int, error) {
	if err := d.svc.DeliverableURL(j.Endpoint); err != nil {
		return 0, refusedURL{err}
	}
	secrets, err := d.svc.SigningSecrets(j.Endpoint, now)
	if err != nil {
		return 0, fmt.Errorf("signing secret unavailable: %w", err)
	}
	req, err := NewDeliveryRequest(ctx, j.Endpoint.URL, j.EventID, []byte(j.Payload), now, secrets...)
	if err != nil {
		return 0, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// FailureReason names a failed attempt for the failing-endpoint emails, from a fixed list:
// "redirect" for a 3xx, "" for any other HTTP answer (its status code says it), else
// "refused address", "timeout", "connection refused", "TLS" or "connection failed". It is made
// from our own error values only, never from text the target sent.
func FailureReason(code int, err error) string {
	var refused refusedURL
	var netErr net.Error
	var tlsAlert tls.AlertError
	var tlsRecord tls.RecordHeaderError
	var certInvalid x509.CertificateInvalidError
	var certAuthority x509.UnknownAuthorityError
	var certHost x509.HostnameError
	var certVerify *tls.CertificateVerificationError
	switch {
	case err == nil && code >= 300 && code < 400:
		return "redirect"
	case err == nil:
		return ""
	case errors.As(err, &refused) || errors.Is(err, errPrivateAddress):
		return "refused address"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED) || connectionRefused(err):
		return "connection refused"
	case errors.As(err, &tlsAlert) || errors.As(err, &tlsRecord) || errors.As(err, &certInvalid) || errors.As(err, &certAuthority) ||
		errors.As(err, &certHost) || errors.As(err, &certVerify):
		return "TLS"
	}
	return "connection failed"
}

// errorText is the error a merchant sees on a failed attempt: its delivery's last error and the
// test-send result. A refused address or a failed name lookup gets a fixed text that starts with
// the FailureReason: the raw error names the address the endpoint resolved to, or our resolver,
// which belong to our network. Any other error is shown whole.
func errorText(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, errPrivateAddress):
		return FailureReason(0, err) + ": the endpoint resolves to a non-public address"
	case errors.As(err, &dnsErr):
		return FailureReason(0, err) + ": the endpoint host name could not be resolved"
	}
	return err.Error()
}

// connectionRefused catches a refused connection whose errno does not match ECONNREFUSED
// (Windows reports WSAECONNREFUSED).
func connectionRefused(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 10061
}

// DeliveryHeaders are the headers of every delivery, in the order they are listed.
var DeliveryHeaders = []string{"Content-Type", "User-Agent", "Prismaport-Event-Id", webhooksig.HeaderName}

// NewDeliveryRequest builds the POST of one delivery: the body bytes as stored, signed at now
// with each secret. Real deliveries, test events and the CLI forwarder all use it.
func NewDeliveryRequest(ctx context.Context, url, eventID string, body []byte, now time.Time, secrets ...string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Prismaport-Webhooks/1")
	req.Header.Set("Prismaport-Event-Id", eventID)
	req.Header.Set(webhooksig.HeaderName, webhooksig.Header(now, body, secrets...))
	return req, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
