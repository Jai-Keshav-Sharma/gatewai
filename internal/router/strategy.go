package router

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// RoundRobin selects candidates in strict rotation.
// Selection order is shared across all goroutines via an atomic counter —
// no locks on the hot path.
type RoundRobin struct {
	counter atomic.Uint64
}

// Name returns "round-robin".
func (s *RoundRobin) Name() string { return "round-robin" }

// Select returns the next candidate in rotation.
func (s *RoundRobin) Select(ctx context.Context, candidates []Endpoint) (*Endpoint, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("round-robin: no candidates")
	}
	idx := s.counter.Add(1) - 1
	return &candidates[idx%uint64(len(candidates))], nil
}

// Weighted selects candidates proportionally to their configured weight:
// an instance with weight 2 receives twice the traffic of one with weight 1.
type Weighted struct {
	weights map[string]int
}

// Name returns "weighted".
func (s *Weighted) Name() string { return "weighted" }

// Select picks a candidate via weighted random selection.
func (s *Weighted) Select(ctx context.Context, candidates []Endpoint) (*Endpoint, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("weighted: no candidates")
	}
	total := 0
	for _, c := range candidates {
		total += s.weight(c.ProviderName)
	}
	pick := rand.IntN(total)
	for i := range candidates {
		pick -= s.weight(candidates[i].ProviderName)
		if pick < 0 {
			return &candidates[i], nil
		}
	}
	return &candidates[len(candidates)-1], nil // unreachable when total > 0
}

func (s *Weighted) weight(name string) int {
	if w, ok := s.weights[name]; ok && w > 0 {
		return w
	}
	return 1 // defensive default; config validation requires weight > 0
}

// LeastLatency picks the candidate with the lowest EWMA latency — traffic
// flows toward whichever instance is actually fastest right now, not just
// the one that used to be.
type LeastLatency struct {
	tracker LatencyTracker
}

// Name returns "least-latency".
func (s *LeastLatency) Name() string { return "least-latency" }

// Select returns the lowest-latency candidate. Instances with no recorded
// observations report 0 and start as preferred, which gives them the chance
// to be measured; ties keep the first candidate.
func (s *LeastLatency) Select(ctx context.Context, candidates []Endpoint) (*Endpoint, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("least-latency: no candidates")
	}
	best := &candidates[0]
	bestLatency := time.Duration(math.MaxInt64)
	for i := range candidates {
		lat := s.tracker.Get(candidates[i].ProviderName)
		if lat < bestLatency {
			bestLatency = lat
			best = &candidates[i]
		}
	}
	return best, nil
}

// newStrategy selects the strategy implementation by config name.
// Unknown names fall back to round-robin (config validation already rejects
// them at load time, so this is a defensive default).
func newStrategy(name string, weights map[string]int, tracker LatencyTracker) Strategy {
	switch name {
	case "weighted":
		return &Weighted{weights: weights}
	case "least-latency":
		return &LeastLatency{tracker: tracker}
	default:
		return &RoundRobin{}
	}
}
