package runko

import "testing"

func TestStripPort(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ipv4 with port", "1.2.3.4:8080", "1.2.3.4"},
		{"ipv4 no port", "1.2.3.4", "1.2.3.4"},
		{"ipv6 with port bracketed", "[::1]:8080", "::1"},
		{"ipv6 no port bracketed", "[::1]", "::1"},
		{"ipv6 no brackets", "::1", "::1"},
		{"ipv6 full with port", "[2001:db8::1]:443", "2001:db8::1"},
		{"empty", "", ""},
		{"localhost with port", "127.0.0.1:3000", "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripPort(tt.input)
			if got != tt.want {
				t.Errorf("stripPort(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProxyResolver_NoTrustedProxies(t *testing.T) {
	pr := newProxyResolver(nil)

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			"direct connection, no XFF",
			"1.2.3.4:8080", "",
			"1.2.3.4",
		},
		{
			"XFF present but ignored",
			"1.2.3.4:8080", "6.6.6.6",
			"1.2.3.4",
		},
		{
			"XFF chain present but ignored",
			"1.2.3.4:8080", "6.6.6.6, 7.7.7.7, 8.8.8.8",
			"1.2.3.4",
		},
		{
			"localhost with spoofed XFF",
			"127.0.0.1:9999", "10.0.0.1",
			"127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pr.resolveClientIP(tt.remoteAddr, tt.xff)
			if got != tt.want {
				t.Errorf("resolveClientIP(%q, %q) = %q, want %q",
					tt.remoteAddr, tt.xff, got, tt.want)
			}
		})
	}
}

func TestProxyResolver_WithTrustedProxies(t *testing.T) {
	pr := newProxyResolver([]string{
		"10.0.0.0/8",    // Private network.
		"172.17.0.1",    // Docker host.
		"192.168.1.100", // Specific proxy.
	})

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			"direct from untrusted — ignore XFF",
			"5.5.5.5:8080", "6.6.6.6",
			"5.5.5.5",
		},
		{
			"trusted proxy, single XFF entry",
			"10.0.0.1:8080", "203.0.113.50",
			"203.0.113.50",
		},
		{
			"trusted proxy, XFF chain — rightmost untrusted",
			"10.0.0.1:8080", "203.0.113.50, 10.0.0.2",
			"203.0.113.50",
		},
		{
			"trusted proxy, multi-hop all trusted except client",
			"10.0.0.1:8080", "8.8.8.8, 10.0.0.5, 10.0.0.3",
			"8.8.8.8",
		},
		{
			"trusted proxy, no XFF header",
			"10.0.0.1:8080", "",
			"10.0.0.1",
		},
		{
			"docker host proxy with XFF",
			"172.17.0.1:8080", "192.0.2.1",
			"192.0.2.1",
		},
		{
			"specific trusted proxy IP",
			"192.168.1.100:8080", "198.51.100.25",
			"198.51.100.25",
		},
		{
			"entire XFF chain is trusted — return leftmost",
			"10.0.0.1:8080", "10.0.0.10, 10.0.0.20",
			"10.0.0.10",
		},
		{
			"XFF with spaces",
			"10.0.0.1:8080", " 203.0.113.50 , 10.0.0.2 ",
			"203.0.113.50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pr.resolveClientIP(tt.remoteAddr, tt.xff)
			if got != tt.want {
				t.Errorf("resolveClientIP(%q, %q) = %q, want %q",
					tt.remoteAddr, tt.xff, got, tt.want)
			}
		})
	}
}

func TestProxyResolver_IsTrusted(t *testing.T) {
	pr := newProxyResolver([]string{
		"10.0.0.0/8",
		"127.0.0.1",
	})

	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"127.0.0.1", true},
		{"127.0.0.2", false},
		{"192.168.1.1", false},
		{"8.8.8.8", false},
		{"", false},
		{"not-an-ip", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			got := pr.isTrusted(tt.ip)
			if got != tt.want {
				t.Errorf("isTrusted(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestProxyResolver_InvalidEntry_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for invalid proxy entry, got none")
		}
	}()
	newProxyResolver([]string{"not-a-valid-ip-or-cidr"})
}
