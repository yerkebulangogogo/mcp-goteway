package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/erke/mcp-gateway/internal/config"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestDefaultsApplied(t *testing.T) {
	path := writeTemp(t, `
servers:
  svc:
    type: stdio
    command: echo
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	srv := cfg.Servers["svc"]

	if srv.Prefix != "svc" {
		t.Errorf("prefix: want svc, got %q", srv.Prefix)
	}
	if srv.Timeout.Connect.Duration != 30*time.Second {
		t.Errorf("connect timeout: want 30s, got %s", srv.Timeout.Connect.Duration)
	}
	if srv.Timeout.Call.Duration != 30*time.Second {
		t.Errorf("call timeout: want 30s, got %s", srv.Timeout.Call.Duration)
	}
	if srv.CircuitBreaker.Threshold != 5 {
		t.Errorf("cb threshold: want 5, got %d", srv.CircuitBreaker.Threshold)
	}
	if srv.CircuitBreaker.OpenDuration.Duration != 30*time.Second {
		t.Errorf("cb open_duration: want 30s, got %s", srv.CircuitBreaker.OpenDuration.Duration)
	}
}

func TestExplicitValuesOverrideDefaults(t *testing.T) {
	path := writeTemp(t, `
servers:
  svc:
    type: stdio
    command: echo
    prefix: myprefix
    timeout:
      connect: 10s
      call: 3s
    circuit_breaker:
      enabled: true
      threshold: 2
      open_duration: 1m
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	srv := cfg.Servers["svc"]

	if srv.Prefix != "myprefix" {
		t.Errorf("prefix: want myprefix, got %q", srv.Prefix)
	}
	if srv.Timeout.Connect.Duration != 10*time.Second {
		t.Errorf("connect timeout: want 10s, got %s", srv.Timeout.Connect.Duration)
	}
	if srv.Timeout.Call.Duration != 3*time.Second {
		t.Errorf("call timeout: want 3s, got %s", srv.Timeout.Call.Duration)
	}
	if !srv.CircuitBreaker.Enabled {
		t.Error("circuit_breaker.enabled: want true")
	}
	if srv.CircuitBreaker.Threshold != 2 {
		t.Errorf("cb threshold: want 2, got %d", srv.CircuitBreaker.Threshold)
	}
	if srv.CircuitBreaker.OpenDuration.Duration != time.Minute {
		t.Errorf("cb open_duration: want 1m, got %s", srv.CircuitBreaker.OpenDuration.Duration)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing type", "servers:\n  svc:\n    command: echo\n"},
		{"stdio missing command", "servers:\n  svc:\n    type: stdio\n"},
		{"sse missing url", "servers:\n  svc:\n    type: sse\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.yaml)
			_, err := config.Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
