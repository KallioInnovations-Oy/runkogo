package runko

// sanitizeID validates an externally-provided identifier string.
// Accepts: alphanumeric characters, hyphens, and underscores.
// Maximum length: 64 characters.
// Returns empty string if the input is invalid, signaling the caller
// to generate a fresh identifier instead.
//
// This prevents log injection attacks where a malicious client sends
// crafted X-Request-ID or X-Trace-ID headers containing JSON fragments,
// newlines, or extremely long strings designed to corrupt structured
// log output or exhaust disk space.
func sanitizeID(id string) string {
	if len(id) == 0 || len(id) > 64 {
		return ""
	}
	for _, c := range id {
		if !isIDChar(c) {
			return ""
		}
	}
	return id
}

// sanitizeTraceparent validates a W3C Trace Context `traceparent` header.
//
// Format (55 chars exactly):
//
//	version "-" trace-id "-" parent-id "-" flags
//	  2hex       32hex        16hex        2hex
//
// Returns the value unchanged if valid, or "" if not. The header arrives
// from outside and is propagated onward and into logs, so it is validated
// strictly rather than trusted: an all-zero trace-id or parent-id is
// invalid per the spec, and version "ff" is forbidden.
//
// Higher versions are accepted, not rejected. The W3C format is
// deliberately additive: §3.2.4 requires an implementation seeing a higher
// version to parse the first 55 characters and preserve the trace-id, with
// any extra fields appended after another dash. Rejecting them would sever
// a trace the moment an upstream adopts version 01 — precisely the failure
// this propagation exists to prevent. Version ff is forbidden by the spec.
//
// The value is returned unchanged rather than downgraded to version 00.
// The spec's downgrade rule applies to implementations that construct a
// new traceparent by making themselves the parent; RunkoGO creates no
// spans and never sets itself as parent-id, so it is a conduit rather than
// a participant, and passing the original through preserves information a
// version-aware downstream can still use.
func sanitizeTraceparent(v string) string {
	// version "-" trace-id "-" parent-id "-" flags
	const base = 55
	if len(v) < base {
		return ""
	}
	if v[2] != '-' || v[35] != '-' || v[52] != '-' {
		return ""
	}

	version, traceID, parentID, flags := v[0:2], v[3:35], v[36:52], v[53:55]

	for _, part := range []string{version, traceID, parentID, flags} {
		if !isLowerHex(part) {
			return ""
		}
	}
	if version == "ff" {
		return ""
	}
	if isAllZero(traceID) || isAllZero(parentID) {
		return ""
	}

	if version == "00" {
		// Version 00 is exactly 55 characters; trailing content is invalid
		// per its ABNF, not an extension.
		if len(v) != base {
			return ""
		}
		return v
	}

	// A higher version may append fields, which must be dash-separated.
	if len(v) > base {
		if v[base] != '-' {
			return ""
		}
		// The trailing fields are forwarded verbatim and may reach logs, so
		// they are constrained to the character set the format allows
		// rather than trusted.
		for i := base; i < len(v); i++ {
			c := v[i]
			if c != '-' && !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return ""
			}
		}
	}
	return v
}

// sanitizeTracestate validates a W3C `tracestate` header. It carries
// vendor-specific trace data as a comma-separated list; §3.5 requires a
// receiver to pass it on unchanged, so it is validated rather than parsed.
//
// The spec advises against propagating values beyond 512 characters, and
// the value is forwarded onward, so it is restricted to visible ASCII —
// this is what stops a CR/LF from riding along into a downstream header.
func sanitizeTracestate(v string) string {
	const maxLen = 512
	if v == "" || len(v) > maxLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return v
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isAllZero(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}

func isIDChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_'
}
