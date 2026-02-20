// internal/config/compat_env.go
package config

import (
	"strconv"
	"strings"
)

// EnvOr returns env var value (trimmed) or default.
func EnvOr(key, def string) string {
	return getEnv(key, def)
}

// EnvIntOr returns env var parsed as int or default.
func EnvIntOr(key string, def int) int {
	v := strings.TrimSpace(getEnv(key, ""))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// MaskPresent is a tiny helper used in logs: it never prints secrets, only indicates presence.
func MaskPresent(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(missing)"
	}
	return "(present)"
}
