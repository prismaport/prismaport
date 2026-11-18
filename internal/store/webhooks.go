package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Endpoint statuses and the reasons a disabled endpoint gives.
const (
	EndpointEnabled  = "enabled"
	EndpointDisabled = "disabled"
	// DisabledByMerchant is a pause; DisabledFailing is Prismaport's, after a long failure streak.
	DisabledByMerchant = "merchant"
	DisabledFailing    = "failing"
)

// MaxWebhookEndpoints is how many endpoints a merchant holds per mode, enabled and disabled
// (deleted ones and the sandbox inbox's do not count).
const MaxWebhookEndpoints = 16

// The errors pending deliveries are dead-lettered with when their endpoint stops (last_error).
const (
	ErrTextEndpointDeleted     = "endpoint deleted"
	ErrTextEndpointPaused      = "endpoint paused"
	ErrTextEndpointKeptFailing = "endpoint kept failing"
)

// Failure streak limits: the streak, the warning and the automatic disable.
const (
	// FailingWarnAfter: the first failed attempt this long after failing_since warns.
	FailingWarnAfter = 24 * time.Hour
	// FailingDisableAfter: the first failed attempt this long after failing_since, and at least
	// WarnedBeforeDisable after the warning counted, disables the endpoint.
	FailingDisableAfter = 5 * 24 * time.Hour
	WarnedBeforeDisable = 24 * time.Hour
)

// Notice kinds: the emails about a failing endpoint.
const (
	NoticeFailingWarning = "failing_warning"
	NoticeDisabled       = "disabled"
)

// WebhookEndpoint is a merchant's webhook destination with its enabled events and sealed signing secret.
type WebhookEndpoint struct {
	ID                      string
	MerchantID              string
	Livemode                bool
	URL                     string
	EnabledEvents           []string
	Description             *string
	SecretSealed            []byte
	PreviousSecretSealed    []byte
	PreviousSecretExpiresAt *time.Time
	CreatedAt               time.Time
	Status                  string
	DisabledReason          *string
	// DisabledAt is when the endpoint became disabled (paused or failing); nil while enabled.
	DisabledAt *time.Time
	// FailingSince is the start of the current failure streak, nil when none is open.
	FailingSince *time.Time
	DeletedAt    *time.Time
	// Inbox marks the hosted sandbox inbox's endpoint: it is never edited, counted or disabled.
	Inbox bool
}

// endpointColumns reads an endpoint aliased e.
const endpointColumns = `e.id, e.merchant_id, e.livemode, e.url, e.enabled_events, e.description, e.secret_ciphertext,
	e.previous_secret_ciphertext, e.previous_secret_expires_at, e.created_at, e.status, e.disabled_reason, e.disabled_at,
	e.failing_since, e.deleted_at, EXISTS (SELECT 1 FROM sandbox_inboxes sbx WHERE sbx.endpoint_id = e.id)`

// fields are the scan destinations of endpointColumns, in order.
func (e *WebhookEndpoint) fields() []any {
	return []any{&e.ID, &e.MerchantID, &e.Livemode, &e.URL, &e.EnabledEvents, &e.Description, &e.SecretSealed,
		&e.PreviousSecretSealed, &e.PreviousSecretExpiresAt, &e.CreatedAt, &e.Status, &e.DisabledReason, &e.DisabledAt,
		&e.FailingSince, &e.DeletedAt, &e.Inbox}
}

func scanEndpoint(r pgx.Row) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	err := r.Scan(e.fields()...)
	return e, notFound(err)
}

var (
	// ErrEndpointLimit refuses an endpoint past MaxWebhookEndpoints in its mode.
	ErrEndpointLimit = errors.New("store: the merchant holds the most webhook endpoints a mode allows")
	// ErrInboxEndpoint refuses an edit of the sandbox inbox's endpoint.
	ErrInboxEndpoint = errors.New("store: the sandbox inbox endpoint is managed through the inbox")
)

// RotationPendingError refuses a secret rotation while the previous secret's window is open: a
// consumer may still verify with that secret only, and a second rotation would end it at once.
type RotationPendingError struct {
	// Until is when the previous secret stops signing deliveries and a rotation is allowed again.
	Until time.Time
}

