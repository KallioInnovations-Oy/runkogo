package runko

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowedHosts_Allowed(t *testing.T) {
	handler := AllowedHosts(AllowedHostsConfig{
		Hosts: []string{"example.com", "api.example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("allowed host: status = %d, want 200", rec.Code)
	}
}

func TestAllowedHosts_Blocked(t *testing.T) {
	handler := AllowedHosts(AllowedHostsConfig{
		Hosts: []string{"example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("blocked host: status = %d, want 421", rec.Code)
	}

	var result map[string]map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	if result["error"]["code"] != "invalid_host" {
		t.Errorf("error code = %q, want %q", result["error"]["code"], "invalid_host")
	}
}

func TestAllowedHosts_StripPort(t *testing.T) {
	handler := AllowedHosts(AllowedHostsConfig{
		Hosts: []string{"example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.com:8080"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("host with port: status = %d, want 200", rec.Code)
	}
}

func TestAllowedHosts_CaseInsensitive(t *testing.T) {
	handler := AllowedHosts(AllowedHostsConfig{
		Hosts: []string{"example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "Example.COM"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("case-insensitive host: status = %d, want 200", rec.Code)
	}
}

func TestAllowedHosts_EmptyConfig_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty AllowedHosts config, got none")
		}
	}()
	AllowedHosts(AllowedHostsConfig{})
}

func TestAllowedHosts_IPv6WithPort(t *testing.T) {
	handler := AllowedHosts(AllowedHostsConfig{
		Hosts: []string{"::1"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "[::1]:8080"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("IPv6 host with port: status = %d, want 200", rec.Code)
	}
}
