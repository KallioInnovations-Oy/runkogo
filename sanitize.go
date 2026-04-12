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

func isIDChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_'
}
