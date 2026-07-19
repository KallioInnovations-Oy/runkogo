package runko

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
} {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the runko error shape: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

func TestRespondError_UsesAppErrorStatusAndCode(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"bad request", BadRequest("Malformed"), 400, "bad_request"},
		{"unauthorized", Unauthorized("No credentials"), 401, "unauthorized"},
		{"forbidden", Forbidden("Not yours"), 403, "forbidden"},
		{"not found", NotFound("User not found"), 404, "not_found"},
		{"conflict", Conflict("Email taken"), 409, "conflict"},
		{"custom", NewAppError(418, "teapot", "Short and stout"), 418, "teapot"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			RespondError(rec, httptest.NewRequest("GET", "/", nil), nil, tc.err)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := decodeErrorBody(t, rec).Error.Code; got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// CONV-03: an error that never passed through this package's vocabulary
// has not been vetted for disclosure, so it must render as a generic 500
// rather than leaking whatever it happens to say.
func TestRespondError_UnknownErrorIsGeneric500(t *testing.T) {
	err := errors.New("pq: connection to 10.0.0.5:5432 failed: password authentication failed for user \"admin\"")

	rec := httptest.NewRecorder()
	RespondError(rec, httptest.NewRequest("GET", "/", nil), nil, err)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}

	body := decodeErrorBody(t, rec)
	if body.Error.Code != "internal_error" {
		t.Errorf("error.code = %q, want internal_error", body.Error.Code)
	}
	for _, leak := range []string{"pq:", "10.0.0.5", "password", "admin"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaked internal detail %q: %s", leak, rec.Body.String())
		}
	}
}

// The internal cause must reach the log and never the response body.
func TestRespondError_CauseIsLoggedNotRendered(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	cause := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	rec := httptest.NewRecorder()
	RespondError(rec, httptest.NewRequest("GET", "/users/1", nil), logger,
		NotFound("User not found").Wrap(cause))

	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("internal cause leaked into the response: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "10.0.0.5") {
		t.Errorf("internal cause was not logged: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"path":"/users/1"`) {
		t.Errorf("log line lacks request correlation: %s", buf.String())
	}
}

// 4xx is a client mistake, not a server fault — it must not log at error
// level and page someone.
func TestRespondError_LogLevelBySeverity(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLevel string
	}{
		{"client error logs at warn", NotFound("nope"), `"level":"WARN"`},
		{"server error logs at error", Internal(errors.New("boom")), `"level":"ERROR"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			logger := slog.New(slog.NewJSONHandler(&buf, nil))

			RespondError(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), logger, tc.err)

			if !strings.Contains(buf.String(), tc.wantLevel) {
				t.Errorf("log = %s, want %s", buf.String(), tc.wantLevel)
			}
		})
	}
}

// The point of the type: a sentinel wrapped deep in a call stack still
// renders correctly at the handler boundary, with no errors.Is ladder.
func TestRespondError_FindsAppErrorThroughWrapping(t *testing.T) {
	base := NotFound("User not found")
	wrapped := fmt.Errorf("loading profile: %w", fmt.Errorf("store lookup: %w", base))

	rec := httptest.NewRecorder()
	RespondError(rec, httptest.NewRequest("GET", "/", nil), nil, wrapped)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — the AppError was not found through the wrapping", rec.Code)
	}
}

// AppError must interoperate with errors.Is/As so it can wrap domain
// sentinels without hiding them.
func TestAppError_UnwrapsToCause(t *testing.T) {
	sentinel := errors.New("no rows in result set")
	err := NotFound("User not found").Wrap(sentinel)

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is could not see through AppError to the cause")
	}

	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusNotFound {
		t.Error("errors.As did not recover the AppError")
	}
}

func TestValidation_RendersDetails(t *testing.T) {
	err := Validation("Invalid input", Map{"fields": []string{"email", "name"}})

	rec := httptest.NewRecorder()
	RespondError(rec, httptest.NewRequest("POST", "/", nil), nil, err)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Error.Details == nil {
		t.Fatalf("details missing from response: %s", rec.Body.String())
	}
	if _, ok := body.Error.Details["fields"]; !ok {
		t.Errorf("details lack the fields key: %v", body.Error.Details)
	}
}

// Wrap and WithDetails must copy, so shared package-level errors cannot be
// mutated by one caller and observed by another.
func TestAppError_WrapAndWithDetailsDoNotMutate(t *testing.T) {
	base := NotFound("User not found")

	wrapped := base.Wrap(errors.New("cause"))
	if base.Cause != nil {
		t.Error("Wrap mutated the receiver")
	}
	if wrapped.Cause == nil {
		t.Error("Wrap did not attach the cause to the copy")
	}

	detailed := base.WithDetails(Map{"a": 1})
	if base.Details != nil {
		t.Error("WithDetails mutated the receiver")
	}
	if detailed.Details == nil {
		t.Error("WithDetails did not attach details to the copy")
	}
}

func TestRespondError_NilErrorWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	RespondError(rec, httptest.NewRequest("GET", "/", nil), nil, nil)

	if rec.Body.Len() != 0 {
		t.Errorf("nil error produced a body: %q", rec.Body.String())
	}
}

// A nil logger and a nil request must not panic — RespondError is called
// from handlers that may have neither in tests and background paths.
func TestRespondError_ToleratesNilLoggerAndRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	RespondError(rec, nil, slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)), NotFound("gone"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// errors.As succeeds on a (*AppError)(nil) held in a non-nil error
// interface — the classic typed-nil trap, which is easier to hit because
// this API hands callers a concrete pointer type. It must render a 500,
// not dereference nil.
func TestRespondError_TypedNilAppErrorDoesNotPanic(t *testing.T) {
	var typedNil *AppError
	var err error = typedNil // non-nil interface holding a nil pointer

	tests := []struct {
		name string
		err  error
	}{
		{"bare typed nil", err},
		{"wrapped typed nil", fmt.Errorf("loading user: %w", err)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			RespondError(rec, httptest.NewRequest("GET", "/", nil), nil, tc.err)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
			if got := decodeErrorBody(t, rec).Error.Code; got != "internal_error" {
				t.Errorf("error.code = %q, want internal_error", got)
			}
		})
	}
}

// Details is rendered to the client, so aliasing it across copies would let
// one request mutate a package-level error and have that appear in every
// other request's response body.
func TestAppError_DetailsAreNotAliasedAcrossCopies(t *testing.T) {
	base := Validation("Invalid input", Map{"fields": []string{"email"}})

	a := base.Wrap(errors.New("first"))
	b := base.Wrap(errors.New("second"))

	a.Details["injected"] = "from-a"

	if _, leaked := b.Details["injected"]; leaked {
		t.Error("mutating one copy's Details leaked into another copy")
	}
	if _, leaked := base.Details["injected"]; leaked {
		t.Error("mutating a copy's Details leaked into the shared base error")
	}

	// WithDetails must isolate the caller's map too.
	src := Map{"fields": []string{"name"}}
	withDetails := base.WithDetails(src)
	src["injected"] = "from-caller"
	if _, leaked := withDetails.Details["injected"]; leaked {
		t.Error("WithDetails aliased the caller's map")
	}
}

// Below 400 is neither a client nor a server fault, so it must not be
// logged as a warning.
func TestRespondError_SubClientErrorLogsAtInfo(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	RespondError(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), logger,
		NewAppError(http.StatusOK, "odd", "Not really an error"))

	if !strings.Contains(buf.String(), `"level":"INFO"`) {
		t.Errorf("log = %s, want INFO", buf.String())
	}
}
