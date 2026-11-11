package crawler

import (
	"net"
	"testing"
)

func TestIsPrivateIPTable(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"10.0.0.1",
		"192.168.0.1",
		"172.16.0.1",
		"0.0.0.0",
		"255.255.255.255",
		"224.0.0.1",        // multicast
		"100.64.0.1",       // CGNAT
		"192.0.0.1",        // 192.0.0.0/24
		"198.18.0.1",       // benchmarking
		"240.0.0.1",        // reserved
		"169.254.169.254",  // link-local metadata
		"::1",
		"fc00::1",
		"fe80::1",
		"::ffff:10.0.0.1",
		"64:ff9b::192.0.2.1", // NAT64 to TEST-NET-1 (blocked CIDR)
		"64:ff9b::10.0.0.1",  // NAT64 to private v4
	}
	for _, s := range blocked {
		if !isPrivateIP(net.ParseIP(s)) {
			t.Errorf("isPrivateIP(%q) = false, want true", s)
		}
		if !isPrivateHost(s) {
			t.Errorf("isPrivateHost(%q) = false, want true", s)
		}
	}
	allowed := []string{
		"8.8.8.8",
		"93.184.216.34",
		"2001:4860:4860::8888",
	}
	for _, s := range allowed {
		if isPrivateIP(net.ParseIP(s)) {
			t.Errorf("isPrivateIP(%q) = true, want false", s)
		}
	}
}

func TestHostIPZoneID(t *testing.T) {
	if got := hostIP("fe80::1%lo"); got == nil || !isPrivateIP(got) {
		t.Fatalf("fe80::1%%lo should be link-local private, got %v", got)
	}
}

func TestHostIPDecimal(t *testing.T) {
	if !isPrivateHost("2130706433") {
		t.Error("decimal 127.0.0.1 encoding must be blocked")
	}
}