func (e *RotationPendingError) Error() string {
	return "store: the previous webhook secret is valid until " + e.Until.UTC().Format(time.RFC3339)
}

// lockSequencer takes the sequencer's lock inside tx, so no event turns into deliveries while an
// endpoint stops or starts: a delivery is created before the change (and dead-lettered by it) or
// not at all.
func lockSequencer(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(sequencerLockKey))
	return err
}

// deadLetterPending moves the endpoint's pending deliveries to the dead letter with reason.
func deadLetterPending(ctx context.Context, tx pgx.Tx, endpointID, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'dead', last_error = $2
		WHERE endpoint_id = $1 AND status = 'pending'`, endpointID, reason)
	return err
}

// CreateWebhookEndpoint inserts an enabled endpoint. The count against MaxWebhookEndpoints and
// the insert run under one lock per merchant and mode, so concurrent creates cannot pass it.
func (s *Store) CreateWebhookEndpoint(ctx context.Context, e WebhookEndpoint, actor string) (WebhookEndpoint, error) {
	e.CreatedAt = s.now()
	e.Status = EndpointEnabled
	if e.EnabledEvents == nil {
		e.EnabledEvents = []string{}
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"webhook_endpoints/"+e.MerchantID+"/"+boolKey(e.Livemode)); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM webhook_endpoints e WHERE e.merchant_id = $1 AND e.livemode = $2
			AND e.deleted_at IS NULL AND NOT EXISTS (SELECT 1 FROM sandbox_inboxes sbx WHERE sbx.endpoint_id = e.id)`,
			e.MerchantID, e.Livemode).Scan(&n); err != nil {
			return err
		}
		if n >= MaxWebhookEndpoints {
			return ErrEndpointLimit
		}
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_endpoints (id, merchant_id, livemode, url, enabled_events, description,
			secret_ciphertext, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			e.ID, e.MerchantID, e.Livemode, e.URL, e.EnabledEvents, e.Description, e.SecretSealed, e.CreatedAt); err != nil {
			return err
		}
		return audit(ctx, tx, e.MerchantID, actor, "webhook_endpoint.created", map[string]any{"endpoint_id": e.ID, "url": e.URL})
	})
	return e, opErr("create webhook endpoint", err)
}

// GetWebhookEndpoint returns an endpoint of the merchant and mode that is not deleted.
func (s *Store) GetWebhookEndpoint(ctx context.Context, merchantID string, livemode bool, id string) (WebhookEndpoint, error) {
	out, err := scanEndpoint(s.db.QueryRow(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints e
		WHERE e.id = $1 AND e.merchant_id = $2 AND e.livemode = $3 AND e.deleted_at IS NULL`, id, merchantID, livemode))
	return out, opErr("get webhook endpoint", err)
}

// ListWebhookEndpoints lists every endpoint that is not deleted, enabled and disabled.
func (s *Store) ListWebhookEndpoints(ctx context.Context, merchantID string, livemode bool) ([]WebhookEndpoint, error) {
	rows, err := s.db.Query(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints e
		WHERE e.merchant_id = $1 AND e.livemode = $2 AND e.deleted_at IS NULL ORDER BY e.id`, merchantID, livemode)
	if err != nil {
		return nil, opErr("list webhook endpoints", err)
	}
	defer rows.Close()
	var out []WebhookEndpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, opErr("list webhook endpoints", err)
		}
		out = append(out, e)
	}
	return out, opErr("list webhook endpoints", rows.Err())
}

// DeleteWebhookEndpoint removes the endpoint for good: no more deliveries, its pending ones are
// dead-lettered (endpoint deleted) and an inbox registered as it is closed. Its log stays readable.
func (s *Store) DeleteWebhookEndpoint(ctx context.Context, merchantID string, livemode bool, id, actor string) error {
	return opErr("delete webhook endpoint", s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockSequencer(ctx, tx); err != nil {
			return err
		}
		now := s.now()
		tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET deleted_at = $4
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL`, id, merchantID, livemode, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := deadLetterPending(ctx, tx, id, ErrTextEndpointDeleted); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM webhook_endpoint_notices WHERE endpoint_id = $1`, id); err != nil {
			return err
		}
		if err := disableInboxOfEndpoint(ctx, tx, id, now); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "webhook_endpoint.deleted", map[string]any{"endpoint_id": id})
	}))
}

