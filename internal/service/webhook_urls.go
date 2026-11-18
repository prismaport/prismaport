package service

import (
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/prismaport/prismaport/internal/netguard"
	"github.com/prismaport/prismaport/internal/store"
)

// loopbackHost reports whether host names this machine: localhost, *.localhost or a loopback IP.
func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// nonPublicHost reports whether host is this machine, an IP literal no public endpoint has (the
// dispatcher's dial-time rule) or a name only a private network resolves: a single label
// ("intranet") or one under .internal or .local. Other names that resolve to non-public addresses
// are refused at dial time.
func nonPublicHost(host string) bool {
	if loopbackHost(host) {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return !netguard.Public(ip)
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	return !strings.Contains(name, ".") || strings.HasSuffix(name, ".internal") || strings.HasSuffix(name, ".local")
}

// sandboxInboxURL reports whether raw is a hosted inbox address of this deployment.
func (s *Service) sandboxInboxURL(raw string) bool {
	token, ok := strings.CutPrefix(raw, s.inboxBase())
	return ok && token != "" && !strings.ContainsAny(token, "/?#")
}

// checkWebhookURL applies the endpoint URL rules:
//   - live mode: https only, and not this machine, a private, link-local or unspecified IP, or a
//     single-label, .internal or .local name (unless private networks are allowed, for local
//     development);
//   - test mode: https anywhere, plain http only to this machine (localhost, *.localhost, a
//     loopback IP) or to a sandbox inbox of this deployment.
//
// Whatever the URL, deliveries never dial a non-public address unless private networks are
// allowed, and never follow redirects.
func (s *Service) checkWebhookURL(raw string, livemode bool) error {
	if err := validateURL("url", raw, true); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("url must be an absolute URL without credentials")
	}
	host := u.Hostname()
	if livemode {
		if u.Scheme != "https" {
			return invalid("live webhook endpoints must use https://")
		}
		if !s.cfg.AllowPrivateHooks && nonPublicHost(host) {
			return invalid("url must be a public address: Prismaport never delivers to localhost, private or link-local addresses")
		}
		return nil
	}
	if u.Scheme == "https" || loopbackHost(host) || s.sandboxInboxURL(raw) {
		return nil
	}
	return invalid("url must use https://; plain http:// is accepted only in test mode, for localhost or the sandbox inbox")
}

// DeliverableURL refuses a delivery to an endpoint whose URL the rules no longer accept (one
// registered before them), so no live event ever travels over plain http.
func (s *Service) DeliverableURL(e store.WebhookEndpoint) error {
	return s.checkWebhookURL(e.URL, e.Livemode)
}
