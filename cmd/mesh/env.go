package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// parseDotEnv parses key-value pairs from .env format content.
// It skips empty lines and comments (starting with #).
// It trims optional "export " prefix and surrounding quotes.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	vars := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		val := strings.TrimSpace(parts[1])
		// Strip outer quotes if matched
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				quote := val[0]
				val = val[1 : len(val)-1]
				if quote == '"' {
					val = strings.ReplaceAll(val, `\n`, "\n")
					val = strings.ReplaceAll(val, `\t`, "\t")
					val = strings.ReplaceAll(val, `\"`, "\"")
					val = strings.ReplaceAll(val, `\\`, "\\")
				}
			}
		}
		vars[key] = val
	}
	return vars, scanner.Err()
}

// loadDotEnv loads environment variables from .env file into the process environment.
// If MESH_ENV_FILE is set, it loads that file; otherwise it defaults to ".env".
// Existing environment variables are never overwritten.
func loadDotEnv() {
	envPath := os.Getenv("MESH_ENV_FILE")
	if envPath == "" {
		envPath = ".env"
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		return
	}
	vars, err := parseDotEnv(bytes.NewReader(data))
	if err != nil {
		return
	}
	for k, v := range vars {
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// envOrDefault returns the value of the first found non-empty environment variable,
// or fallback if none are set.
func envOrDefault(keys []string, fallback string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
	}
	return fallback
}

// envString is a convenience helper for single env key.
func envString(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

// envInt returns an integer from environment or fallback.
func envInt(key string, fallback int) int {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}

// envDuration returns a time.Duration from environment or fallback.
func envDuration(key string, fallback time.Duration) time.Duration {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return fallback
}
