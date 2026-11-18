package service

import (
	"strings"
	"testing"
)

func TestCheckWebhookURL(t *testing.T) {
	strict := New(nil, Config{CheckoutBaseURL: "http://api.internal:8080/"})
	local := New(nil, Config{CheckoutBaseURL: "http://api.internal:8080", AllowPrivateHooks: true})
	inbox := "http://api.internal:8080/v1/sandbox/inbox/9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"
	cases := []struct {
		url        string
		live, test bool
		// liveLocal is the live answer when private networks are allowed (local development).
		liveLocal bool
	}{
		{"https://shop.example/webhooks", true, true, true},
		{"https://shop.example:8443/webhooks?x=1", true, true, true},
		{"http://shop.example/webhooks", false, false, false},
		{"http://localhost:3000/webhooks", false, true, false},
		{"http://LOCALHOST:3000/webhooks", false, true, false},
		{"http://app.localhost/webhooks", false, true, false},
		{"http://127.0.0.1:3000/webhooks", false, true, false},
		{"http://127.1.2.3/webhooks", false, true, false},
		{"http://[::1]:3000/webhooks", false, true, false},
		{"https://localhost/webhooks", false, true, true},
		{"https://127.0.0.1/webhooks", false, true, true},
		{"https://10.1.2.3/webhooks", false, true, true},
		{"https://192.168.1.10/webhooks", false, true, true},
		{"https://169.254.169.254/latest/meta-data", false, true, true},
		{"https://[fe80::1]/webhooks", false, true, true},
		{"https://0.0.0.0/webhooks", false, true, true},
		{"https://0.0.0.1/webhooks", false, true, true},
		{"https://100.100.100.200/latest/meta-data", false, true, true},
		{"https://100.64.0.1/webhooks", false, true, true},
		{"https://192.0.0.8/webhooks", false, true, true},
		{"https://198.18.0.1/webhooks", false, true, true},
		{"https://240.0.0.1/webhooks", false, true, true},
		{"https://[64:ff9b::a9fe:a9fe]/webhooks", false, true, true},
		{"https://[64:ff9b:1::a00:1]/webhooks", false, true, true},
		{"https://[2002:a00:1::1]/webhooks", false, true, true},
		{"https://[100::1]/webhooks", false, true, true},
		{"https://[::ffff:169.254.169.254]/webhooks", false, true, true},
		{"https://[fe80::1%25eth0]/webhooks", false, true, true},
		{"https://93.184.216.34/webhooks", true, true, true},
		{"https://[2606:2800:220:1:248:1893:25c8:1946]/webhooks", true, true, true},
		{"https://intranet/webhooks", false, true, true},
		{"https://intranet./webhooks", false, true, true},
		{"https://api.internal/webhooks", false, true, true},
		{"https://postgres.railway.INTERNAL/webhooks", false, true, true},
		{"https://printer.local./webhooks", false, true, true},
		{"https://shop.internal.example/webhooks", true, true, true},
		{"https://shop.localhost.example/webhooks", true, true, true},
		{"http://10.1.2.3/webhooks", false, false, false},
		{"http://localhost.evil.example/webhooks", false, false, false},
		{"http://127.0.0.1.nip.io/webhooks", false, false, false},
		{inbox, false, true, false},
		{"http://api.internal:8080/v1/sandbox/inbox/", false, false, false},
		{"http://api.internal:8080/v1/sandbox/inbox/a/b", false, false, false},
		{"http://api.internal:8080/v1/sandbox/inbox/a#frag", false, false, false},
		{"http://evil.example/v1/sandbox/inbox/9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin", false, false, false},
		{"https://user:pw@shop.example/webhooks", false, false, false},
		{"ftp://shop.example/webhooks", false, false, false},
		{"/webhooks", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			if got := strict.checkWebhookURL(c.url, true) == nil; got != c.live {
				t.Errorf("live %q: accepted=%v", c.url, got)
			}
			if got := strict.checkWebhookURL(c.url, false) == nil; got != c.test {
				t.Errorf("test %q: accepted=%v", c.url, got)
			}
			if got := local.checkWebhookURL(c.url, true) == nil; got != c.liveLocal {
				t.Errorf("live with private networks allowed %q: accepted=%v", c.url, got)
			}
		})
	}
}

func TestClientReferencePattern(t *testing.T) {
	long := strings.Repeat("z", 128)
	for _, ok := range []string{"order-42", "a", "A.b_c:d-9", "user:f36361bc-0000", "0", long} {
		if validateClientReference(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", long + "z", "order 42", "order/42", "a?b", "a#b", "a&b=c", "pedido-é", "<script>", "a\nb", "a\x00b",
		"%20", "a+b", "order-42\n"} {
		if validateClientReference(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
