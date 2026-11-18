// Package txjob is the transaction job lifecycle shared by the EVM sender and the Solana
// sender: retries with exponential backoff and alerts, giving up, failing (including a job
// whose stored fields cannot be read), completing, waiting for a sibling flush, and the
// flush-fee reconciliation after a flush settlement. What a send attempt leaves on a job, and
// so what a retry or a failure clears, is the chain's own (Lifecycle.ResetForRetry,
// Lifecycle.ClearOnFail).
package txjob

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/flushfee"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
)

// MaxBackoffShift caps the doubling exponent of Backoff, so the shift cannot overflow a
// Duration before MaxBackoff applies.
const MaxBackoffShift = 20

// DefaultBaseBackoff is the senders' first wait after a failure unless configured.
const DefaultBaseBackoff = 30 * time.Second

// SettleMinedMargin is how long a signed-path (EIP-3009) authorization must stay valid for the
// sender to send its settle job: one sent closer to valid_before may not be mined in time.
const SettleMinedMargin = 30 * time.Second

// SettleSubmitMargin is how long the authorization must stay valid when the payer submits it:
// the sender's own margin, one failed attempt's backoff before the retry, and 30 s for the
// sender's tick and the jobs queued ahead. An authorization accepted with less would fail as
// expired after a single retry.
const SettleSubmitMargin = SettleMinedMargin + DefaultBaseBackoff + 30*time.Second

// JobStore is what a Lifecycle reads and writes of the transaction jobs; *store.Store
// satisfies it.
type JobStore interface {
	AddTxJobDetails(ctx context.Context, id string, extra map[string]string) error
	FlushSiblingInFlight(ctx context.Context, j store.TxJob) (bool, error)
	UpdateTxJob(ctx context.Context, j *store.TxJob) error
	Now() time.Time
}

// Lifecycle moves one chain's transaction jobs between states.
type Lifecycle struct {
	Chain  string
	Store  JobStore
	Alerts alert.Sink
	Log    *slog.Logger
	// BaseBackoff is the first wait after a failure, doubled per attempt up to MaxBackoff; it is
	// also how long a flush waits for a sibling.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// AlertAfter failures raise a critical alert; MaxAttempts give the job up.
	AlertAfter  int
	MaxAttempts int
	// ResetForRetry clears what a send attempt left on a job that is retried.
	ResetForRetry func(j *store.TxJob)
	// ClearOnFail clears what a failed job must not keep; nil keeps everything.
	ClearOnFail func(j *store.TxJob)
	// Sibling names what two flushes of a customer address share, in WaitForSibling's log line.
	Sibling string
}

// Alert raises an alert about the lifecycle's chain.
func (l *Lifecycle) Alert(ctx context.Context, sev alert.Severity, kind, msg string, fields map[string]string) {
	l.Alerts.Alert(ctx, alert.Alert{Severity: sev, Chain: l.Chain, Kind: kind, Message: msg, Fields: fields})
}

// Backoff is the wait before attempt number attempts+1.
func (l *Lifecycle) Backoff(attempts int) time.Duration {
	d := l.BaseBackoff << min(attempts-1, MaxBackoffShift)
	if d > l.MaxBackoff || d <= 0 {
		d = l.MaxBackoff
	}
	return d
}

// Retry records a failure: the job waits with exponential backoff, alerts after AlertAfter
// failures, and is given up after MaxAttempts.
func (l *Lifecycle) Retry(ctx context.Context, j store.TxJob, why string) error {
	j.Attempts++
	j.LastError = why
	j.Status = store.JobPending
	l.ResetForRetry(&j)
	j.NextAttemptAt = l.Store.Now().Add(l.Backoff(j.Attempts))
	fields := map[string]string{"job": j.ID, "kind": j.Kind, "session": j.SessionID, "attempts": fmt.Sprint(j.Attempts), "error": why}
	if j.Attempts >= l.MaxAttempts {
		j.Status, j.Outcome = store.JobFailed, "gave_up"
		l.Alert(ctx, alert.Critical, j.Kind+"_gave_up", "transaction job abandoned after repeated failures", fields)
	} else if j.Attempts >= l.AlertAfter {
		l.Alert(ctx, alert.Critical, j.Kind+"_failing", "transaction job keeps failing", fields)
	}
	l.Log.Warn("tx job retry", "chain", l.Chain, "job", j.ID, "attempts", j.Attempts, "err", why)
	return l.Store.UpdateTxJob(ctx, &j)
}

