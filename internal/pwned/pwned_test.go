package pwned

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const password = "Tr0ub4dor&3 long enough"

func parts(pw string) (prefix, suffix string) {
	sum := sha1.Sum([]byte(pw))
	d := strings.ToUpper(hex.EncodeToString(sum[:]))
	return d[:5], d[5:]
}

// serve answers the range API with body and records the requests it got.
func serve(t *testing.T, status int, body string) (*Client, *[]*http.Request) {
	t.Helper()
	var got []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}, &got
}

func TestBreached(t *testing.T) {
	_, suffix := parts(password)
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"found", "0018A45C4D1DEF81644B54AB7F969B88D65:1\r\n" + suffix + ":42\r\n", true},
		{"found in lower case", strings.ToLower(suffix) + ":7\r\n", true},
		{"not found", "0018A45C4D1DEF81644B54AB7F969B88D65:1\r\n00D4F6E8FA6EECAD2A3AA415EEC418D38EC:2\r\n", false},
		{"padded entry with count 0", suffix + ":0\r\n", false},
		{"empty answer", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, http.StatusOK, tc.body)
			got, err := c.Breached(t.Context(), password)
			if err != nil || got != tc.want {
				t.Fatalf("Breached = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestOnlyThePrefixIsSent(t *testing.T) {
	prefix, suffix := parts(password)
	c, got := serve(t, http.StatusOK, "")
	if _, err := c.Breached(t.Context(), password); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("%d requests", len(*got))
	}
	r := (*got)[0]
	if r.Method != http.MethodGet || r.URL.Path != "/range/"+prefix || r.URL.RawQuery != "" {
		t.Fatalf("request: %s %s", r.Method, r.URL)
	}
	if r.Header.Get("Add-Padding") != "true" {
		t.Fatalf("Add-Padding: %q", r.Header.Get("Add-Padding"))
	}
	for name, vals := range r.Header {
		for _, v := range append(vals, name) {
			if strings.Contains(v, password) || strings.Contains(strings.ToUpper(v), suffix) {
				t.Fatalf("header %s leaks the password or the suffix: %q", name, v)
			}
		}
	}
	if strings.Contains(r.URL.String(), suffix) {
		t.Fatalf("URL leaks the suffix: %s", r.URL)
	}
}

func TestFailures(t *testing.T) {
	prefix, suffix := parts(password)
	check := func(t *testing.T, c *Client) {
		t.Helper()
		got, err := c.Breached(t.Context(), password)
		if err == nil || got {
			t.Fatalf("Breached = %v, %v; want an error", got, err)
		}
		for _, leak := range []string{password, prefix, suffix} {
			if strings.Contains(strings.ToUpper(err.Error()), strings.ToUpper(leak)) {
				t.Fatalf("the error leaks %q: %v", leak, err)
			}
		}
	}
	t.Run("server error", func(t *testing.T) {
		c, _ := serve(t, http.StatusServiceUnavailable, "busy")
		check(t, c)
	})
	t.Run("malformed count", func(t *testing.T) {
		c, _ := serve(t, http.StatusOK, suffix+":many\r\n")
		check(t, c)
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		client := srv.Client()
		url := srv.URL
		srv.Close()
		check(t, &Client{BaseURL: url, HTTP: client})
	})
	t.Run("redirect not followed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}))
		defer srv.Close()
		c := New()
		c.BaseURL = srv.URL
		check(t, c)
	})
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := New()
	c.BaseURL = srv.URL
	c.HTTP.Timeout = 50 * time.Millisecond
	start := time.Now()
	got, err := c.Breached(t.Context(), password)
	if err == nil || got {
		t.Fatalf("Breached = %v, %v; want a timeout", got, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	prefix, _ := parts(password)
	if strings.Contains(err.Error(), prefix) {
		t.Fatalf("the error leaks the prefix: %v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	c, _ := serve(t, http.StatusOK, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Breached(ctx, password); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaults(t *testing.T) {
	c := New()
	if c.BaseURL != "https://api.pwnedpasswords.com" || c.HTTP.Timeout != 2*time.Second {
		t.Fatalf("defaults: %s %v", c.BaseURL, c.HTTP.Timeout)
	}
}
