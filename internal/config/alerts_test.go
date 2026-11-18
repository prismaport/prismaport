package config

import (
	"strings"
	"testing"
)

func TestLoadAlertWebhook(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	cases := []struct {
		url, secret string
		err         string
	}{
		{url: "", secret: ""},
		{url: "https://hooks.slack.com/services/T/B/X", secret: ""},
		{url: "https://alerts.example.com/prismaport", secret: "s3cret"},
		{url: "http://127.0.0.1:9000/alerts", secret: ""},
		{url: "http://localhost:9000/alerts", secret: ""},
		{url: "http://alerts.example.com/prismaport", err: "must use https"},
		{url: "ftp://alerts.example.com/", err: "must use https"},
		{url: "/alerts", err: "absolute URL"},
		{url: "", secret: "s3cret", err: "without PRISMAPORT_ALERT_WEBHOOK_URL"},
	}
	for _, c := range cases {
		t.Setenv("PRISMAPORT_ALERT_WEBHOOK_URL", c.url)
		t.Setenv("PRISMAPORT_ALERT_WEBHOOK_SECRET", c.secret)

		e, err := Load()

		switch {
		case c.err == "" && err != nil:
			t.Errorf("%q: %v", c.url, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%q secret %q: err %v, want %q", c.url, c.secret, err, c.err)
		case c.err == "" && (e.AlertWebhookURL != c.url || e.AlertWebhookSecret != c.secret):
			t.Errorf("%q: loaded %q %q", c.url, e.AlertWebhookURL, e.AlertWebhookSecret)
		}
	}
}
