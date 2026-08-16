package pkg

import "net"

// IsInnerIP reports whether host is a loopback, unspecified, private (RFC1918),
// or CGNAT (100.64.0.0/10) address — i.e. reachable only on this machine or an
// internal network. Used by model.IsLocalBaseURL to decide whether a model
// endpoint can skip credentials.
//
// Accepts both IPv4 and IPv6 ("127.0.0.1", "::1"); non-IP hostnames return
// false.
func IsInnerIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() || isCGNAT(ip)
}

// isCGNAT matches the carrier-grade NAT range 100.64.0.0/10.
func isCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 100 && ip4[1]&0xC0 == 0x40
}

