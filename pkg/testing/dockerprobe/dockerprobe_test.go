package dockerprobe

import (
	"context"
	"errors"
	"testing"
)

// TestAvailablePanickingProbe pins the behavior the recover exists for. A
// daemon that answers badly makes testcontainers panic while deriving the
// host, and without the recover that hard-fails the package whose TestMain
// only wanted to skip -- reading as the code being broken rather than Docker.
func TestAvailablePanickingProbe(t *testing.T) {
	if available(context.Background(), func(context.Context) error { panic("the daemon is unwell") }) {
		t.Error("available = true for a probe that panicked, want false")
	}
}

func TestAvailableFailingProbe(t *testing.T) {
	if available(context.Background(), func(context.Context) error { return errors.New("refused") }) {
		t.Error("available = true for a probe that failed, want false")
	}
}

func TestAvailableHealthyProbe(t *testing.T) {
	if !available(context.Background(), func(context.Context) error { return nil }) {
		t.Error("available = false for a probe that succeeded, want true")
	}
}