// EndpointPatch is an edit of an endpoint; nil fields do not change.
type EndpointPatch struct {
	URL           *string
	EnabledEvents *[]string
	// SetDescription changes the description to Description (nil clears it).
	SetDescription bool
	Description    *string
	Status         *string
	// CheckURL validates the endpoint's URL, against the rules of now, when the edit enables it.
	CheckURL func(url string) error
}

// UpdateWebhookEndpoint edits an endpoint that is not deleted; the secret never changes here. A
// patch with a status takes the sequencer's lock first. A pause (disabled, reason merchant)
// dead-letters the pending deliveries (endpoint paused); an enable re-checks the URL, clears the
// reason, disabled_at and the failure streak with its warning, and dead-letters anything still
// pending, so nothing from the disabled period goes out. A status equal to the current one
// changes nothing, so its timers stay.
func (s *Store) UpdateWebhookEndpoint(ctx context.Context, merchantID string, livemode bool, id string, p EndpointPatch,
	actor string) (WebhookEndpoint, error) {
	var out WebhookEndpoint
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if p.Status != nil {
			if err := lockSequencer(ctx, tx); err != nil {
				return err
			}
		}
		cur, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints e
			WHERE e.id = $1 AND e.merchant_id = $2 AND e.livemode = $3 AND e.deleted_at IS NULL FOR UPDATE OF e`, id, merchantID, livemode))
		if err != nil {
			return err
		}
		if cur.Inbox {
			return ErrInboxEndpoint
		}
		now := s.now()
		next := cur
		var changed []string
		if p.URL != nil && *p.URL != cur.URL {
			next.URL = *p.URL
			changed = append(changed, "url")
		}
		if p.EnabledEvents != nil && !slices.Equal(*p.EnabledEvents, cur.EnabledEvents) {
			next.EnabledEvents = *p.EnabledEvents
			changed = append(changed, "enabled_events")
		}
		if p.SetDescription && !equalText(p.Description, cur.Description) {
			next.Description = p.Description
			changed = append(changed, "description")
		}
		action := ""
		var details map[string]any
		if p.Status != nil && *p.Status != cur.Status {
			switch *p.Status {
			case EndpointDisabled:
				reason := DisabledByMerchant
				next.Status, next.DisabledReason, next.DisabledAt = EndpointDisabled, &reason, &now
				action, details = "webhook_endpoint.disabled", map[string]any{"endpoint_id": id, "disabled_reason": reason}
			case EndpointEnabled:
				if p.CheckURL != nil {
					if err := p.CheckURL(next.URL); err != nil {
						return err
					}
				}
				next.Status, next.DisabledReason, next.DisabledAt, next.FailingSince = EndpointEnabled, nil, nil, nil
				action, details = "webhook_endpoint.enabled", map[string]any{"endpoint_id": id}
			default:
				return fmt.Errorf("store: unknown endpoint status %q", *p.Status)
			}
		}
		out = next
		if len(changed) == 0 && action == "" {
			return nil
		}
		// The warning belongs to the streak: it goes when failing_since does.
		if _, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET url = $2, enabled_events = $3, description = $4, status = $5,
			disabled_reason = $6, disabled_at = $7, failing_since = $8,
			failing_warned_at = CASE WHEN $8::timestamptz IS NULL THEN NULL ELSE failing_warned_at END WHERE id = $1`, id, next.URL, next.EnabledEvents, next.Description,
			next.Status, next.DisabledReason, next.DisabledAt, next.FailingSince); err != nil {
			return err
		}
		if len(changed) > 0 {
			d := map[string]any{"endpoint_id": id, "fields": changed}
			if next.URL != cur.URL {
				d["url"] = next.URL
			}
			if err := audit(ctx, tx, merchantID, actor, "webhook_endpoint.updated", d); err != nil {
				return err
			}
		}
		if action == "" {
			return nil
		}
		// Both directions dead-letter what is pending and drop a queued warning: a pause discards
		// what was waiting, an enable starts from the next sequenced event with no streak, and an
		// attempt that started before the enable no longer counts towards one.
		if err := deadLetterPending(ctx, tx, id, ErrTextEndpointPaused); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM webhook_endpoint_notices WHERE endpoint_id = $1 AND kind = $2`, id, NoticeFailingWarning); err != nil {
			return err
		}
		if next.Status == EndpointEnabled {
			if _, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET outcome_started_at = $2 WHERE id = $1`,
				id, now); err != nil {
				return err
			}
		}
		return audit(ctx, tx, merchantID, actor, action, details)
	})
	return out, opErr("update webhook endpoint", err)
}

