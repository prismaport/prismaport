package webhook

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	base, max := 30*time.Second, 6*time.Hour
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if got := Backoff(i+1, base, max); got != w {
			t.Errorf("attempt %d: %s want %s", i+1, got, w)
		}
	}
	if got := Backoff(40, base, max); got != max {
		t.Fatalf("cap: %s", got)
	}
	for n := 1; n < 30; n++ {
		if Backoff(n+1, base, max) < Backoff(n, base, max) {
			t.Fatalf("backoff decreased at %d", n)
		}
	}
}

func TestLeaseCoversTheBatch(t *testing.T) {
	cases := []struct {
		name string
		in   Config
		want time.Duration
	}{
		{"defaults", Config{}, 10*(10*time.Second+recordMargin) + leaseMargin},
		{"short lease raised", Config{Batch: 50, Timeout: 10 * time.Second, Lease: 2 * time.Minute}, 50*(10*time.Second+recordMargin) + leaseMargin},
		{"long lease kept", Config{Batch: 2, Timeout: time.Second, Lease: time.Hour}, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.in
			c.defaults()
			if c.Lease != tc.want {
				t.Errorf("%s: lease %s want %s", tc.name, c.Lease, tc.want)
			}
			if worst := time.Duration(c.Batch) * (c.Timeout + recordMargin); c.Lease <= worst {
				t.Errorf("%s: lease %s does not outlast the batch's worst case %s", tc.name, c.Lease, worst)
			}
		})
	}
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := defaultJitter(time.Minute)
		if d < 54*time.Second || d > 66*time.Second {
			t.Fatalf("jitter %s outside ±10%%", d)
		}
	}
}

func TestPublicOnly(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:443", "10.0.0.1:443", "192.168.1.1:80", "169.254.169.254:80", "[::1]:443", "0.0.0.0:80", "[fd00::1]:443",
		"0.0.0.1:80", "100.64.0.1:443", "100.100.100.200:80", "192.0.0.8:443", "198.18.0.1:443", "240.0.0.1:443",
		"[64:ff9b::a9fe:a9fe]:80", "[64:ff9b:1::a00:1]:443", "[2002:a00:1::1]:443", "[100::1]:443", "[fe80::1%eth0]:443",
		"[::ffff:169.254.169.254]:80", "[::ffff:100.100.100.200]:80", "not-an-address", "example.com:443"} {
		if err := publicOnly("tcp", addr, nil); !errors.Is(err, errPrivateAddress) {
			t.Errorf("%s allowed: %v", addr, err)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443", "[::ffff:93.184.216.34]:443"} {
		if err := publicOnly("tcp", addr, nil); err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
	}
}

func TestValidUTF8Prefix(t *testing.T) {
	cases := map[string]string{
		"plain":         "plain",
		"caf\xc3":       "caf",
		"caf\xc3\xa9":   "café",
		"a\xe2\x82":     "a",
		"a\xe2\x82\xac": "a€",
		"\xff ok":       "\uFFFD ok",
		"":              "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := validUTF8Prefix([]byte(in)); got != want {
				t.Errorf("%q: got %q want %q", in, got, want)
			}
		})
	}
}

func TestFailureReason(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for _, c := range []struct {
		code int
		err  error
		want string
	}{
		{500, nil, ""},
		{302, nil, "redirect"},
		{0, refusedURL{errors.New("url must be public")}, "refused address"},
		{0, &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errPrivateAddress}}, "refused address"},
		{0, &url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}, "timeout"},
		{0, &url.Error{Op: "Post", URL: "https://x", Err: refused}, "connection refused"},
		{0, &url.Error{Op: "Post", URL: "https://x", Err: x509.UnknownAuthorityError{}}, "TLS"},
		{0, &url.Error{Op: "Post", URL: "https://x", Err: errors.New("lookup x: no such host")}, "connection failed"},
	} {
		t.Run(c.want, func(t *testing.T) {
			if got := FailureReason(c.code, c.err); got != c.want {
				t.Errorf("%d %v: %q, want %q", c.code, c.err, got, c.want)
			}
		})
	}
}

// The error a merchant sees never names the address its endpoint resolved to or our resolver.
func TestErrorTextHidesOurNetwork(t *testing.T) {
	private := &url.Error{Op: "Post", URL: "https://shop.example", Err: &net.OpError{Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 443}, Err: errPrivateAddress}}
	lookup := &url.Error{Op: "Post", URL: "https://shop.example", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "api.railway.internal", Server: "10.0.0.53:53", IsNotFound: true}}}
	slow := &url.Error{Op: "Post", URL: "https://shop.example", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "i/o timeout", Name: "shop.example", Server: "10.0.0.53:53", IsTimeout: true}}}
	for _, c := range []struct {
		err  error
		want string
	}{
		{private, "refused address: the endpoint resolves to a non-public address"},
		{lookup, "connection failed: the endpoint host name could not be resolved"},
		{slow, "timeout: the endpoint host name could not be resolved"},
		{errors.New("HTTP/1.x transport connection broken"), "HTTP/1.x transport connection broken"},
	} {
		if got := errorText(c.err); got != c.want {
			t.Errorf("errorText(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A real refused connection is named as such on every platform (Windows has its own errno).
func TestFailureReasonOfARealRefusal(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = (&http.Client{Timeout: 5 * time.Second}).Post("http://"+addr, "application/json", nil)
	if got := FailureReason(0, err); got != "connection refused" {
		t.Fatalf("%v: %q", err, got)
	}
}

// Every background delivery error reaches the next Start, not only the first.
func TestBackgroundErrorsAreAllKept(t *testing.T) {
	d := &Dispatcher{}
	e1, e2 := errors.New("first"), errors.New("second")
	d.keepErr(e1)
	d.keepErr(nil)
	d.keepErr(e2)
	got := d.takeErr()
	if !errors.Is(got, e1) || !errors.Is(got, e2) {
		t.Fatalf("kept %v", got)
	}
	if d.takeErr() != nil {
		t.Fatal("errors kept after they were taken")
	}
}
