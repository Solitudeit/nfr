package transport

import "net"

// BestHost picks the address an operator on another machine should type.
// Priority: configured domain, then an explicit non-wildcard bind address,
// then a global unicast address from the local interfaces, then the first
// private LAN address, then loopback. Agents usually reach the server over
// the public internet, so public-looking addresses win over LAN ones.
func BestHost(domain, listenHost string) string {
	if domain != "" {
		return domain
	}
	switch listenHost {
	case "", "0.0.0.0", "::", "[::]", "localhost", "127.0.0.1":
	default:
		return listenHost // explicit bind address wins
	}
	var v4pub, v4priv, v6pub, v6priv, other string
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			s := ip.String()
			v4 := ip.To4() != nil
			switch {
			case isPublicUnicast(ip) && v4:
				if v4pub == "" {
					v4pub = s
				}
			case ip.IsPrivate() && v4:
				if v4priv == "" {
					v4priv = s
				}
			case isPublicUnicast(ip):
				if v6pub == "" {
					v6pub = s
				}
			case ip.IsPrivate() || isCGNAT(ip) || ip.IsLinkLocalUnicast():
				if v6priv == "" {
					v6priv = s
				}
			default:
				if other == "" {
					other = s
				}
			}
		}
	}
	// IPv4 wins over IPv6: a bare v6 literal in a printed command is awkward
	// to paste and unreachable from networks without v6.
	for _, candidate := range []string{v4pub, v4priv, v6pub, v6priv, other} {
		if candidate != "" {
			return candidate
		}
	}
	return "localhost"
}

// isCGNAT reports whether ip is in 100.64.0.0/10 (carrier-grade NAT).
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] < 128
}

// isPublicUnicast reports whether ip is globally routable, excluding
// private, CGNAT, link-local and documentation ranges.
func isPublicUnicast(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || isCGNAT(ip) {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 (documentation)
		return !(v4[0] == 192 && v4[1] == 0 && v4[2] == 2 ||
			v4[0] == 198 && v4[1] == 51 && v4[2] == 100 ||
			v4[0] == 203 && v4[1] == 0 && v4[2] == 113)
	}
	// 2001:db8::/32 documentation prefix
	return !(len(ip) == 16 && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8)
}