func equalText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// RotateWebhookSecret installs a new secret and keeps the old one valid until previousExpiresAt.
// It works on a disabled endpoint, so a merchant can rotate before enabling it. While the window
// of an earlier rotation is still open it changes nothing and returns a *RotationPendingError.
func (s *Store) RotateWebhookSecret(ctx context.Context, merchantID string, livemode bool, id string,
	seal func(endpointID string) ([]byte, error), previousExpiresAt time.Time, actor string) (WebhookEndpoint, error) {
	var out WebhookEndpoint
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints e
			WHERE e.id = $1 AND e.merchant_id = $2 AND e.livemode = $3 AND e.deleted_at IS NULL FOR UPDATE OF e`, id, merchantID, livemode))
		if err != nil {
			return err
		}
		if until := cur.PreviousSecretExpiresAt; until != nil && s.Now().Before(*until) {
			return &RotationPendingError{Until: *until}
		}
		sealed, err := seal(id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET secret_ciphertext = $2, previous_secret_ciphertext = $3,
			previous_secret_expires_at = $4 WHERE id = $1`, id, sealed, cur.SecretSealed, previousExpiresAt); err != nil {
			return err
		}
		cur.PreviousSecretSealed, cur.PreviousSecretExpiresAt, cur.SecretSealed = cur.SecretSealed, &previousExpiresAt, sealed
		out = cur
		return audit(ctx, tx, merchantID, actor, "webhook_endpoint.secret_rotated", map[string]any{"endpoint_id": id})
	})
	return out, opErr("rotate webhook secret", err)
}

// DeliveryJob is a claimed webhook delivery attempt with its endpoint and event payload.
type DeliveryJob struct {
	DeliveryID string
	Attempts   int
	// LeasedUntil is the lease this claim wrote; the outcome is recorded only while it still holds.
	LeasedUntil time.Time
	Endpoint    WebhookEndpoint
	EventID     string
	Payload     json.RawMessage
}

