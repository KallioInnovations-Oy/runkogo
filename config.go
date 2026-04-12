package runko

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ConfigLoader reads typed configuration from environment variables.
// It supports required values, defaults, and type conversion.
// No files, no parsers, no dependencies — just env vars.
type ConfigLoader struct{}

func newConfigLoader() *ConfigLoader {
	return &ConfigLoader{}
}

// Get reads a string from the environment. Returns an error if the
// variable is not set or is empty.
func (c *ConfigLoader) Get(key string) (string, error) {
	val := os.Getenv(key)
	if val == "" {
		return "", fmt.Errorf("config: required env var %s is not set", key)
	}
	return val, nil
}

// GetDefault reads a string from the environment, returning the
// default value if the variable is not set or empty.
func (c *ConfigLoader) GetDefault(key, defaultVal string) string {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val
}

// GetInt reads an integer from the environment.
func (c *ConfigLoader) GetInt(key string) (int, error) {
	raw, err := c.Get(key)
	if err != nil {
		return 0, err
	}
	val, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid integer: %s", key, raw)
	}
	return val, nil
}

// GetIntDefault reads an integer from the environment with a fallback.
func (c *ConfigLoader) GetIntDefault(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(raw)
	if err != nil {
		return defaultVal
	}
	return val
}

// GetBool reads a boolean from the environment.
// Truthy values: "true", "1", "yes". Everything else is false.
func (c *ConfigLoader) GetBool(key string) bool {
	raw := strings.ToLower(os.Getenv(key))
	return raw == "true" || raw == "1" || raw == "yes"
}

// GetDuration reads a time.Duration from the environment.
// Accepts Go duration strings: "5s", "2m30s", "1h", etc.
func (c *ConfigLoader) GetDuration(key string) (time.Duration, error) {
	raw, err := c.Get(key)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid duration: %s", key, raw)
	}
	return d, nil
}

// GetDurationDefault reads a duration with a fallback.
func (c *ConfigLoader) GetDurationDefault(key string, defaultVal time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return defaultVal
	}
	return d
}

// GetSlice reads a comma-separated list from the environment.
// Example: ALLOWED_ORIGINS=http://localhost,https://app.example.com
func (c *ConfigLoader) GetSlice(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// MustGet reads a required value and panics if not set.
// Use this in startup code where missing config should prevent boot.
func (c *ConfigLoader) MustGet(key string) string {
	val, err := c.Get(key)
	if err != nil {
		panic(err)
	}
	return val
}

// MustGetInt reads a required integer and panics if not set or invalid.
func (c *ConfigLoader) MustGetInt(key string) int {
	val, err := c.GetInt(key)
	if err != nil {
		panic(err)
	}
	return val
}
