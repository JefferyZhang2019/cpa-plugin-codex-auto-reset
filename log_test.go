package main

import (
	"testing"
	"time"
)

func TestLogRing_CapsAtCapacity(t *testing.T) {
	r := newLogRing(maxLogEntries)
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxLogEntries+50; i++ {
		r.append(LogEntry{Timestamp: base.Add(time.Duration(i) * time.Minute), Level: "info", Scope: "x"})
	}
	all := r.all()
	if len(all) != maxLogEntries {
		t.Fatalf("len = %d, want %d", len(all), maxLogEntries)
	}
	// Oldest 50 dropped (FIFO eviction); first kept entry is i=50.
	if !all[0].Timestamp.Equal(base.Add(50 * time.Minute)) {
		t.Fatalf("first entry = %v, want i=50", all[0].Timestamp)
	}
}

func TestLogRing_PruneOlderThan(t *testing.T) {
	r := newLogRing(maxLogEntries)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.append(LogEntry{Timestamp: now.Add(-2 * logRetention), Level: "info"}) // too old
	r.append(LogEntry{Timestamp: now.Add(-1 * time.Hour), Level: "info"})    // keep
	r.pruneOlderThan(now.Add(-logRetention))
	all := r.all()
	if len(all) != 1 {
		t.Fatalf("after prune len = %d, want 1", len(all))
	}
}

func TestLogRing_EmptyAllReturnsEmptyNotNil(t *testing.T) {
	r := newLogRing(maxLogEntries)
	all := r.all()
	if all == nil {
		t.Fatalf("all() returned nil for empty ring")
	}
	if len(all) != 0 {
		t.Fatalf("len = %d", len(all))
	}
}

func TestLogRing_ConcurrentAppendAndAll(t *testing.T) {
	// Smoke test: the ring is used from the worker goroutine and read from
	// management handlers; verify no race under -race.
	r := newLogRing(100)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			r.append(LogEntry{Timestamp: time.Now(), Level: "info"})
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		_ = r.all()
	}
	<-done
}
