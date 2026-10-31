package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsWhenPathEmpty(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Provider != DefaultProvider {
		t.Errorf("Provider = %q", s.Provider)
	}
	if s.MaxSteps != DefaultMaxSteps {
		t.Errorf("MaxSteps = %d", s.MaxSteps)
	}
	if s.TimeoutSec != DefaultTimeoutSec {
		t.Errorf("TimeoutSec = %d", s.TimeoutSec)
	}
	if s.Model != "" {
		t.Errorf("Model = %q, want empty", s.Model)
	}
	if s.MaxRetries != nil {
		t.Errorf("MaxRetries = %v, want nil", *s.MaxRetries)
	}
	if s.Timeout() != DefaultTimeoutSec*time.Second {
		t.Errorf("Timeout = %v", s.Timeout())
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestLoadFullSettings(t *testing.T) {
	path := writeTemp(t, "config.json", `{
		"provider": "openai",
		"model": "gpt-5",
		"max_steps": 20,
		"timeout_sec": 120,
		"max_retries": 3
	}`)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Provider != "openai" || s.Model != "gpt-5" {
		t.Errorf("provider/model = %q/%q", s.Provider, s.Model)
	}
	if s.MaxSteps != 20 {
		t.Errorf("MaxSteps = %d", s.MaxSteps)
	}
	if s.TimeoutSec != 120 || s.Timeout() != 120*time.Second {
		t.Errorf("timeout = %d (%v)", s.TimeoutSec, s.Timeout())
	}
	if s.MaxRetries == nil || *s.MaxRetries != 3 {
		t.Errorf("MaxRetries = %v", s.MaxRetries)
	}
}

func TestLoadExplicitZeroRetries(t *testing.T) {
	// max_retries is a pointer so 0 must survive defaulting.
	path := writeTemp(t, "config.json", `{"max_retries": 0}`)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.MaxRetries == nil || *s.MaxRetries != 0 {
		t.Errorf("MaxRetries = %v, want &0", s.MaxRetries)
	}
}

func TestLoadUnknownFieldRejected(t *testing.T) {
	path := writeTemp(t, "config.json", `{"max_stepz": 5}`)

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := writeTemp(t, "config.json", `{not json`)

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed json")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := writeTemp(t, ".env", "TEST_SECRET_A=alpha\nTEST_SECRET_B=beta\n")

	t.Setenv("TEST_SECRET_B", "untouchable")
	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := Secret("TEST_SECRET_A"); got != "alpha" {
		t.Errorf("Secret(TEST_SECRET_A) = %q", got)
	}
	// Existing env vars must not be overwritten.
	if got := Secret("TEST_SECRET_B"); got != "untouchable" {
		t.Errorf("Secret(TEST_SECRET_B) = %q", got)
	}
}

func TestLoadDotEnvMissingFileOK(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.env")
	if err := LoadDotEnv(missing); err != nil {
		t.Errorf("LoadDotEnv(missing) = %v, want nil", err)
	}
}

func TestRequireSecret(t *testing.T) {
	t.Setenv("TEST_SECRET_C", "gamma")
	if v, err := RequireSecret("TEST_SECRET_C"); err != nil || v != "gamma" {
		t.Errorf("RequireSecret = %q, %v", v, err)
	}

	os.Unsetenv("TEST_SECRET_D")
	if _, err := RequireSecret("TEST_SECRET_D"); err == nil {
		t.Error("expected error for unset secret")
	}
}