// ClaimDueDeliveries leases due deliveries of enabled endpoints: their next_attempt_at moves to
// now+lease so another worker skips them while this one makes the HTTP call. If this worker dies,
// the lease lapses and the delivery is retried, which keeps delivery at-least-once. Deliveries of
// the endpoints in skip (those this worker is still sending to) are left for a later claim.
func (s *Store) ClaimDueDeliveries(ctx context.Context, limit int, lease time.Duration, skip ...string) ([]DeliveryJob, error) {
	var jobs []DeliveryJob
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		// Postgres keeps microseconds, so the token must too or the guarded write never matches.
		until := now.Add(lease).Truncate(time.Microsecond)
		rows, err := tx.Query(ctx, `SELECT d.id, d.attempts, d.event_id, ev.payload::text, `+endpointColumns+`
			FROM webhook_deliveries d
			JOIN webhook_endpoints e ON e.id = d.endpoint_id
			JOIN events ev ON ev.id = d.event_id
			WHERE d.status = 'pending' AND d.next_attempt_at <= $1 AND e.status = 'enabled' AND e.deleted_at IS NULL
			  AND d.endpoint_id <> ALL($3::text[])
			ORDER BY d.next_attempt_at, ev.seq LIMIT $2 FOR UPDATE OF d SKIP LOCKED`, now, limit, append([]string{}, skip...))
		if err != nil {
			return err
		}
		jobs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeliveryJob, error) {
			var j DeliveryJob
			var payload string
			err := r.Scan(append([]any{&j.DeliveryID, &j.Attempts, &j.EventID, &payload}, j.Endpoint.fields()...)...)
			j.Payload = json.RawMessage(payload)
			j.LeasedUntil = until
			return j, err
		})
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = $2 WHERE id = $1`, j.DeliveryID, until); err != nil {
				return err
			}
		}
		return nil
	})
	return jobs, opErr("claim due deliveries", err)
}

// DeliveryOutcome is the result of one webhook delivery attempt.
type DeliveryOutcome struct {
	StatusCode int
	Err        string
	// NextAttemptAt nil on a failure means the delivery is dead-lettered.
	NextAttemptAt *time.Time
	Succeeded     bool
	// StartedAt is when the attempt started. Outcomes apply to the endpoint's failure streak in
	// this order, whatever order they are recorded in.
	StartedAt time.Time
	// Reason names a failure without an HTTP answer worth showing, from a fixed list ("timeout",
	// "redirect", ...), for the failing-endpoint emails. It never carries text from the target.
	Reason string
}

// streak is an endpoint's failure streak as an outcome finds it.
type streak struct {
	enabled          bool
	inbox            bool
	failingSince     *time.Time
	warnedAt         *time.Time
	outcomeStartedAt *time.Time
}

// streakStep is what one attempt's outcome does to the streak.
type streakStep int

const (
	// streakIgnore: a disabled or inbox endpoint, or an outcome older than one already applied.
	streakIgnore streakStep = iota
	streakClose
	streakOpen
	streakKeep
	streakWarn
	streakDisable
)

// step decides what an outcome of an attempt started at started does. Warning and disable are
// checked on failed attempts only, so an idle endpoint is never warned or disabled; an attempt
// that crosses both limits at once only warns, since disabling needs a warning a day old.
func (st streak) step(succeeded bool, started time.Time) streakStep {
	if !st.enabled || st.inbox || (st.outcomeStartedAt != nil && started.Before(*st.outcomeStartedAt)) {
		return streakIgnore
	}
	switch {
	case succeeded:
		return streakClose
	case st.failingSince == nil:
		return streakOpen
	case st.warnedAt == nil:
		if started.Sub(*st.failingSince) >= FailingWarnAfter {
			return streakWarn
		}
	case started.Sub(*st.failingSince) >= FailingDisableAfter && started.Sub(*st.warnedAt) >= WarnedBeforeDisable:
		return streakDisable
	}
	return streakKeep
}

// errNeedSequencerLock asks for the outcome to be recorded again holding the sequencer's lock,
// which disabling the endpoint needs and which must be taken before the endpoint's row.
var errNeedSequencerLock = errors.New("store: disabling the endpoint needs the sequencer's lock")

// RecordDeliveryAttempt writes the outcome of a claimed attempt, only while the claim's lease
// still holds: the row is pending with next_attempt_at at the lease it wrote. A lapsed lease that
// another worker claimed again, a resend, a pause or a deleted endpoint all change that, and the
// outcome is dropped (false) instead of overwriting what they wrote. A recorded outcome then
// moves the endpoint's failure streak: it may open or close it, queue the warning email or
// disable the endpoint.
func (s *Store) RecordDeliveryAttempt(ctx context.Context, job DeliveryJob, attempts int, o DeliveryOutcome) (bool, error) {
	if o.StartedAt.IsZero() {
		o.StartedAt = s.now()
	}
	recorded, err := s.recordAttempt(ctx, job, attempts, o, false)
	if errors.Is(err, errNeedSequencerLock) {
		recorded, err = s.recordAttempt(ctx, job, attempts, o, true)
	}
	return recorded, opErr("record delivery attempt", err)
}

func (s *Store) recordAttempt(ctx context.Context, job DeliveryJob, attempts int, o DeliveryOutcome, sequencerLocked bool) (bool, error) {
	now := s.now()
	var code *int
	if o.StatusCode != 0 {
		code = &o.StatusCode
	}
	var errText *string
	if o.Err != "" {
		errText = &o.Err
	}
	recorded := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if sequencerLocked {
			if err := lockSequencer(ctx, tx); err != nil {
				return err
			}
		}
		// Lock order everywhere: the sequencer's lock, the endpoint, then its deliveries.
		var st streak
		if err := tx.QueryRow(ctx, `SELECT e.status = 'enabled' AND e.deleted_at IS NULL,
			EXISTS (SELECT 1 FROM sandbox_inboxes sbx WHERE sbx.endpoint_id = e.id), e.failing_since, e.failing_warned_at, e.outcome_started_at
			FROM webhook_endpoints e WHERE e.id = $1 FOR NO KEY UPDATE OF e`, job.Endpoint.ID).Scan(
			&st.enabled, &st.inbox, &st.failingSince, &st.warnedAt, &st.outcomeStartedAt); err != nil {
			return err
		}
		step := st.step(o.Succeeded, o.StartedAt)
		if step == streakDisable && !sequencerLocked {
			return errNeedSequencerLock
		}
		const leased = ` WHERE id = $1 AND status = 'pending' AND next_attempt_at = $6`
		var tag pgconn.CommandTag
		var err error
		switch {
		case o.Succeeded:
			tag, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'succeeded', attempts = $2, last_attempt_at = $3,
				last_status_code = $4, last_error = $5, delivered_at = $3`+leased, job.DeliveryID, attempts, now, code, errText, job.LeasedUntil)
		case o.NextAttemptAt == nil:
			tag, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'dead', attempts = $2, last_attempt_at = $3,
				last_status_code = $4, last_error = $5`+leased, job.DeliveryID, attempts, now, code, errText, job.LeasedUntil)
		default:
			tag, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET attempts = $2, last_attempt_at = $3, last_status_code = $4,
				last_error = $5, next_attempt_at = $7`+leased, job.DeliveryID, attempts, now, code, errText, job.LeasedUntil, *o.NextAttemptAt)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
		recorded = true
		return s.applyStreak(ctx, tx, job.Endpoint, o, st, step, now)
	})
	return recorded, err
}

