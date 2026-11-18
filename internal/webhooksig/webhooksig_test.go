package webhooksig_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/webhooksig"
)

var now = time.Unix(1_761_240_000, 0)

func TestKnownVector(t *testing.T) {
	// Computed independently with Python's hmac module over b'1761240000.{"id":"evt_1"}'.
	got := webhooksig.Compute("whsec_test", 1761240000, []byte(`{"id":"evt_1"}`))
	if got != "27b7b99a1b97b1b82cfe33170c926269604cc5e36bbf29169553b84bf6c9d06b" {
		t.Fatalf("hmac %s", got)
	}
	h := webhooksig.Header(time.Unix(1761240000, 0), []byte(`{"id":"evt_1"}`), "whsec_test")
	if h != "t=1761240000,v1=27b7b99a1b97b1b82cfe33170c926269604cc5e36bbf29169553b84bf6c9d06b" {
		t.Fatalf("header %s", h)
	}
}

func TestVerify(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"session.paid"}`)
	h := webhooksig.Header(now, body, "whsec_new", "whsec_old")
	for _, secret := range []string{"whsec_new", "whsec_old"} {
		if err := webhooksig.Verify(body, h, secret, 0, now.Add(time.Minute)); err != nil {
			t.Fatalf("%s: %v", secret, err)
		}
	}
	cases := []struct {
		name   string
		body   []byte
		header string
		secret string
		at     time.Time
		want   error
	}{
		{"wrong secret", body, h, "whsec_other", now, webhooksig.ErrMismatch},
		{"tampered body", []byte(`{"id":"evt_2","type":"session.paid"}`), h, "whsec_new", now, webhooksig.ErrMismatch},
		{"stale", body, h, "whsec_new", now.Add(6 * time.Minute), webhooksig.ErrTimestamp},
		{"future", body, h, "whsec_new", now.Add(-6 * time.Minute), webhooksig.ErrTimestamp},
		{"no v1", body, "t=1761240000", "whsec_new", now, webhooksig.ErrMalformed},
		{"no t", body, "v1=abc", "whsec_new", now, webhooksig.ErrMalformed},
		{"two t", body, "t=1,t=2,v1=abc", "whsec_new", now, webhooksig.ErrMalformed},
		{"garbage", body, "garbage", "whsec_new", now, webhooksig.ErrMalformed},
		{"empty", body, "", "whsec_new", now, webhooksig.ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := webhooksig.Verify(c.body, c.header, c.secret, 0, c.at); !errors.Is(err, c.want) {
				t.Errorf("%s: got %v want %v", c.name, err, c.want)
			}
		})
	}
}

// TestConsumerHandler is the integration pattern documented for merchants: verify the raw body
// before parsing it.
func TestConsumerHandler(t *testing.T) {
	const secret = "whsec_consumer"
	var accepted []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		if err := webhooksig.Verify(raw, r.Header.Get(webhooksig.HeaderName), secret, 0, now); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		accepted = append(accepted, string(raw))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	send := func(body, header string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set(webhooksig.HeaderName, header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := `{"id":"evt_1"}`
	if code := send(body, webhooksig.Header(now, []byte(body), secret)); code != http.StatusNoContent {
		t.Fatalf("valid delivery rejected: %d", code)
	}
	if code := send(body, webhooksig.Header(now, []byte(body), "whsec_attacker")); code != http.StatusBadRequest {
		t.Fatalf("forged delivery accepted: %d", code)
	}
	if len(accepted) != 1 {
		t.Fatalf("accepted %d", len(accepted))
	}
}

func TestParseAndMatches(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	h := webhooksig.Header(now, body, "whsec_a", "whsec_b")
	ts, sigs, err := webhooksig.Parse(h)
	if err != nil || ts != now.Unix() || len(sigs) != 2 {
		t.Fatalf("parse: %d %v %v", ts, sigs, err)
	}
	if !webhooksig.Matches(body, ts, sigs, "whsec_b") || webhooksig.Matches(body, ts, sigs, "whsec_c") ||
		webhooksig.Matches(body, ts+1, sigs, "whsec_a") {
		t.Fatal("matches")
	}
	// Unknown schemes are ignored next to v1.
	if _, sigs, err := webhooksig.Parse("t=1,v0=zz,v1=ab"); err != nil || len(sigs) != 1 || sigs[0] != "ab" {
		t.Fatalf("unknown scheme: %v %v", sigs, err)
	}
	for _, bad := range []string{"", "t=1", "v1=a", "t=-1,v1=a", "t=x,v1=a", "t=1,t=2,v1=a", "t=1,v1"} {
		if _, _, err := webhooksig.Parse(bad); !errors.Is(err, webhooksig.ErrMalformed) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

type vectorFile struct {
	Compute []struct {
		Name      string `json:"name"`
		Secret    string `json:"secret"`
		Timestamp int64  `json:"timestamp"`
		Body      string `json:"body"`
		Signature string `json:"signature"`
	} `json:"compute"`
	Verify []struct {
		Name      string `json:"name"`
		Body      string `json:"body"`
		Header    string `json:"header"`
		Secret    string `json:"secret"`
		Now       int64  `json:"now"`
		Tolerance int64  `json:"tolerance_seconds"`
		Result    string `json:"result"`
	} `json:"verify"`
}

// TestSharedVectors runs the vectors the developer portal (web/docs) is tested against too.
func TestSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Compute) < 4 || len(v.Verify) < 20 {
		t.Fatalf("vectors: %d compute, %d verify", len(v.Compute), len(v.Verify))
	}
	for _, c := range v.Compute {
		if got := webhooksig.Compute(c.Secret, c.Timestamp, []byte(c.Body)); got != c.Signature {
			t.Errorf("%s: %s", c.Name, got)
		}
		if h := webhooksig.Header(time.Unix(c.Timestamp, 0), []byte(c.Body), c.Secret); h != "t="+strconv.FormatInt(c.Timestamp, 10)+",v1="+c.Signature {
			t.Errorf("%s header: %s", c.Name, h)
		}
	}
	want := map[string]error{"ok": nil, "malformed": webhooksig.ErrMalformed, "timestamp": webhooksig.ErrTimestamp, "mismatch": webhooksig.ErrMismatch}
	for _, c := range v.Verify {
		w, ok := want[c.Result]
		if !ok {
			t.Fatalf("%s: unknown result %q", c.Name, c.Result)
		}
		err := webhooksig.Verify([]byte(c.Body), c.Header, c.Secret, time.Duration(c.Tolerance)*time.Second, time.Unix(c.Now, 0))
		if !errors.Is(err, w) || (w == nil && err != nil) {
			t.Errorf("%s: got %v want %v", c.Name, err, w)
		}
	}
}