// Fail ends a job as failed with outcome.
func (l *Lifecycle) Fail(ctx context.Context, j store.TxJob, outcome, why string) error {
	j.Status, j.Outcome, j.LastError = store.JobFailed, outcome, why
	if l.ClearOnFail != nil {
		l.ClearOnFail(&j)
	}
	l.Log.Warn("tx job failed", "chain", l.Chain, "job", j.ID, "outcome", outcome, "err", why)
	return l.Store.UpdateTxJob(ctx, &j)
}

// BadJob fails a job whose stored fields cannot be read, with a critical alert: retrying cannot
// fix it, and nothing is simulated, estimated or sent for it.
func (l *Lifecycle) BadJob(ctx context.Context, j store.TxJob, err error) error {
	l.Alert(ctx, alert.Critical, j.Kind+"_bad_job", "transaction job holds a malformed field; failed without sending",
		map[string]string{"job": j.ID, "session": j.SessionID, "error": err.Error()})
	return l.Fail(ctx, j, "bad_job", err.Error())
}

// Done ends a job as done with outcome.
func (l *Lifecycle) Done(ctx context.Context, j store.TxJob, outcome string) error {
	j.Status, j.Outcome, j.LastError = store.JobDone, outcome, ""
	l.Log.Info("tx job done", "chain", l.Chain, "job", j.ID, "kind", j.Kind, "outcome", outcome)
	return l.Store.UpdateTxJob(ctx, &j)
}

// WaitForSibling postpones a flush of a session on a customer address while another flush of the
// same forwarder or deposit account (and token) is submitted or not yet final: a flush sweeps the
// whole balance, so that one may move this session's funds too, and this job then finds it empty
// and ends as covered instead of reading a balance that is about to move. Waiting is not a
// failure: no attempt is counted and nothing alerts.
func (l *Lifecycle) WaitForSibling(ctx context.Context, j store.TxJob) (bool, error) {
	busy, err := l.Store.FlushSiblingInFlight(ctx, j)
	if err != nil || !busy {
		return false, err
	}
	j.NextAttemptAt = l.Store.Now().Add(l.BaseBackoff)
	l.Log.Info("flush waits for another flush of the same "+l.Sibling, "chain", l.Chain, "job", j.ID, "session", j.SessionID)
	return true, l.Store.UpdateTxJob(ctx, &j)
}

// ReconcileFlushFees runs after a flush settlement was recorded on the job's session: when the
// session's flushes of the asset charged the fixed fee more than once (a third party flushed after
// a part payment, ADR 0008 I-1), it raises an alert and records the extra on the job's
// details. The settlement is already stored, so a failure here is logged, never retried.
func (l *Lifecycle) ReconcileFlushFees(ctx context.Context, j store.TxJob, ses *store.Session) {
	if ses == nil {
		return
	}
	ex, ok, err := flushfee.Reconcile(ses, l.Chain, j.Asset)
	if err != nil {
		l.Log.Warn("flush fee reconciliation failed", "chain", l.Chain, "job", j.ID, "session", j.SessionID, "err", err)
		return
	}
	if !ok {
		return
	}
	l.Alerts.Alert(ctx, ex.Alert(ses))
	if err := l.Store.AddTxJobDetails(ctx, j.ID, ex.Details()); err != nil {
		l.Log.Warn("flush fee excess not recorded on the job", "chain", l.Chain, "job", j.ID, "session", j.SessionID, "err", err)
	}
}

// EncodeRecipients keeps a job's recipient list in its details: "address:bps,..." in order.
func EncodeRecipients(rs []payout.Recipient) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Address + ":" + strconv.Itoa(int(r.Bps))
	}
	return strings.Join(parts, ",")
}
