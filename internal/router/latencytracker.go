package router

import (
	"sync"
	"time"
)

// ewmaAlpha is the smoothing factor of the EWMA: alpha = 2/(N+1) with N≈19
// means ~19 recent observations still influence the average while ancient
// ones fade away. Recent latency matters most for routing decisions.
const ewmaAlpha = 0.1

// latencyTracker records per-instance Exponentially Weighted Moving Average
// (EWMA) latencies (§5.6). It is safe for concurrent use.
type latencyTracker struct {
	mu sync.RWMutex
	m  map[string]time.Duration
}

// NewLatencyTracker creates a LatencyTracker.
func NewLatencyTracker() LatencyTracker {
	return &latencyTracker{m: make(map[string]time.Duration)}
}

// Record folds one latency observation into the instance's EWMA.
func (t *latencyTracker) Record(endpointName string, latency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.m[endpointName]
	ewma := time.Duration(ewmaAlpha*float64(latency) + (1-ewmaAlpha)*float64(prev))
	t.m[endpointName] = ewma
}

// Get returns the current EWMA latency for the instance (0 if none recorded).
func (t *latencyTracker) Get(endpointName string) time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[endpointName]
}
