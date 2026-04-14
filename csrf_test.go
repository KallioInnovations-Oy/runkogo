package runko

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSRF_IssuesTokenOnSafeMethod(t *testing.T) {
	handler := CSRF(CSRFConfig{})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET should pass through, got %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "csrf_token" {
			found = true
			if c.Value == "" {
				t.Error("csrf_token cookie value should not be empty")
			}
			if !c.Secure {
				t.Error("csrf_token cookie should be Secure by default")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite = %v, want Lax", c.SameSite)
			}
		}
	}
	if !found {
		t.Error("CSRF middleware should issue cookie on safe method when missing")
	}
}

func TestCSRF_DoesNotReissueWhenPresent(t *testing.T) {
	handler := CSRF(CSRFConfig{})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "existing-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == "csrf_token" {
			t.Error("CSRF middleware should not reissue token when one is present")
		}
	}
}

func TestCSRF_RejectsUnsafeWithoutToken(t *testing.T) {
	handler := CSRF(CSRFConfig{})(nopHandler)

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST without token: status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "csrf_missing") {
		t.Errorf("expected csrf_missing code in body, got %q", rec.Body.String())
	}
}

func TestCSRF_RejectsMismatchedToken(t *testing.T) {
	handler := CSRF(CSRFConfig{})(nopHandler)

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "cookie-value"})
	req.Header.Set("X-CSRF-Token", "different-value")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatched token: status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "csrf_invalid") {
		t.Errorf("expected csrf_invalid code in body, got %q", rec.Body.String())
	}
}

func TestCSRF_AcceptsMatchedToken(t *testing.T) {
	handler := CSRF(CSRFConfig{})(nopHandler)

	token := "matching-value-123"
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: token})
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("matched token: status = %d, want 200", rec.Code)
	}
}

func TestCSRF_SkipAuthHeaderBypasses(t *testing.T) {
	handler := CSRF(CSRFConfig{SkipAuthHeader: true})(nopHandler)

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer some-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("SkipAuthHeader with Authorization present: status = %d, want 200", rec.Code)
	}
}

func TestCSRF_SkipAuthHeaderDoesNotBypassWithoutAuthHeader(t *testing.T) {
	handler := CSRF(CSRFConfig{SkipAuthHeader: true})(nopHandler)

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("SkipAuthHeader without Authorization: status = %d, want 403", rec.Code)
	}
}

func TestCSRF_EmptyCookieRejected(t *testing.T) {
	handler := CSRF(CSRFConfig{})(nopHandler)

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: ""})
	req.Header.Set("X-CSRF-Token", "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("empty cookie + empty header: status = %d, want 403", rec.Code)
	}
}

func TestCSRF_InsecureOptOut(t *testing.T) {
	insecure := false
	handler := CSRF(CSRFConfig{Secure: &insecure})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == "csrf_token" && c.Secure {
			t.Error("Secure: &false should produce a non-Secure cookie")
		}
	}
}
