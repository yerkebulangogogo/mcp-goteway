package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Admin ──────────────────────────────────────────────────────────────────

type AdminConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"` // e.g. ":9090"
}

// ── Audit ──────────────────────────────────────────────────────────────────

type MaskConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Patterns []string `yaml:"patterns"` // additional regex patterns beyond built-ins
}

type AuditConfig struct {
	Enabled bool       `yaml:"enabled"`
	Output  string     `yaml:"output"` // "stderr" (default) or "file"
	Path    string     `yaml:"path"`   // required when output=file
	Mask    MaskConfig `yaml:"mask"`
}

// ── Servers ────────────────────────────────────────────────────────────────

type ServerType string

const (
	ServerTypeStdio ServerType = "stdio"
	ServerTypeSSE   ServerType = "sse"
)

// Duration wraps time.Duration so YAML strings like "5s", "1m" unmarshal correctly.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

type TimeoutConfig struct {
	// Connect is the deadline for the initial handshake (dial + initialize + list tools/resources/prompts).
	Connect Duration `yaml:"connect"`
	// Call is the per-request deadline for tool calls, resource reads, and prompt gets.
	Call Duration `yaml:"call"`
}

type CircuitBreakerConfig struct {
	Enabled bool `yaml:"enabled"`
	// Threshold is the number of consecutive failures that trips the circuit open.
	Threshold uint32 `yaml:"threshold"`
	// OpenDuration is how long the circuit stays open before allowing a probe request.
	OpenDuration Duration `yaml:"open_duration"`
}


type ServerConfig struct {
	Type    ServerType `yaml:"type"`
	Command string     `yaml:"command"`
	Args    []string   `yaml:"args"`
	Env     []string   `yaml:"env"`
	URL     string     `yaml:"url"`    // for SSE type
	Prefix  string     `yaml:"prefix"` // tool/prompt name prefix; defaults to server name

	Timeout        TimeoutConfig        `yaml:"timeout"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

type Config struct {
	Admin   AdminConfig             `yaml:"admin"`
	Audit   AuditConfig             `yaml:"audit"`
	Servers map[string]ServerConfig `yaml:"servers"`
}

const (
	defaultConnectTimeout = 30 * time.Second
	defaultCallTimeout    = 30 * time.Second
	defaultCBThreshold    = 5
	defaultCBOpenDuration = 30 * time.Second
	defaultAdminAddr      = ":9090"
)

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Admin.Addr == "" {
		cfg.Admin.Addr = defaultAdminAddr
	}

	for name, srv := range cfg.Servers {
		// Prefix default
		if srv.Prefix == "" {
			srv.Prefix = name
		}

		// Validation
		if srv.Type == "" {
			return nil, fmt.Errorf("server %q: type is required", name)
		}
		if srv.Type == ServerTypeStdio && srv.Command == "" {
			return nil, fmt.Errorf("server %q: command is required for stdio type", name)
		}
		if srv.Type == ServerTypeSSE && srv.URL == "" {
			return nil, fmt.Errorf("server %q: url is required for sse type", name)
		}

		// Timeout defaults
		if srv.Timeout.Connect.Duration == 0 {
			srv.Timeout.Connect.Duration = defaultConnectTimeout
		}
		if srv.Timeout.Call.Duration == 0 {
			srv.Timeout.Call.Duration = defaultCallTimeout
		}

		// Circuit breaker defaults
		if srv.CircuitBreaker.Threshold == 0 {
			srv.CircuitBreaker.Threshold = defaultCBThreshold
		}
		if srv.CircuitBreaker.OpenDuration.Duration == 0 {
			srv.CircuitBreaker.OpenDuration.Duration = defaultCBOpenDuration
		}

		cfg.Servers[name] = srv
	}

	return &cfg, nil
}
