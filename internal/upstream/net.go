package upstream

import (
	"net"
	"net/netip"
	"strings"
)

// IsPrivateHost: whether host is, or resolves to, a loopback, private,
// link-local, CGNAT, multicast or unspecified address. Unresolvable hosts
// count as private.
func IsPrivateHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return isPrivateAddr(a)
	}
	addrs, err := net.LookupHost(h)
	if err != nil || len(addrs) == 0 {
		return true
	}
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil || isPrivateAddr(a) {
			return true
		}
	}
	return false
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPrivateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() ||
		(a.Is4() && (cgnat.Contains(a) || a.As4()[0] == 0 || a.As4()[0] >= 224))
}
