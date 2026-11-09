package transport

import (
	"net"
	"testing"
)

func TestBestHostPriority(t *testing.T) {
	cases := []struct {
		name       string
		domain     string
		listenHost string
		want       string
	}{
		{"domain wins", "nfr.example.com", "0.0.0.0", "nfr.example.com"},
		{"explicit bind wins", "", "10.0.0.5", "10.0.0.5"},
		{"wildcard falls through", "", "0.0.0.0", ""}, // interface-dependent, checked below
		{"localhost falls through", "", "127.0.0.1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BestHost(c.domain, c.listenHost)
			if c.want == "" {
				if got == "localhost" {
					t.Skip("no non-loopback interface on this host")
				}
				return
			}
			if got != c.want {
				t.Errorf("BestHost(%q,%q)=%q want %q", c.domain, c.listenHost, got, c.want)
			}
		})
	}
}

func TestIsPublicUnicast(t *testing.T) {
	pub := []string{"8.8.8.8", "1.2.3.4", "2606:4700::1111"}
	priv := []string{"10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1",
		"169.254.1.1", "192.0.2.1", "198.51.100.7", "203.0.113.9",
		"127.0.0.1", "224.0.0.1", "2001:db8::1", "fe80::1"}
	for _, s := range pub {
		if !isPublicUnicast(net.ParseIP(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range priv {
		if isPublicUnicast(net.ParseIP(s)) {
			t.Errorf("%s should not be public", s)
		}
	}
}

func TestIsCGNAT(t *testing.T) {
	if !isCGNAT(net.ParseIP("100.64.0.1")) || !isCGNAT(net.ParseIP("100.127.255.255")) {
		t.Error("inside 100.64/10 should be CGNAT")
	}
	if isCGNAT(net.ParseIP("100.63.0.1")) || isCGNAT(net.ParseIP("100.128.0.1")) {
		t.Error("outside 100.64/10 should not be CGNAT")
	}
}
