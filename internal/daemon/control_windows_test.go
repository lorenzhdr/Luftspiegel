//go:build windows

package daemon

import "testing"

// The control channel is completely unauthenticated (see handleRequest), so
// normalizeControlAddr must never hand back an address that binds anything
// but loopback — otherwise "-socket :7654" would let anyone on the LAN start
// mirroring this screen.
func TestNormalizeControlAddrForcesLoopback(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty means default", "", "127.0.0.1:7654"},
		{"bare port", "7654", "127.0.0.1:7654"},
		{"explicit loopback is kept", "127.0.0.1:7654", "127.0.0.1:7654"},
		{"other loopback ip is kept", "127.0.0.2:9000", "127.0.0.2:9000"},
		{"localhost is kept", "localhost:7654", "localhost:7654"},
		{"ipv6 loopback is kept", "[::1]:7654", "[::1]:7654"},
		{"wildcard is forced to loopback", ":7654", "127.0.0.1:7654"},
		{"0.0.0.0 is forced to loopback", "0.0.0.0:7654", "127.0.0.1:7654"},
		{"lan address is forced to loopback", "192.168.1.5:7654", "127.0.0.1:7654"},
		{"ipv6 wildcard is forced to loopback", "[::]:7654", "127.0.0.1:7654"},
		{"non-loopback port is preserved", "0.0.0.0:9999", "127.0.0.1:9999"},
		{"bare non-loopback host falls back to default", "example.com", "127.0.0.1:7654"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeControlAddr(tt.in); got != tt.want {
				t.Errorf("normalizeControlAddr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"127.0.0.1", "127.5.5.5", "::1", "localhost", "LOCALHOST"}
	for _, h := range loopback {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	// An empty host is the wildcard bind, and an unresolvable name must fail
	// closed rather than open.
	notLoopback := []string{"", "0.0.0.0", "192.168.1.5", "::", "example.com", "not a host"}
	for _, h := range notLoopback {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}
