package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

func TestTestKeyRefusesLiveKeys(t *testing.T) {
	t.Setenv("PRISMAPORT_API_KEY", "")
	for key, ok := range map[string]bool{"sk_test_abc": true, "sk_live_abc": false, "pk_test_abc": false, "": false} {
		t.Run(key, func(t *testing.T) {
			if _, err := testKey(key); (err == nil) != ok {
				t.Errorf("%q: %v", key, err)
			}
		})
	}
	t.Setenv("PRISMAPORT_API_KEY", "sk_test_env")
	if k, err := testKey(""); err != nil || k != "sk_test_env" {
		t.Fatalf("env key: %s %v", k, err)
	}
}

// Nothing reaches the network before the key and the flags are checked.
func TestSandboxCommandsRefuseBeforeAnyRequest(t *testing.T) {
	hit := false
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer api.Close()
	cases := [][]string{
		{"listen", "--api-key", "sk_live_x", "--forward-to", "http://localhost:1/x", "--api-url", api.URL},
		{"listen", "--api-key", "sk_test_x", "--api-url", api.URL},
		{"listen", "--api-key", "sk_test_x", "--forward-to", "localhost:3000", "--api-url", api.URL},
		{"listen", "--api-key", "sk_test_x", "--forward-to", "http://localhost:1/x", "--events", "session.refunded", "--api-url", api.URL},
		{"listen", "--api-key", "sk_test_x", "--forward-to", "http://localhost:1/x", "--after", "yesterday", "--api-url", api.URL},
		{"listen", "--api-key", "sk_test_x", "--forward-to", "http://localhost:1/x", "--secret", "abc", "--api-url", api.URL},
		{"trigger", "session.paid", "--api-key", "sk_live_x", "--api-url", api.URL},
		{"trigger", "session.refunded", "--api-key", "sk_test_x", "--api-url", api.URL},
		{"trigger", "--api-key", "sk_test_x"},
		{"trigger", "session.paid", "--api-key", "sk_test_x", "--api-url", "ftp://x"},
	}
	for _, args := range cases {
		var out bytes.Buffer
		if err := run(t.Context(), args, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if hit {
		t.Fatal("a refused command reached the API")
	}
}

func TestTriggerPlansCoverEveryEvent(t *testing.T) {
	if len(triggerPlans) != len(session.AllEventTypes) {
		t.Fatalf("%d plans for %d event types", len(triggerPlans), len(session.AllEventTypes))
	}
	for _, e := range session.AllEventTypes {
		if len(triggerPlans[string(e)]) == 0 {
			t.Errorf("no plan for %s", e)
		}
	}
	if f, err := eventFilter(" session.paid , payment.reversed"); err != nil || !f["session.paid"] || !f["payment.reversed"] || len(f) != 2 {
		t.Fatalf("filter: %v %v", f, err)
	}
	if f, err := eventFilter(""); err != nil || f != nil {
		t.Fatalf("empty filter: %v %v", f, err)
	}
}

func TestForwardOneSignsLikeADelivery(t *testing.T) {
	var got http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	payload := []byte(`{"id":"evt_1","type":"session.paid"}`)
	line := forwardOne(t.Context(), srv.Client(), srv.URL, "evt_1", "session.paid", payload, "whsec_cli")
	if !strings.Contains(line, "session.paid") || !strings.Contains(line, "evt_1") || !strings.Contains(line, "-> 202 Accepted") {
		t.Fatalf("line: %s", line)
	}
	if string(body) != string(payload) || got.Get("Prismaport-Event-Id") != "evt_1" || got.Get("Content-Type") != "application/json" {
		t.Fatalf("request: %v %s", got, body)
	}
	if err := webhooksig.Verify(body, got.Get("Signature"), "whsec_cli", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if line := forwardOne(t.Context(), http.DefaultClient, srv.URL, "evt_1", "session.paid", payload, "whsec_cli"); !strings.Contains(line, "-> error:") {
		t.Fatalf("unreachable: %s", line)
	}
}
