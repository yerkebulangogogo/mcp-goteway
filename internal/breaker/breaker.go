package breaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrOpen is returned when a call is rejected because the circuit is open.
var ErrOpen = errors.New("circuit breaker open")

type state int

const (
	stateClosed   state = iota // normal — requests pass through
	stateOpen                  // failing fast — requests rejected immediately
	stateHalfOpen              // recovery probe — one request allowed through
)

// Breaker is a thread-safe circuit breaker for a single downstream server.
// Transitions: Closed → Open (on threshold failures) → HalfOpen (after openDuration) → Closed (on success).
type Breaker struct {
	mu           sync.Mutex
	state        state
	failures     uint32
	threshold    uint32
	openUntil    time.Time
	openDuration time.Duration
	name         string
}

func New(name string, threshold uint32, openDuration time.Duration) *Breaker {
	return &Breaker{
		name:         name,
		threshold:    threshold,
		openDuration: openDuration,
	}
}

// Execute runs fn if the circuit allows it, then records the outcome.
// Returns ErrOpen (wrapping the server name and retry time) when the circuit is open.
func (b *Breaker) Execute(fn func() error) error {
	if err := b.allow(); err != nil {
		return err
	}
	err := fn()
	b.record(err)
	return err
}

// State returns the current circuit state as a string (for logging/metrics).
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

func (b *Breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == stateOpen {
		if time.Now().Before(b.openUntil) {
			remaining := time.Until(b.openUntil).Round(time.Second)
			return fmt.Errorf("%w: server %q is unavailable, retry in %s", ErrOpen, b.name, remaining)
		}
		// openDuration elapsed — probe with one request
		b.state = stateHalfOpen
	}
	return nil
}

func (b *Breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err != nil {
		b.failures++
		if b.state == stateHalfOpen || b.failures >= b.threshold {
			b.state = stateOpen
			b.openUntil = time.Now().Add(b.openDuration)
			b.failures = 0
		}
		return
	}

	// success — reset
	b.state = stateClosed
	b.failures = 0
}
