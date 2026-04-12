package runko

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode_ValidJSON(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	body := `{"name":"Ville","email":"ville@test.com"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	var p payload
	err := Decode(req, &p)
	if err != nil {
		t.Fatalf("Decode valid JSON: unexpected error: %v", err)
	}
	if p.Name != "Ville" || p.Email != "ville@test.com" {
		t.Errorf("Decode result = %+v, want Name=Ville, Email=ville@test.com", p)
	}
}

func TestDecode_TrailingData_Rejected(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	body := `{"name":"Ville"}{"extra":"payload"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))

	var p payload
	err := Decode(req, &p)
	if err == nil {
		t.Fatal("Decode with trailing data should return error")
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("error should mention trailing data, got: %v", err)
	}
}

func TestDecode_UnknownFields_Rejected(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	body := `{"name":"Ville","unknown_field":"value"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))

	var p payload
	err := Decode(req, &p)
	if err == nil {
		t.Fatal("Decode with unknown fields should return error")
	}
}

func TestDecode_Oversized_Rejected(t *testing.T) {
	type payload struct {
		Data string `json:"data"`
	}

	// DecodeWithLimit at 100 bytes, body is 200+ bytes.
	bigData := strings.Repeat("x", 200)
	body := `{"data":"` + bigData + `"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))

	var p payload
	err := DecodeWithLimit(req, &p, 100)
	if err == nil {
		t.Fatal("DecodeWithLimit should reject oversized body")
	}
}

func TestJSON_Response(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusOK, Map{"key": "value"})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	if result["key"] != "value" {
		t.Errorf("body key = %q, want %q", result["key"], "value")
	}
}

func TestError_Response(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, http.StatusNotFound, "not_found", "User not found")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	var result map[string]map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	if result["error"]["code"] != "not_found" {
		t.Errorf("error code = %q, want %q", result["error"]["code"], "not_found")
	}
	if result["error"]["message"] != "User not found" {
		t.Errorf("error message = %q, want %q", result["error"]["message"], "User not found")
	}
}

func TestCreated_LocationHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	Created(rec, "/api/v1/users/42", Map{"id": "42"})

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/users/42" {
		t.Errorf("Location = %q, want /api/v1/users/42", loc)
	}
}

func TestNoContent_Response(t *testing.T) {
	rec := httptest.NewRecorder()
	NoContent(rec)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %d bytes", rec.Body.Len())
	}
}

func TestPaginated_Response(t *testing.T) {
	rec := httptest.NewRecorder()
	data := []string{"a", "b", "c"}
	Paginated(rec, data, 1, 10, 25)

	var result PaginatedResponse
	json.NewDecoder(rec.Body).Decode(&result)

	if result.Pagination.Page != 1 {
		t.Errorf("page = %d, want 1", result.Pagination.Page)
	}
	if result.Pagination.TotalPages != 3 {
		t.Errorf("total_pages = %d, want 3", result.Pagination.TotalPages)
	}
	if !result.Pagination.HasMore {
		t.Error("has_more should be true for page 1 of 3")
	}
}

func TestPaginated_LastPage(t *testing.T) {
	rec := httptest.NewRecorder()
	Paginated(rec, []string{"z"}, 3, 10, 25)

	var result PaginatedResponse
	json.NewDecoder(rec.Body).Decode(&result)

	if result.Pagination.HasMore {
		t.Error("has_more should be false on the last page")
	}
}
