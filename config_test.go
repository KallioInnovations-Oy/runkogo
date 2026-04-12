package runko

import (
	"os"
	"testing"
	"time"
)

func TestConfig_GetDefault(t *testing.T) {
	c := newConfigLoader()

	// Unset var returns default.
	os.Unsetenv("RUNKO_TEST_MISSING")
	got := c.GetDefault("RUNKO_TEST_MISSING", "fallback")
	if got != "fallback" {
		t.Errorf("GetDefault missing var = %q, want %q", got, "fallback")
	}

	// Set var returns value.
	os.Setenv("RUNKO_TEST_SET", "actual")
	defer os.Unsetenv("RUNKO_TEST_SET")
	got = c.GetDefault("RUNKO_TEST_SET", "fallback")
	if got != "actual" {
		t.Errorf("GetDefault set var = %q, want %q", got, "actual")
	}
}

func TestConfig_Get_Required(t *testing.T) {
	c := newConfigLoader()

	os.Unsetenv("RUNKO_TEST_REQUIRED")
	_, err := c.Get("RUNKO_TEST_REQUIRED")
	if err == nil {
		t.Error("Get on missing required var should return error")
	}

	os.Setenv("RUNKO_TEST_REQUIRED", "value")
	defer os.Unsetenv("RUNKO_TEST_REQUIRED")
	val, err := c.Get("RUNKO_TEST_REQUIRED")
	if err != nil {
		t.Errorf("Get on set var: unexpected error: %v", err)
	}
	if val != "value" {
		t.Errorf("Get = %q, want %q", val, "value")
	}
}

func TestConfig_GetInt(t *testing.T) {
	c := newConfigLoader()

	os.Setenv("RUNKO_TEST_INT", "42")
	defer os.Unsetenv("RUNKO_TEST_INT")

	val, err := c.GetInt("RUNKO_TEST_INT")
	if err != nil {
		t.Fatalf("GetInt: unexpected error: %v", err)
	}
	if val != 42 {
		t.Errorf("GetInt = %d, want 42", val)
	}

	// Invalid int.
	os.Setenv("RUNKO_TEST_INT", "notanumber")
	_, err = c.GetInt("RUNKO_TEST_INT")
	if err == nil {
		t.Error("GetInt on non-integer should return error")
	}
}

func TestConfig_GetIntDefault(t *testing.T) {
	c := newConfigLoader()

	os.Unsetenv("RUNKO_TEST_INTDEF")
	got := c.GetIntDefault("RUNKO_TEST_INTDEF", 99)
	if got != 99 {
		t.Errorf("GetIntDefault missing = %d, want 99", got)
	}

	os.Setenv("RUNKO_TEST_INTDEF", "7")
	defer os.Unsetenv("RUNKO_TEST_INTDEF")
	got = c.GetIntDefault("RUNKO_TEST_INTDEF", 99)
	if got != 7 {
		t.Errorf("GetIntDefault set = %d, want 7", got)
	}
}

func TestConfig_GetBool(t *testing.T) {
	c := newConfigLoader()

	truthy := []string{"true", "1", "yes", "TRUE", "Yes"}
	for _, v := range truthy {
		os.Setenv("RUNKO_TEST_BOOL", v)
		if !c.GetBool("RUNKO_TEST_BOOL") {
			t.Errorf("GetBool(%q) = false, want true", v)
		}
	}

	falsy := []string{"false", "0", "no", "", "anything"}
	for _, v := range falsy {
		os.Setenv("RUNKO_TEST_BOOL", v)
		if v != "" && c.GetBool("RUNKO_TEST_BOOL") {
			t.Errorf("GetBool(%q) = true, want false", v)
		}
	}

	os.Unsetenv("RUNKO_TEST_BOOL")
}

func TestConfig_GetDuration(t *testing.T) {
	c := newConfigLoader()

	os.Setenv("RUNKO_TEST_DUR", "5s")
	defer os.Unsetenv("RUNKO_TEST_DUR")

	val, err := c.GetDuration("RUNKO_TEST_DUR")
	if err != nil {
		t.Fatalf("GetDuration: unexpected error: %v", err)
	}
	if val != 5*time.Second {
		t.Errorf("GetDuration = %v, want 5s", val)
	}
}

func TestConfig_GetSlice(t *testing.T) {
	c := newConfigLoader()

	os.Setenv("RUNKO_TEST_SLICE", "a, b ,c")
	defer os.Unsetenv("RUNKO_TEST_SLICE")

	got := c.GetSlice("RUNKO_TEST_SLICE")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("GetSlice = %v, want [a b c]", got)
	}

	// Empty.
	os.Unsetenv("RUNKO_TEST_SLICE")
	got = c.GetSlice("RUNKO_TEST_SLICE")
	if got != nil {
		t.Errorf("GetSlice empty = %v, want nil", got)
	}
}

func TestConfig_MustGet_Panics(t *testing.T) {
	c := newConfigLoader()
	os.Unsetenv("RUNKO_TEST_MUST")

	defer func() {
		if r := recover(); r == nil {
			t.Error("MustGet on missing var should panic")
		}
	}()
	c.MustGet("RUNKO_TEST_MUST")
}
