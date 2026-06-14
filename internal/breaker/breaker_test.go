package breaker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/erke/mcp-gateway/internal/breaker"
)

var errFake = errors.New("downstream error")

func TestClosedPassesThrough(t *testing.T) {
	b := breaker.New("svc", 3, time.Second)

	called := false
	err := b.Execute(func() error {
		called = true
		return nil
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !called {
		t.Fatal("expected fn to be called")
	}
	if b.State() != "closed" {
		t.Fatalf("expected closed, got %s", b.State())
	}
}

func TestOpensAfterThreshold(t *testing.T) {
	b := breaker.New("svc", 3, time.Minute)

	for i := range 3 {
		err := b.Execute(func() error { return errFake })
		if err == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}

	if b.State() != "open" {
		t.Fatalf("expected open after threshold, got %s", b.State())
	}
}

func TestOpenRejectsWithoutCallingFn(t *testing.T) {
	b := breaker.New("svc", 1, time.Minute)

	_ = b.Execute(func() error { return errFake }) // trip open

	called := false
	err := b.Execute(func() error {
		called = true
		return nil
	})

	if !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("expected ErrOpen, got %v", err)
	}
	if called {
		t.Fatal("fn must not be called when circuit is open")
	}
}

func TestTransitionsToHalfOpenAfterDuration(t *testing.T) {
	b := breaker.New("svc", 1, 50*time.Millisecond)

	_ = b.Execute(func() error { return errFake }) // trip open

	time.Sleep(60 * time.Millisecond)

	// First call after duration: half-open probe
	err := b.Execute(func() error { return nil }) // probe succeeds
	if err != nil {
		t.Fatalf("probe call failed: %v", err)
	}
	if b.State() != "closed" {
		t.Fatalf("expected closed after successful probe, got %s", b.State())
	}
}

func TestHalfOpenReopensOnFailure(t *testing.T) {
	b := breaker.New("svc", 1, 50*time.Millisecond)

	_ = b.Execute(func() error { return errFake }) // trip open

	time.Sleep(60 * time.Millisecond)

	// Probe fails → back to open
	_ = b.Execute(func() error { return errFake })

	if b.State() != "open" {
		t.Fatalf("expected open after failed probe, got %s", b.State())
	}
}

func TestClosedAfterSuccessResetFailures(t *testing.T) {
	b := breaker.New("svc", 3, time.Minute)

	// Two failures, then a success — should not open
	_ = b.Execute(func() error { return errFake })
	_ = b.Execute(func() error { return errFake })
	_ = b.Execute(func() error { return nil }) // success resets counter

	// Two more failures — still below threshold
	_ = b.Execute(func() error { return errFake })
	_ = b.Execute(func() error { return errFake })

	if b.State() != "closed" {
		t.Fatalf("expected closed (failures reset on success), got %s", b.State())
	}
}
