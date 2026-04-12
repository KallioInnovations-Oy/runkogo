package runko

import "testing"

func TestSanitizeID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Valid IDs — should pass through unchanged.
		{"simple hex", "abc123def456", "abc123def456"},
		{"with hyphens", "req-abc-123", "req-abc-123"},
		{"with underscores", "trace_id_99", "trace_id_99"},
		{"mixed case", "AbCdEf-123_XyZ", "AbCdEf-123_XyZ"},
		{"max length 64", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},

		// Invalid IDs — should return empty (caller generates fresh).
		{"empty string", "", ""},
		{"too long 65 chars", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ""},
		{"contains space", "abc 123", ""},
		{"contains newline", "abc\n123", ""},
		{"JSON injection", `evil","injected":"pwned`, ""},
		{"script injection", "<script>alert(1)</script>", ""},
		{"null byte", "abc\x00def", ""},
		{"unicode", "café-123", ""},
		{"dot", "req.id.123", ""},
		{"slash", "path/to/id", ""},
		{"colon", "id:123", ""},
		{"equals", "key=value", ""},
		{"pipe", "a|b", ""},
		{"semicolon", "a;b", ""},
		{"backslash", `a\b`, ""},
		{"tab", "a\tb", ""},
		{"percent", "a%20b", ""},
		{"at sign", "user@host", ""},
		{"hash", "id#fragment", ""},
		{"only hyphens", "---", "---"},
		{"only underscores", "___", "___"},
		{"single char", "a", "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeID(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