func (s *Store) applyStreak(ctx context.Context, tx pgx.Tx, e WebhookEndpoint, o DeliveryOutcome, st streak, step streakStep, now time.Time) error {
	id := e.ID
	started := o.StartedAt
	var err error
	switch step {
	case streakIgnore:
		return nil
	case streakClose:
		if _, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET failing_since = NULL, failing_warned_at = NULL, outcome_started_at = $2
			WHERE id = $1`, id, started); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM webhook_endpoint_notices WHERE endpoint_id = $1 AND kind = $2`, id, NoticeFailingWarning)
		return err
	case streakOpen:
		_, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET failing_since = $2, outcome_started_at = $2 WHERE id = $1`, id, started)
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET outcome_started_at = $2 WHERE id = $1`, id, started); err != nil {
		return err
	}
	if step == streakKeep {
		return nil
	}
	users, err := verifiedUserCount(ctx, tx, e.MerchantID)
	if err != nil {
		return err
	}
	if step == streakWarn {
		if users == 0 {
			// Nobody to tell: the warning counts at once, so the endpoint can still be disabled.
			if _, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET failing_warned_at = $2 WHERE id = $1`, id, now); err != nil {
				return err
			}
			return audit(ctx, tx, e.MerchantID, "system", "webhook_endpoint.failing_warned", map[string]any{"endpoint_id": id,
				"failing_since": st.failingSince.UTC().Format(time.RFC3339), "recipients": 0, "email": "skipped: no verified dashboard user"})
		}
		return queueNotice(ctx, tx, id, NoticeFailingWarning, *st.failingSince, o, now)
	}
	reason := DisabledFailing
	if _, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET status = 'disabled', disabled_reason = $2, disabled_at = $3 WHERE id = $1`,
		id, reason, now); err != nil {
		return err
	}
	if err := deadLetterPending(ctx, tx, id, ErrTextEndpointKeptFailing); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM webhook_endpoint_notices WHERE endpoint_id = $1 AND kind = $2`, id, NoticeFailingWarning); err != nil {
		return err
	}
	if users > 0 {
		if err := queueNotice(ctx, tx, id, NoticeDisabled, *st.failingSince, o, now); err != nil {
			return err
		}
	}
	return audit(ctx, tx, e.MerchantID, "system", "webhook_endpoint.disabled", map[string]any{"endpoint_id": id,
		"disabled_reason": reason, "failing_since": st.failingSince.UTC().Format(time.RFC3339)})
}

// queueNotice queues an email about a failing endpoint for the worker. A warning already queued
// or in flight keeps its row: at most one per endpoint at a time.
func queueNotice(ctx context.Context, tx pgx.Tx, endpointID, kind string, failingSince time.Time, o DeliveryOutcome, now time.Time) error {
	var code *int
	if o.StatusCode != 0 {
		code = &o.StatusCode
	}
	var reason *string
	if o.Reason != "" {
		reason = &o.Reason
	}
	_, err := tx.Exec(ctx, `INSERT INTO webhook_endpoint_notices (endpoint_id, kind, failing_since, last_status_code, last_error, created_at, leased_until)
		VALUES ($1, $2, $3, $4, $5, $6, $6) ON CONFLICT (endpoint_id) WHERE kind = 'failing_warning' DO NOTHING`,
		endpointID, kind, failingSince, code, reason, now)
	return err
}

func verifiedUserCount(ctx context.Context, tx pgx.Tx, merchantID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM dashboard_users WHERE merchant_id = $1 AND email_verified_at IS NOT NULL
		AND disabled_at IS NULL`, merchantID).Scan(&n)
	return n, err
}

