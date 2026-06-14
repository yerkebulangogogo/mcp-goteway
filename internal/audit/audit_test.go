package audit_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/erke/mcp-gateway/internal/audit"
	"github.com/erke/mcp-gateway/internal/config"
)

// ── Masker ─────────────────────────────────────────────────────────────────

func TestMaskCardNumber(t *testing.T) {
	m, err := audit.NewMasker(nil)
	if err != nil {
		t.Fatal(err)
	}
	in := `{"card":"4111111111111111","amount":100}`
	out := m.Mask(in)
	if strings.Contains(out, "4111111111111111") {
		t.Errorf("card number not masked: %s", out)
	}
	if !strings.Contains(out, "[MASKED]") {
		t.Errorf("expected [MASKED] in output: %s", out)
	}
}

func TestMaskIIN(t *testing.T) {
	m, err := audit.NewMasker(nil)
	if err != nil {
		t.Fatal(err)
	}
	out := m.Mask(`{"iin":"123456789012"}`)
	if strings.Contains(out, "123456789012") {
		t.Errorf("IIN not masked: %s", out)
	}
}

func TestMaskEmail(t *testing.T) {
	m, err := audit.NewMasker(nil)
	if err != nil {
		t.Fatal(err)
	}
	out := m.Mask(`{"email":"user@example.com"}`)
	if strings.Contains(out, "user@example.com") {
		t.Errorf("email not masked: %s", out)
	}
}

func TestMaskPhone(t *testing.T) {
	m, err := audit.NewMasker(nil)
	if err != nil {
		t.Fatal(err)
	}
	out := m.Mask(`{"phone":"+7 (777) 123-45-67"}`)
	if strings.Contains(out, "123-45-67") {
		t.Errorf("phone not masked: %s", out)
	}
}

func TestMaskSafeStringUnchanged(t *testing.T) {
	m, err := audit.NewMasker(nil)
	if err != nil {
		t.Fatal(err)
	}
	in := `{"sql":"SELECT * FROM products WHERE id = 42"}`
	out := m.Mask(in)
	if out != in {
		t.Errorf("safe string was modified: %s", out)
	}
}

func TestMaskExtraPattern(t *testing.T) {
	m, err := audit.NewMasker([]string{`SECRET-\w+`})
	if err != nil {
		t.Fatal(err)
	}
	out := m.Mask(`token=SECRET-abc123`)
	if strings.Contains(out, "SECRET-abc123") {
		t.Errorf("custom pattern not masked: %s", out)
	}
}

func TestMaskInvalidPatternError(t *testing.T) {
	_, err := audit.NewMasker([]string{`[invalid`})
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

// ── Logger ─────────────────────────────────────────────────────────────────

func TestLoggerDisabledReturnsNil(t *testing.T) {
	l, err := audit.New(config.AuditConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if l != nil {
		t.Fatal("expected nil Logger when disabled")
	}
	// Nil Logger must not panic
	l.Log(audit.Entry{Method: "tools/call", Name: "x", Server: "s", Result: "ok"})
}

func TestLoggerWritesNDJSON(t *testing.T) {
	tmpFile := t.TempDir() + "/audit.log"
	cfg := config.AuditConfig{
		Enabled: true,
		Output:  "file",
		Path:    tmpFile,
		Mask:    config.MaskConfig{Enabled: false},
	}
	l, err := audit.New(cfg)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	l.Log(audit.Entry{
		Method:     audit.MethodToolCall,
		Name:       "dummy-db__db_query",
		Server:     "dummy-db",
		Args:       map[string]any{"sql": "SELECT 1"},
		Result:     "ok",
		DurationMS: 5,
	})
	l.Log(audit.Entry{
		Method: audit.MethodResourceRead,
		Name:   "db://schema",
		Server: "dummy-db",
		Result: "ok",
	})

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Read and parse each line as JSON
	data, err := readFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(data)
	if len(lines) < 2 {
		t.Fatalf("expected 2 log lines, got %d", len(lines))
	}

	var rec1 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec1); err != nil {
		t.Fatalf("line 1 not valid JSON: %v\n%s", err, lines[0])
	}
	if rec1["method"] != "tools/call" {
		t.Errorf("method: want tools/call, got %v", rec1["method"])
	}
	if rec1["name"] != "dummy-db__db_query" {
		t.Errorf("name: want dummy-db__db_query, got %v", rec1["name"])
	}
	if rec1["result"] != "ok" {
		t.Errorf("result: want ok, got %v", rec1["result"])
	}
}

func TestLoggerMasksArgs(t *testing.T) {
	tmpFile := t.TempDir() + "/audit.log"
	cfg := config.AuditConfig{
		Enabled: true,
		Output:  "file",
		Path:    tmpFile,
		Mask:    config.MaskConfig{Enabled: true},
	}
	l, err := audit.New(cfg)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	l.Log(audit.Entry{
		Method: audit.MethodToolCall,
		Name:   "db__query",
		Server: "db",
		Args:   map[string]any{"sql": "WHERE card = '4111111111111111'"},
		Result: "ok",
	})
	_ = l.Close()

	data, _ := readFile(tmpFile)
	if strings.Contains(string(data), "4111111111111111") {
		t.Error("card number must be masked in audit log")
	}
	if !strings.Contains(string(data), "[MASKED]") {
		t.Error("expected [MASKED] in audit log")
	}
}

func TestLoggerSequentialIDs(t *testing.T) {
	tmpFile := t.TempDir() + "/audit.log"
	cfg := config.AuditConfig{Enabled: true, Output: "file", Path: tmpFile}
	l, _ := audit.New(cfg)

	for range 3 {
		l.Log(audit.Entry{Method: "tools/call", Result: "ok"})
	}
	_ = l.Close()

	data, _ := readFile(tmpFile)
	lines := splitLines(string(data))
	ids := make(map[string]bool)
	for _, line := range lines {
		var r map[string]any
		_ = json.Unmarshal([]byte(line), &r)
		id, _ := r["id"].(string)
		if ids[id] {
			t.Errorf("duplicate audit ID %q", id)
		}
		ids[id] = true
	}
}

// helpers

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
