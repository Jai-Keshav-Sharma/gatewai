package router

import (
	"sync"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
)

// Circuit states.
const (
	stateClosed int32 = iota
	stateOpen
	stateHalfOpen
)

// CircuitBreaker implements the Closed → Open → HalfOpen state machine (§4.1).
//
//   - CLOSED:  normal operation; consecutive failures are counted. Reaching
//     failure_threshold flips the breaker OPEN — the downstream provider is
//     unhealthy, so further calls are blocked before they waste a request.
//   - OPEN:    every call is rejected immediately (fail fast) for
//     recovery_timeout. This prevents cascading failures: a broken provider
//     can't drag every request into a slow timeout death.
//   - HALF-OPEN: after the recovery window, a few probe requests
//     (half_open_max_requests) are allowed through. One success → CLOSED
//     (provider recovered); one failure → OPEN again.
//
// The router calls Allow() before every attempt and Success()/Failure() after
// every outcome. Concurrency: a mutex guards the shared state.
type CircuitBreaker struct {
	mu        sync.Mutex
	state     int32
	failures  int
	openUntil time.Time
	halfUsed  int

	threshold int
	recovery  time.Duration
	halfMax   int
}

// NewCircuitBreaker builds a breaker with the configured parameters.
func NewCircuitBreaker(cfg config.CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: cfg.FailureThreshold,
		recovery:  time.Duration(cfg.RecoveryTimeout),
		halfMax:   cfg.HalfOpenMaxRequests,
	}
}

// Allow reports whether a request may proceed against this provider.
func (c *CircuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateClosed:
		return true
	case stateOpen:
		if time.Now().After(c.openUntil) {
			// Recovery window elapsed: open the door for the first probe.
			c.state = stateHalfOpen
			c.halfUsed = 1
			return true
		}
		return false
	default: // half-open: admit only the allowed number of probes
		if c.halfUsed < c.halfMax {
			c.halfUsed++
			return true
		}
		return false
	}
}

// Success records a successful call: resets the failure count and, from
// half-open, recovers the breaker to closed.
func (c *CircuitBreaker) Success() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	if c.state == stateHalfOpen {
		c.state = stateClosed
	}
}

// Failure records a failed call. In half-open a single failure sends the
// breaker back to open for another recovery window; in closed it accumulates
// toward the threshold.
func (c *CircuitBreaker) Failure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateHalfOpen:
		c.state = stateOpen
		c.openUntil = time.Now().Add(c.recovery)
	case stateClosed:
		c.failures++
		if c.failures >= c.threshold {
			c.state = stateOpen
			c.openUntil = time.Now().Add(c.recovery)
		}
	}
}

// State returns the current state name (for logs and tests).
func (c *CircuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}