// WebhookNotice is a queued email about a failing endpoint, claimed by a worker.
type WebhookNotice struct {
	ID       int64
	Kind     string
	Endpoint WebhookEndpoint
	// FailingSince is the streak the notice is about; a warning counts only for that streak.
	FailingSince   time.Time
	LastStatusCode *int
	LastError      *string
	// WarnedAt is when the endpoint's warning counted, if it did.
	WarnedAt *time.Time
	// Recipients are the merchant's verified dashboard users.
	Recipients []string
}

// Current reports whether the notice still describes the endpoint: a warning while the endpoint
// is enabled in the same unwarned streak, a disabled notice while it stays disabled for failing.
func (n WebhookNotice) Current() bool {
	e := n.Endpoint
	if e.DeletedAt != nil || e.FailingSince == nil || !e.FailingSince.Equal(n.FailingSince) {
		return false
	}
	if n.Kind == NoticeFailingWarning {
		return e.Status == EndpointEnabled && n.WarnedAt == nil
	}
	return e.Status == EndpointDisabled && e.DisabledReason != nil && *e.DisabledReason == DisabledFailing
}

// ClaimWebhookNotices leases queued notices so no other worker sends them meanwhile; a worker
// that dies leaves them to be claimed again when the lease lapses.
func (s *Store) ClaimWebhookNotices(ctx context.Context, limit int, lease time.Duration) ([]WebhookNotice, error) {
	var out []WebhookNotice
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		rows, err := tx.Query(ctx, `SELECT n.id, n.kind, n.failing_since, n.last_status_code, n.last_error, e.failing_warned_at, `+endpointColumns+`
			FROM webhook_endpoint_notices n JOIN webhook_endpoints e ON e.id = n.endpoint_id
			WHERE n.leased_until <= $1 ORDER BY n.id LIMIT $2 FOR UPDATE OF n SKIP LOCKED`, now, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (WebhookNotice, error) {
			var n WebhookNotice
			err := r.Scan(append([]any{&n.ID, &n.Kind, &n.FailingSince, &n.LastStatusCode, &n.LastError, &n.WarnedAt}, n.Endpoint.fields()...)...)
			return n, err
		})
		if err != nil {
			return err
		}
		for i := range out {
			if _, err := tx.Exec(ctx, `UPDATE webhook_endpoint_notices SET leased_until = $2 WHERE id = $1`, out[i].ID, now.Add(lease)); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT email FROM dashboard_users WHERE merchant_id = $1 AND email_verified_at IS NOT NULL
				AND disabled_at IS NULL ORDER BY email`, out[i].Endpoint.MerchantID)
			if err != nil {
				return err
			}
			if out[i].Recipients, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, opErr("claim webhook notices", err)
}

// FinishWebhookNotice removes a sent notice. A warning that counted (the provider accepted it
// for at least one recipient, or there was nobody to tell) records at, when it was sent, on the
// endpoint, if the streak it was about is still open and unwarned, and is audited.
func (s *Store) FinishWebhookNotice(ctx context.Context, n WebhookNotice, counted bool, accepted int, at time.Time) error {
	return opErr("finish webhook notice", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM webhook_endpoint_notices WHERE id = $1`, n.ID); err != nil {
			return err
		}
		if n.Kind != NoticeFailingWarning || !counted {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET failing_warned_at = $3 WHERE id = $1 AND failing_since = $2
			AND failing_warned_at IS NULL AND status = 'enabled' AND deleted_at IS NULL`, n.Endpoint.ID, n.FailingSince, at)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		details := map[string]any{"endpoint_id": n.Endpoint.ID, "failing_since": n.FailingSince.UTC().Format(time.RFC3339), "recipients": accepted}
		if len(n.Recipients) == 0 {
			details["email"] = "skipped: no verified dashboard user"
		}
		return audit(ctx, tx, n.Endpoint.MerchantID, "system", "webhook_endpoint.failing_warned", details)
	}))
}

// DeliveryStatus is the delivery state of one event to one endpoint.
type DeliveryStatus struct {
	ID             string
	EndpointID     string
	EventID        string
	Status         string
	Attempts       int
	LastStatusCode *int
	LastError      *string
}

// DeliveriesForEvent returns the delivery status of a merchant's event for each endpoint.
func (s *Store) DeliveriesForEvent(ctx context.Context, merchantID, eventID string) ([]DeliveryStatus, error) {
	rows, err := s.db.Query(ctx, deliveriesForEventSQL, eventID, merchantID)
	if err != nil {
		return nil, opErr("deliveries for event", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeliveryStatus, error) {
		var d DeliveryStatus
		err := r.Scan(&d.ID, &d.EndpointID, &d.EventID, &d.Status, &d.Attempts, &d.LastStatusCode, &d.LastError)
		return d, err
	})
	return out, opErr("deliveries for event", err)
}

// deliveriesForEventSQL reads an event's deliveries through webhook_deliveries_event (migration
// 00104).
const deliveriesForEventSQL = `SELECT id, endpoint_id, event_id, status, attempts, last_status_code, last_error
	FROM webhook_deliveries WHERE event_id = $1 AND merchant_id = $2 ORDER BY id`

// TestDelivery is a finished "Send test event" attempt.
type TestDelivery struct {
	ID         string
	EndpointID string
	EventID    string
	EventType  string
	StatusCode int
	Err        string
	Succeeded  bool
	At         time.Time
}

// InsertTestDelivery records a test delivery in the endpoint's log. It is final: succeeded or
// dead after its single attempt, never pending. It never touches the endpoint's status or
// failure streak.
func (s *Store) InsertTestDelivery(ctx context.Context, merchantID string, livemode bool, actor string, d TestDelivery) (DeliveryRecord, error) {
	var code *int
	if d.StatusCode != 0 {
		code = &d.StatusCode
	}
	var errText *string
	if d.Err != "" {
		errText = &d.Err
	}
	status := "dead"
	var delivered *time.Time
	if d.Succeeded {
		status, delivered = "succeeded", &d.At
	}
	var out DeliveryRecord
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, merchant_id, livemode, endpoint_id, event_id, status, attempts,
			next_attempt_at, last_attempt_at, last_status_code, last_error, delivered_at, created_at, test, test_event_id, test_event_type)
			VALUES ($1, $2, $3, $4, NULL, $5, 1, $6, $6, $7, $8, $9, $6, true, $10, $11)`,
			d.ID, merchantID, livemode, d.EndpointID, status, d.At, code, errText, delivered, d.EventID, d.EventType); err != nil {
			return err
		}
		var err error
		if out, err = scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+deliveryFrom+` WHERE d.id = $1`, d.ID)); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "webhook_endpoint.test_sent", map[string]any{"endpoint_id": d.EndpointID,
			"event_type": d.EventType, "delivery_id": d.ID})
	})
	return out, opErr("insert test delivery", err)
}
