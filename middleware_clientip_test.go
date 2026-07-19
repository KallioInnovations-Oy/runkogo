package runko

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// clientIPFor runs ClientIPMiddleware and reports the IP it resolved.
func clientIPFor(proxy *proxyResolver, remoteAddr string, xffLines []string) string {
	var got string
	h := ClientIPMiddleware(proxy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ClientIP(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remoteAddr
	for _, line := range xffLines {
		req.Header.Add("X-Forwarded-For", line)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// CONV-01: a client must not be able to choose its own resolved IP.
//
// Go stores repeated headers as separate values. Some proxies append a new
// X-Forwarded-For line rather than extending the existing one, so reading
// only the first line would hand the attacker's forged entry straight
// through — spoofing rate-limit buckets, IP allowlists and audit logs.
func TestClientIPMiddleware_MultipleXFFHeaderLines(t *testing.T) {
	proxy := newProxyResolver([]string{"10.0.0.0/8"})

	got := clientIPFor(proxy, "10.0.0.1:1234", []string{
		"9.9.9.9",     // forged by the client, arrives first
		"203.0.113.7", // appended by the trusted proxy
	})

	if got != "203.0.113.7" {
		t.Errorf("resolved client IP = %q, want %q — a forged header line was trusted", got, "203.0.113.7")
	}
}

func TestClientIPMiddleware_SingleLineChain(t *testing.T) {
	proxy := newProxyResolver([]string{"10.0.0.0/8"})

	got := clientIPFor(proxy, "10.0.0.1:1234", []string{"203.0.113.7, 10.0.0.2"})
	if got != "203.0.113.7" {
		t.Errorf("resolved client IP = %q, want %q", got, "203.0.113.7")
	}
}

// With no trusted proxies configured, forwarding headers are ignored
// entirely — the secure default.
func TestClientIPMiddleware_IgnoresXFFWithoutTrustedProxies(t *testing.T) {
	proxy := newProxyResolver(nil)

	got := clientIPFor(proxy, "10.0.0.1:1234", []string{"9.9.9.9"})
	if got != "10.0.0.1" {
		t.Errorf("resolved client IP = %q, want %q — XFF was honoured without trusted proxies", got, "10.0.0.1")
	}
}
