package runko

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Map is a convenience alias for building JSON response payloads.
//
// Example:
//
//	runko.JSON(w, 200, runko.Map{"user": user, "token": token})
type Map map[string]any

// JSON writes a JSON response with the given status code.
// Sets Content-Type to application/json automatically.
func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

// Error writes a standardized JSON error response. Every error from
// every service in the cluster has the same shape, making client-side
// error handling consistent.
//
// Output format:
//
//	{
//	    "error": {
//	        "code": "not_found",
//	        "message": "User not found"
//	    }
//	}
func Error(w http.ResponseWriter, status int, code string, message string) {
	JSON(w, status, Map{
		"error": Map{
			"code":    code,
			"message": message,
		},
	})
}

// ErrorWithDetails writes an error response with additional detail fields.
// Useful for validation errors where you want to tell the client which
// fields failed.
//
// Example:
//
//	runko.ErrorWithDetails(w, 422, "validation_error", "Invalid input", runko.Map{
//	    "fields": []string{"email", "name"},
//	})
func ErrorWithDetails(w http.ResponseWriter, status int, code string, message string, details Map) {
	payload := Map{
		"error": Map{
			"code":    code,
			"message": message,
			"details": details,
		},
	}
	JSON(w, status, payload)
}

// Created writes a 201 Created response with the given data and
// Location header pointing to the new resource.
func Created(w http.ResponseWriter, location string, data any) {
	if location != "" {
		w.Header().Set("Location", location)
	}
	JSON(w, http.StatusCreated, data)
}

// NoContent responds with 204 (successful DELETE, etc.)
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// PaginatedResponse is the standard envelope for paginated list endpoints.
type PaginatedResponse struct {
	Data       any            `json:"data"`
	Pagination PaginationMeta `json:"pagination"`
}

// PaginationMeta contains pagination metadata.
type PaginationMeta struct {
	Page       int  `json:"page"`
	PerPage    int  `json:"per_page"`
	Total      int  `json:"total"`
	TotalPages int  `json:"total_pages"`
	HasMore    bool `json:"has_more"`
}

// Paginated writes a paginated JSON response.
func Paginated(w http.ResponseWriter, data any, page, perPage, total int) {
	totalPages := total / perPage
	if total%perPage != 0 {
		totalPages++
	}

	JSON(w, http.StatusOK, PaginatedResponse{
		Data: data,
		Pagination: PaginationMeta{
			Page:       page,
			PerPage:    perPage,
			Total:      total,
			TotalPages: totalPages,
			HasMore:    page < totalPages,
		},
	})
}

// Decode reads and validates a JSON request body into the target struct.
// Returns an error if the body is malformed, too large, or contains
// trailing data after the JSON value.
//
// The body is limited to 1MB by default. Use DecodeWithLimit for a
// custom size limit.
func Decode(r *http.Request, target any) error {
	return DecodeWithLimit(r, target, 1<<20) // 1MB
}

// DecodeWithLimit reads a JSON request body with a custom size limit.
// The maxBytes parameter controls the maximum body size in bytes.
//
// Note: if BodyLimit middleware is active, the stricter of the two
// limits wins. BodyLimit(1MB) + DecodeWithLimit(10MB) = 1MB effective.
//
// Returns an error if:
//   - The body exceeds maxBytes (or the BodyLimit, whichever is smaller)
//   - The JSON is malformed
//   - Unknown fields are present
//   - Multiple JSON values are present (trailing data)
func DecodeWithLimit(r *http.Request, target any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}

	// Reject bodies with trailing data after the JSON value.
	// This prevents payload smuggling where extra data is appended
	// after a valid JSON object.
	if decoder.More() {
		return fmt.Errorf("request body contains trailing data after JSON value")
	}

	return nil
}
