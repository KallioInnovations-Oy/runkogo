package runko

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Map is a convenience alias for building JSON response payloads.
//
// Example:
//
//	runko.JSON(w, 200, runko.Map{"user": user, "token": token})
type Map map[string]any

// JSON writes a JSON response with the given status code and sets
// Content-Type automatically.
func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			slog.Default().Error("runko: failed to encode JSON response", "error", err)
		}
	}
}

// Error writes a standardized JSON error envelope.
//
//	{"error": {"code": "not_found", "message": "User not found"}}
//
// The message MUST be a generic, public-safe string: never include user
// input, stack traces, hostnames, connection strings, or raw library
// errors. When you have an internal error to record alongside the public
// response, call ErrorLog instead.
func Error(w http.ResponseWriter, status int, code string, message string) {
	JSON(w, status, Map{
		"error": Map{
			"code":    code,
			"message": message,
		},
	})
}

// ErrorLog writes a public error response and records the internal error
// against the request's logger, enriched with request_id and trace_id from
// context. The public response is identical to Error's; only the server
// sees the internal detail.
//
// Example:
//
//	if err := db.Create(ctx, user); err != nil {
//	    runko.ErrorLog(w, r, logger, 500, "store_error", "Failed to create user", err)
//	    return
//	}
func ErrorLog(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, code string, message string, internal error) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{"code", code, "status", status, "error", internal}
	if r != nil {
		logger = LogWithContext(logger, r.Context())
		attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
	}
	logger.Error("handler error", attrs...)
	Error(w, status, code, message)
}

// ErrorWithDetails writes an error response with additional detail fields,
// useful for validation errors that list failing fields. Detail values
// must be server-controlled — never echo raw user input here.
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

// Created writes a 201 with a Location header pointing to the new resource.
// Rejects locations containing CR/LF to prevent header injection — callers
// must pre-sanitize or this panics at startup-level fail-fast. Handlers
// should construct Location values from trusted server-side data (e.g.,
// a newly minted UUID), not raw user input.
func Created(w http.ResponseWriter, location string, data any) {
	if location != "" {
		if strings.ContainsAny(location, "\r\n") {
			panic("runko: Created() Location contains CR/LF — possible header injection")
		}
		w.Header().Set("Location", location)
	}
	JSON(w, http.StatusCreated, data)
}

// NoContent responds with 204 (successful DELETE, etc.).
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
	if perPage <= 0 {
		perPage = 1
	}
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

// Decode reads and validates a JSON request body into target. Returns an
// error if the body is malformed, too large, or contains trailing data.
// The body is limited to 1 MB; use DecodeWithLimit for a custom limit.
func Decode(w http.ResponseWriter, r *http.Request, target any) error {
	return DecodeWithLimit(w, r, target, 1<<20)
}

// DecodeWithLimit reads a JSON body with a custom size limit.
//
// If BodyLimit middleware is active, the stricter of the two limits wins.
// Rejects malformed JSON, unknown fields, and trailing data after the
// JSON value (which would allow payload smuggling).
func DecodeWithLimit(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}

	if decoder.More() {
		return fmt.Errorf("request body contains trailing data after JSON value")
	}

	return nil
}
