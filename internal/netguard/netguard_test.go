package netguard

import (
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	refused := map[string]string{
		"0.0.0.1":                "this network",
		"10.1.2.3":               "private 10/8",
		"100.64.0.1":             "CGNAT, low end",
		"100.100.100.200":        "CGNAT, Alibaba Cloud metadata",
		"100.127.255.254":        "CGNAT, high end",
		"127.0.0.1":              "loopback",
		"169.254.169.254":        "link-local metadata",
		"172.31.255.254":         "private 172.16/12",
		"192.0.0.8":              "IETF protocol assignments",
		"192.168.1.1":            "private 192.168/16",
		"198.18.0.1":             "benchmarking, low end",
		"198.19.255.254":         "benchmarking, high end",
		"224.0.0.1":              "multicast",
		"240.0.0.1":              "reserved",
		"255.255.255.255":        "broadcast",
		"::":                     "unspecified",
		"::1":                    "loopback",
		"64:ff9b::a9fe:a9fe":     "NAT64 of 169.254.169.254",
		"64:ff9b::808:808":       "NAT64 of a public address",
		"64:ff9b:1::a00:1":       "NAT64 local-use",
		"100::1":                 "discard-only",
		"2002:a00:1::1":          "6to4 of 10.0.0.1",
		"fd00::1":                "unique local",
		"fe80::1":                "link-local",
		"fe80::1%eth0":           "link-local with a zone",
		"ff02::1":                "multicast",
		"ff01::1":                "interface-local multicast",
		"::ffff:127.0.0.1":       "IPv4-mapped loopback",
		"::ffff:100.100.100.200": "IPv4-mapped CGNAT",
		"::ffff:a9fe:a9fe":       "IPv4-mapped metadata, hex form",
		"::ffff:198.18.0.1":      "IPv4-mapped benchmarking",
		"::ffff:0.0.0.0":         "IPv4-mapped unspecified",
		"::ffff:10.0.0.1%eth0":   "IPv4-mapped private with a zone",
		"192.0.2.10":             "documentation TEST-NET-1",
		"198.51.100.7":           "documentation TEST-NET-2",
		"203.0.113.250":          "documentation TEST-NET-3",
		"::a9fe:a9fe":            "IPv4-compatible metadata",
		"::10.0.0.1":             "IPv4-compatible private",
		"::8.8.8.8":              "IPv4-compatible of a public address",
		"fec0::1":                "site-local",
		"feff::1":                "site-local, high end",
	}
	for s, why := range refused {
		t.Run(s, func(t *testing.T) {
			if Public(netip.MustParseAddr(s)) {
				t.Errorf("%s (%s) is treated as public", s, why)
			}
		})
	}
	allowed := map[string]string{
		"93.184.216.34":                      "public IPv4",
		"8.8.8.8":                            "public IPv4",
		"1.0.0.1":                            "public IPv4 just above 0/8",
		"100.63.255.255":                     "just below CGNAT",
		"100.128.0.0":                        "just above CGNAT",
		"172.32.0.1":                         "just above 172.16/12",
		"192.0.1.1":                          "just above 192.0.0/24",
		"198.17.255.255":                     "just below benchmarking",
		"198.20.0.1":                         "just above benchmarking",
		"223.255.255.254":                    "just below multicast",
		"::ffff:93.184.216.34":               "IPv4-mapped public",
		"2606:2800:220:1:248:1893:25c8:1946": "public IPv6",
		"2001:4860:4860::8888":               "public IPv6",
		"64:ff9b:0:1::1":                     "just outside the NAT64 /96",
		"100:0:0:1::1":                       "just outside discard-only /64",
		"2003::1":                            "just above 6to4",
		"192.0.3.1":                          "just above TEST-NET-1",
		"198.51.99.255":                      "just below TEST-NET-2",
		"198.51.101.0":                       "just above TEST-NET-2",
		"203.0.112.255":                      "just below TEST-NET-3",
		"203.0.114.0":                        "just above TEST-NET-3",
		"::1:0:0:1":                          "just outside the IPv4-compatible /96",
	}
	for s, why := range allowed {
		t.Run(s, func(t *testing.T) {
			if !Public(netip.MustParseAddr(s)) {
				t.Errorf("%s (%s) is refused", s, why)
			}
		})
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address is treated as public")
	}
}
