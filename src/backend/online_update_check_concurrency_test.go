package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// blockedPublicSource waits for an explicit test release and counts concurrent outbound work.
type blockedPublicSource struct {
	calls    atomic.Int32
	release  chan struct{}
	finished chan struct{}
}

// Latest accepts a bounded operation context and returns one deterministic approved release or cancellation.
func (source *blockedPublicSource) Latest(ctx context.Context) (onlineupdate.Release, error) {
	source.calls.Add(1)
	defer close(source.finished)
	select {
	case <-source.release:
		return onlineupdate.Release{Version: "1.2.3"}, nil
	case <-ctx.Done():
		return onlineupdate.Release{}, ctx.Err()
	}
}

// TestPublicCheckConcurrent verifies that duplicate requests cannot start parallel GitHub checks.
func TestPublicCheckConcurrent(t *testing.T) {
	checker := newOnlineReleaseChecker("1.2.2")
	source := &blockedPublicSource{release: make(chan struct{}), finished: make(chan struct{})}
	checker.source = source
	status, started := checker.start()
	if !started || status.State != "checking" {
		t.Fatal("initial check was not accepted")
	}
	for index := 0; index < 10; index++ {
		status, started = checker.start()
		if started || status.State != "checking" {
			t.Fatal("duplicate check started")
		}
	}
	close(source.release)
	select {
	case <-source.finished:
	case <-time.After(time.Second):
		t.Fatal("check did not finish")
	}
	if source.calls.Load() != 1 {
		t.Fatal("parallel outbound work", source.calls.Load())
	}
}
