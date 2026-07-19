package runko

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// AppError is an error that knows how it should be rendered to a client.
//
// It exists to remove the ladder that otherwise appears in every handler:
//
//	if errors.Is(err, ErrNotFound) { Error(w, 404, "not_found", "..."); return }
//	if errors.Is(err, ErrConflict) { Error(w, 409, "conflict", "..."); return }
//	if err != nil                  { Error(w, 500, "internal", "...");  return }
//
// With AppError the decision is made once, where the error is created, and
// every handler ends the same way:
//
//	if err != nil {
//	    runko.RespondError(w, r, app.Logger, err)
//	    return
//	}
//
// The split between public and internal is the point. Status, Code,
// Message and Details are rendered to the client; Cause never is — it goes
// to the server log, correlated by request ID. This is CONV-03 expressed
// as a type rather than as a rule you have to remember.
//
// CONV-15: errors carry their own disclosure decision.
type AppError struct {
	// Status is the HTTP status code to send.
	Status int

	// Code is the stable, machine-readable error code clients switch on.
	Code string

	// Message is the human-readable text sent to the client. It must not
	// contain internal detail — see CONV-03.
	Message string

	// Details carries optional public context, such as which fields failed
	// validation. Rendered to the client, so it is subject to the same rule
	// as Message.
	Details Map

	// Cause is the underlying internal error. It is logged, never sent.
	//
	// Set it with Wrap rather than by assignment. Assigning an AppError to
	// its own Cause, directly or through a longer chain, makes Error() and
	// errors.Is recurse forever — a stack overflow Go cannot recover from.
	// Wrap copies first, so e.Wrap(e) is safe.
	Cause error
}

func (e *AppError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (%d %s): %v", e.Message, e.Status, e.Code, e.Cause)
	}
	return fmt.Sprintf("%s (%d %s)", e.Message, e.Status, e.Code)
}

// Unwrap exposes the internal cause to errors.Is and errors.As, so an
// AppError can wrap a sentinel without hiding it.
func (e *AppError) Unwrap() error { return e.Cause }

// Wrap attaches an internal cause and returns the error, for use at the
// point where a lower-level failure is translated into a response:
//
//	return runko.NotFound("User not found").Wrap(err)
//
// The cause is logged and never rendered.
func (e *AppError) Wrap(cause error) *AppError {
	wrapped := e.clone()
	wrapped.Cause = cause
	return wrapped
}

// clone returns a copy that shares no mutable state with the receiver, so
// package-level errors cannot be altered by a single request.
func (e *AppError) clone() *AppError {
	copied := *e
	if e.Details != nil {
		copied.Details = make(Map, len(e.Details))
		for k, v := range e.Details {
			copied.Details[k] = v
		}
	}
	return &copied
}

// WithDetails attaches public context, typically the fields that failed
// validation. Returns a copy, so package-level errors stay immutable.
//
// The map is copied too, not aliased. Sharing it would let one request
// mutate a package-level error's Details and have that show up in every
// other request's response body — a cross-request disclosure in the one
// field that is explicitly client-visible. The copy is one level deep:
// values inside the map are still shared, so do not mutate them either.
func (e *AppError) WithDetails(details Map) *AppError {
	withDetails := e.clone()
	withDetails.Details = nil
	if details != nil {
		withDetails.Details = make(Map, len(details))
		for k, v := range details {
			withDetails.Details[k] = v
		}
	}
	return withDetails
}

// NewAppError builds an error with an explicit status, code and public
// message. The helpers below cover the common statuses; use this directly
// for anything they do not.
func NewAppError(status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

// The helpers below construct the errors most APIs need. Each returns a
// fresh value, so callers can Wrap or add details without affecting
// anyone else.

// BadRequest reports malformed input: 400.
func BadRequest(message string) *AppError {
	return NewAppError(http.StatusBadRequest, "bad_request", message)
}

// Unauthorized reports missing or invalid credentials: 401.
func Unauthorized(message string) *AppError {
	return NewAppError(http.StatusUnauthorized, "unauthorized", message)
}

// Forbidden reports valid credentials without sufficient permission: 403.
func Forbidden(message string) *AppError {
	return NewAppError(http.StatusForbidden, "forbidden", message)
}

// NotFound reports a missing resource: 404.
func NotFound(message string) *AppError {
	return NewAppError(http.StatusNotFound, "not_found", message)
}

// Conflict reports a state conflict, such as a duplicate unique field: 409.
func Conflict(message string) *AppError {
	return NewAppError(http.StatusConflict, "conflict", message)
}

// Validation reports input that parsed but failed the rules: 422. The
// details map is rendered to the client, so it must name fields, not
// internal state.
func Validation(message string, details Map) *AppError {
	return NewAppError(http.StatusUnprocessableEntity, "validation_error", message).
		WithDetails(details)
}

// Internal reports an unexpected server-side failure: 500. The cause is
// logged; the client sees only a generic message, because anything more
// specific risks leaking internal detail.
func Internal(cause error) *AppError {
	return NewAppError(http.StatusInternalServerError, "internal_error",
		"An internal error occurred").Wrap(cause)
}

// RespondError writes the correct response for any error and logs what the
// client is not allowed to see.
//
// If err is (or wraps) an *AppError, its status, code, message and details
// are used. Anything else is treated as an unexpected failure and rendered
// as a generic 500 — an error that never passed through this package's
// vocabulary has not been vetted for disclosure, so it is not disclosed.
//
// The internal cause is logged with request correlation: error level for
// 5xx, warn for 4xx, info below that, since client mistakes are not server
// faults and should not page anyone.
//
// RespondError writes the response itself, so call it before anything else
// writes. Called after a handler has already written a body, it appends —
// producing a malformed response, as any writer would.
//
// logger and r may both be nil; nothing is logged when logger is nil.
func RespondError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	if err == nil {
		return
	}

	// errors.As succeeds on a (*AppError)(nil) carried inside a non-nil
	// error interface — the classic typed-nil trap, which handing callers
	// a concrete pointer type makes easier to fall into. The nil check
	// below is what stops that from dereferencing.
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr == nil {
		appErr = Internal(err)
	}

	if logger != nil {
		level := slog.LevelInfo
		switch {
		case appErr.Status >= 500:
			level = slog.LevelError
		case appErr.Status >= 400:
			// A client mistake is not a server fault and must not page
			// anyone.
			level = slog.LevelWarn
		}

		ctx := context.Background()
		attrs := []any{
			"error", err.Error(),
			"status", appErr.Status,
			"code", appErr.Code,
		}
		if r != nil {
			ctx = r.Context()
			attrs = append(attrs,
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", RequestID(ctx),
			)
		}
		logger.Log(ctx, level, "request failed", attrs...)
	}

	if len(appErr.Details) > 0 {
		ErrorWithDetails(w, appErr.Status, appErr.Code, appErr.Message, appErr.Details)
		return
	}
	Error(w, appErr.Status, appErr.Code, appErr.Message)
}
