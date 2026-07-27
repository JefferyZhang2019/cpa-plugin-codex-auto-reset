package main

import (
	"sync"
	"time"
)

// logRing is a capped, thread-safe ring buffer of LogEntry. The worker appends
// from one goroutine; management handlers read via all(). Entries evict FIFO
// when the cap is exceeded, and pruneOlderThan drops entries past logRetention.
// On startup, persisted logs from state.json are loaded back via load().
type logRing struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
}

func newLogRing(capacity int) *logRing {
	if capacity < 1 {
		capacity = 1
	}
	return &logRing{cap: capacity}
}

// load restores previously persisted logs (from state.json) into the ring.
// Called once at startup before the worker begins appending new entries.
func (r *logRing) load(entries []LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(entries) > r.cap {
		entries = entries[len(entries)-r.cap:]
	}
	r.entries = make([]LogEntry, len(entries))
	copy(r.entries, entries)
}

func (r *logRing) append(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	if len(r.entries) > r.cap {
		// FIFO eviction: drop the oldest slice prefix.
		r.entries = r.entries[len(r.entries)-r.cap:]
	}
}

func (r *logRing) all() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *logRing) pruneOlderThan(cutoff time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if !e.Timestamp.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	r.entries = kept
}
