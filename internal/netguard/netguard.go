// Package netguard decides which IP addresses a request made on a merchant's behalf (a webhook
// delivery) may reach: never this machine, a private, shared or reserved network, or an address
// that a translation gateway maps onto one.
package netguard

import "net/netip"

var denied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // shared address space (CGNAT, Tailscale, some cloud metadata)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local (cloud metadata)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation (TEST-NET-1)
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation (TEST-NET-2)
		"203.0.113.0/24",  // documentation (TEST-NET-3)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, broadcast
		"::/128",          // unspecified
		"::1/128",         // loopback
		"::/96",           // IPv4-compatible (deprecated), which embeds any IPv4 address
		"64:ff9b::/96",    // NAT64, well-known prefix
		"64:ff9b:1::/48",  // NAT64, local-use prefix
		"100::/64",        // discard-only
		"2002::/16",       // 6to4, which embeds any IPv4 address
		"fc00::/7",        // unique local
		"fec0::/10",       // site-local (deprecated)
		"fe80::/10",       // link-local
		"ff00::/8",        // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Public reports whether ip is an address a public endpoint may have. IPv4-mapped IPv6 addresses
// are judged as the IPv4 address they carry.
func Public(ip netip.Addr) bool {
	// A zoned address never matches a prefix, so the zone goes first.
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return false
	}
	for _, p := range denied {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
