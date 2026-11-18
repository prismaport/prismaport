package service

import (
	"context"
	"net/url"
	"time"

	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/store"
)

const (
	// noticeBatch is how many queued emails about failing endpoints one step sends.
	noticeBatch = 10
	// noticeLease hides a claimed notice from other workers while this one sends it.
	noticeLease = 5 * time.Minute
)

// SendWebhookNotices sends the queued emails about failing webhook endpoints (a worker loop):
// the streak's warning and the notice that an endpoint was disabled, to every verified dashboard
// user of the merchant. It sends each message itself and waits for the provider's answer, so the
// worker gives it the mail transport, not the background queue. A warning counts only once the
// provider accepted it for at least one recipient (or there is nobody to tell); refused for
// everyone, it is dropped and the next failed attempt queues it again. Without a mail transport
// nothing is sent and the notices wait, so no endpoint is disabled before someone was told.
func (s *Service) SendWebhookNotices(ctx context.Context) (int, error) {
	if s.cfg.Mail == nil {
		return 0, nil
	}
	list, err := s.store.ClaimWebhookNotices(ctx, noticeBatch, noticeLease)
	if err != nil {
		return 0, err
	}
	for _, n := range list {
		current := n.Current()
		// The warning's time is when it was sent: the email promises the disable no sooner than
		// a day after it.
		at := s.store.Now()
		accepted := 0
		if current {
			for _, to := range n.Recipients {
				m, err := s.webhookNotice(n, to, at)
				if err == nil {
					err = s.cfg.Mail.Send(ctx, m)
				}
				if err != nil {
					s.Logger().Warn("webhook endpoint email not accepted", "kind", m.Kind, "endpoint", n.Endpoint.ID, "err", err)
					continue
				}
				accepted++
			}
		}
		counted := current && (accepted > 0 || len(n.Recipients) == 0)
		if err := s.store.FinishWebhookNotice(ctx, n, counted, accepted, at); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

func (s *Service) webhookNotice(n store.WebhookNotice, to string, at time.Time) (mail.Message, error) {
	e := n.Endpoint
	d := mail.WebhookEmail{EndpointID: e.ID, Target: urlTarget(e.URL), Livemode: e.Livemode, FailingSince: n.FailingSince,
		DashboardURL: s.dashboardURL()}
	if n.LastStatusCode != nil {
		d.LastStatusCode = *n.LastStatusCode
	}
	if n.LastError != nil {
		d.LastError = *n.LastError
	}
	if n.Kind == store.NoticeDisabled {
		if e.DisabledAt != nil {
			d.DisabledAt = *e.DisabledAt
		}
		return mail.WebhookDisabled(to, d)
	}
	// The endpoint is disabled at a failed attempt both FailingDisableAfter after the streak
	// began and WarnedBeforeDisable after this warning (recorded at at).
	d.DisableFrom = n.FailingSince.Add(store.FailingDisableAfter)
	if after := at.Add(store.WarnedBeforeDisable); after.After(d.DisableFrom) {
		d.DisableFrom = after
	}
	return mail.WebhookFailing(to, d)
}

// urlTarget is a URL's scheme and host: its path or query can carry the merchant's tokens.
func urlTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unreadable URL)"
	}
	return u.Scheme + "://" + u.Host
}
