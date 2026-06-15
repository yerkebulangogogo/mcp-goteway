package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yerkebulangogogo/mcp-goteway/internal/config"
)

// Method constants mirror MCP method names used in audit entries.
const (
	MethodToolCall    = "tools/call"
	MethodResourceRead = "resources/read"
	MethodPromptGet   = "prompts/get"
)

// Entry is what the proxy builds for each inbound MCP call.
type Entry struct {
	Method     string // "tools/call" | "resources/read" | "prompts/get"
	Name       string // prefixed tool/resource/prompt name
	Server     string // downstream server name
	Args       any    // raw call arguments — serialized + masked before writing
	Result     string // "ok" | "error" | "timeout" | "circuit_open"
	Error      string // non-empty when Result != "ok"
	DurationMS int64
}


const ringCap = 200 // number of recent entries kept in memory for the dashboard

// LogEntry is the exported JSON-serializable form of an audit record.
type LogEntry struct {
	Timestamp  time.Time `json:"ts"`
	ID         string    `json:"id"`
	Method     string    `json:"method"`
	Name       string    `json:"name"`
	Server     string    `json:"server"`
	Args       string    `json:"args,omitempty"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
}

// Logger writes audit entries as NDJSON (one JSON object per line).
// A nil Logger is safe to call — all operations become no-ops.
type Logger struct {
	mu     sync.Mutex
	enc    *json.Encoder
	masker *Masker
	seq    atomic.Int64
	closer io.Closer

	ringMu sync.RWMutex
	ring   []LogEntry
}

// New creates a Logger from config. Returns nil (disabled) when cfg.Enabled is false.
func New(cfg config.AuditConfig) (*Logger, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	var (
		w      io.Writer
		closer io.Closer
	)

	switch cfg.Output {
	case "file":
		if cfg.Path == "" {
			return nil, fmt.Errorf("audit.path is required when output=file")
		}
		f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open audit log %q: %w", cfg.Path, err)
		}
		w, closer = f, f
	case "stderr", "":
		w = os.Stderr
	default:
		return nil, fmt.Errorf("unknown audit output %q (use \"file\" or \"stderr\")", cfg.Output)
	}

	l := &Logger{
		enc:    json.NewEncoder(w),
		closer: closer,
	}

	if cfg.Mask.Enabled {
		m, err := NewMasker(cfg.Mask.Patterns)
		if err != nil {
			return nil, fmt.Errorf("audit masker: %w", err)
		}
		l.masker = m
	}

	return l, nil
}

// Log serialises e, applies masking, and writes one JSON line to the audit sink.
// Safe to call on a nil Logger.
func (l *Logger) Log(e Entry) {
	if l == nil {
		return
	}

	id := fmt.Sprintf("%d", l.seq.Add(1))

	args := marshalArgs(e.Args)
	if l.masker != nil {
		args = l.masker.Mask(args)
	}

	entry := LogEntry{
		Timestamp:  time.Now().UTC(),
		ID:         id,
		Method:     e.Method,
		Name:       e.Name,
		Server:     e.Server,
		Args:       args,
		Result:     e.Result,
		Error:      e.Error,
		DurationMS: e.DurationMS,
	}

	l.mu.Lock()
	_ = l.enc.Encode(entry)
	l.mu.Unlock()

	l.ringMu.Lock()
	l.ring = append(l.ring, entry)
	if len(l.ring) > ringCap {
		l.ring = l.ring[len(l.ring)-ringCap:]
	}
	l.ringMu.Unlock()
}

// Recent returns the last n entries, newest first. Safe to call on a nil Logger.
func (l *Logger) Recent(n int) []LogEntry {
	if l == nil {
		return nil
	}
	l.ringMu.RLock()
	defer l.ringMu.RUnlock()

	src := l.ring
	if len(src) > n {
		src = src[len(src)-n:]
	}
	out := make([]LogEntry, len(src))
	for i, e := range src {
		out[len(src)-1-i] = e // reverse: newest first
	}
	return out
}

// Close flushes and closes the underlying file if the Logger owns it.
// Safe to call on a nil Logger.
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

func marshalArgs(args any) string {
	if args == nil {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("<marshal error: %s>", err)
	}
	return string(b)
}
