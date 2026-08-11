package router

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Retry policy constants (§4.1 — retry ON: connection errors, timeouts, 5xx,
// 429; NEVER: 4xx except 429, or anything after bytes reached the client).
const (
	baseBackoff = 250 * time.Millisecond // first retry delay window: [0, 250ms)
	maxBackoff  = 8 * time.Second        // backoff growth cap (1s, 2s, 4s, 8s...)
)

// attemptFailure classifies one upstream attempt's outcome.
type attemptFailure struct {
	retryable  bool           // true: conn error, timeout, 5xx, 429
	retryAfter time.Duration  // 429 hint from the provider's Retry-After header
	response   *http.Response // set when NOT retryable: the 4xx to pass through
}

// dispatchType drives the retry loop within ONE provider type (§4.1 step 1:
// SAME-TYPE RETRY). The strategy selects an instance; the instance is tried
// up to max_retries+1 times with exponential backoff + jitter (honoring
// Retry-After on 429); when its budget is exhausted it is removed from the
// candidate pool and the next instance is selected. Returns nil when every
// instance of the type is exhausted retryably — the caller then moves to the
// next type in fallback_order.
func (r *Router) dispatchType(ctx context.Context, req *schema.UnifiedRequest, model string, candidates []Endpoint) (*Result, error) {
	attempt := 0
	var retryAfter time.Duration // from the most recent 429, honored on the next wait
	for len(candidates) > 0 {
		endpoint, err := r.strategy.Select(ctx, candidates)
		if err != nil {
			return nil, err
		}
		inst, ok := r.registry.Get(endpoint.ProviderName)
		if !ok {
			return nil, fmt.Errorf("router: unknown instance %q", endpoint.ProviderName)
		}
		// "Respect each instance's max_retries" (§4.1).
		budget := inst.MaxRetries() + 1
		for i := 0; i < budget; i++ {
			if attempt > 0 {
				// Exponential backoff with jitter between retries; a 429's
				// Retry-After, when larger, wins (§4.1).
				delay := backoff(attempt)
				if retryAfter > delay {
					delay = retryAfter
				}
				if err := wait(ctx, delay); err != nil {
					return nil, err
				}
			}
			resp, failure, err := r.doAttempt(ctx, req, inst, model)
			if err != nil {
				return nil, err // context canceled / build failure — stop
			}
			attempt++
			if failure == nil {
				r.breakerFor(inst).Success()
				return &Result{Resp: resp, Instance: inst, Model: model}, nil
			}
			r.breakerFor(inst).Failure()
			if !failure.retryable {
				// 4xx client error: no retry, no failover — pass through.
				return &Result{Resp: failure.response, Instance: inst, Model: model}, nil
			}
			retryAfter = failure.retryAfter
		}
		// Instance budget exhausted: drop it and let the strategy pick again.
		candidates = without(candidates, endpoint.ProviderName)
	}
	return nil, nil // type exhausted retryably
}

// doAttempt performs a single upstream HTTP call with the instance's timeout
// applied. Returns:
//   - (resp, nil, nil)        on success — resp.Body is wrapped so the
//     timeout context stays alive until the proxy closes it (streaming!)
//   - (resp, failure, nil)    on a classified failure; for non-retryable 4xx
//     the response is attached for pass-through
//   - (nil, nil, err)         on fatal errors (client gone, build failure)
func (r *Router) doAttempt(ctx context.Context, req *schema.UnifiedRequest, inst *provider.Instance, model string) (*http.Response, *attemptFailure, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, inst.Timeout())
	start := time.Now()

	// The mapped model may differ from the client's model (cross-type
	// failover). The copy is per-attempt, so the shared
	// RequestContext.ParsedRequest is never mutated.
	mapped := *req
	mapped.Model = model

	upstream, err := inst.BuildRequest(attemptCtx, &mapped, inst.APIKey())
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}

	resp, err := r.client.Do(upstream)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			// Client went away — stop the entire dispatch, don't retry.
			return nil, nil, ctx.Err()
		}
		// Connection error or the instance's own timeout: retryable.
		return nil, &attemptFailure{retryable: true}, nil
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		r.tracker.Record(inst.Name(), time.Since(start))
		resp.Body = &bodyWithCancel{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		// Retryable; honor the provider's Retry-After hint.
		retryAfter := retryAfterDelay(resp)
		_ = resp.Body.Close()
		cancel()
		return nil, &attemptFailure{retryable: true, retryAfter: retryAfter}, nil

	case resp.StatusCode >= 500:
		_ = resp.Body.Close()
		cancel()
		return nil, &attemptFailure{retryable: true}, nil

	default:
		// 4xx client error: the request itself is bad — retrying or failing
		// over would bill another provider for a doomed request.
		r.tracker.Record(inst.Name(), time.Since(start))
		resp.Body = &bodyWithCancel{ReadCloser: resp.Body, cancel: cancel}
		return resp, &attemptFailure{retryable: false, response: resp}, nil
	}
}

// bodyWithCancel releases the attempt's timeout context when the response
// body is closed. Without this, the deferred cancel in doAttempt would kill
// a streaming response before the proxy had relayed a single chunk.
type bodyWithCancel struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *bodyWithCancel) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// backoff returns the delay before attempt n (0-indexed): exponential growth
// with FULL jitter — a random value in [0, base*2^n), capped at maxBackoff.
// Jitter prevents thundering herds: without it, a provider outage would make
// every concurrent request retry at the same instant, amplifying the spike.
func backoff(attempt int) time.Duration {
	exp := 1 << min(attempt, 20)
	window := baseBackoff * time.Duration(exp)
	if window > maxBackoff {
		window = maxBackoff
	}
	return time.Duration(rand.Int64N(int64(window)))
}

// wait sleeps for d, honoring context cancellation.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfterDelay parses the Retry-After header (integer seconds or an
// HTTP-date), returning 0 when absent or unparsable.
func retryAfterDelay(resp *http.Response) time.Duration {
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// without returns candidates minus every endpoint with the given name.
func without(candidates []Endpoint, name string) []Endpoint {
	out := candidates[:0]
	for _, c := range candidates {
		if c.ProviderName != name {
			out = append(out, c)
		}
	}
	return out
}
