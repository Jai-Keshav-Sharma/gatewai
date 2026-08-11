// Package benchmark measures the hot-path latency and allocations of the
// gateway (§12). Run with: go test -bench=. -benchmem ./test/benchmark/
package benchmark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/cache"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider/anthropic"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/ratelimit"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/server"
)

// benchGateway builds a gateway pointed at an instant mock upstream.
func benchGateway(b *testing.B) *httptest.Server {
	b.Helper()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	b.Cleanup(mock.Close)

	d := func(s string) config.Duration {
		dur, _ := time.ParseDuration(s)
		return config.Duration(dur)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 8080, ReadTimeout: d("30s"), WriteTimeout: 0, GracefulShutdown: d("5s")},
		Providers: []config.ProviderConfig{{
			Name: "openai-1", Type: "openai", APIKey: "sk-test",
			BaseURL: mock.URL + "/v1", Models: []string{"gpt-4o"},
			Weight: 1, MaxRetries: 0, Timeout: d("5s"),
		}},
		Routing: config.RoutingConfig{
			Strategy: "round-robin", FallbackOrder: []string{"openai"},
			CircuitBreaker: config.CircuitBreakerConfig{FailureThreshold: 5, RecoveryTimeout: d("30s"), HalfOpenMaxRequests: 3},
		},
		RateLimiting: config.RateLimitConfig{
			Enabled: true, Backend: "memory", TPMEstimation: "char_estimate",
			Global: config.LimitConfig{RPM: 1000000, TPM: 1000000000},
			PerKey: config.LimitConfig{RPM: 1000000, TPM: 1000000000},
		},
		Cache: config.CacheConfig{
			Enabled: true, Backend: "memory",
			ExactMatch: config.ExactMatchConfig{TTL: d("1h"), MaxEntries: 1000},
			Semantic:   config.SemanticConfig{Enabled: false},
		},
		VirtualKeys: config.VirtualKeysConfig{
			Enabled: true,
			Keys: []config.VirtualKeyConfig{{
				Key: "gw-test", AllowedModels: []string{"gpt-4o"},
				RateLimit: config.LimitConfig{RPM: 1000000, TPM: 1000000000},
			}},
		},
		Logging: config.LoggingConfig{Level: "error", Format: "text"},
		Metrics: config.MetricsConfig{Enabled: false, Path: "/metrics"},
	}
	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		b.Fatalf("registry: %v", err)
	}
	routes, err := server.NewRoutes(cfg, reg, &http.Transport{})
	if err != nil {
		b.Fatalf("routes: %v", err)
	}
	ts := httptest.NewServer(routes)
	b.Cleanup(ts.Close)
	return ts
}

// BenchmarkProxyHandler measures the added latency and allocations of one
// proxied non-streaming request (the §2 target: < 0.5ms added latency,
// minimal allocations on the hot path).
func BenchmarkProxyHandler(b *testing.B) {
	gw := benchGateway(b)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer gw-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
	}
}

// BenchmarkStreamChunkTranslation measures the SSE translation overhead for
// one Anthropic event line (the per-chunk cost of foreign-format translation).
func BenchmarkStreamChunkTranslation(b *testing.B) {
	adapter := &anthropic.Adapter{}
	ctx := schema.WithStreamState(context.Background(), adapter.NewStreamState())
	line := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello world"}}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := adapter.TranslateStreamChunk(ctx, line); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCacheLookup measures an LRU cache Get on a populated cache.
func BenchmarkCacheLookup(b *testing.B) {
	c := cache.NewMemory(1000)
	for i := 0; i < 500; i++ {
		_ = c.Set(context.Background(), fmt.Sprintf("key-%d", i), &schema.UnifiedResponse{ID: "x"}, time.Hour)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(context.Background(), fmt.Sprintf("key-%d", i%500))
	}
}

// BenchmarkRateLimiterAllow measures one rate-limit check.
func BenchmarkRateLimiterAllow(b *testing.B) {
	l := ratelimit.NewMemory(map[string]int{"global": 1000000}, 1000000, 1000000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = l.Allow(ctx, "global", 1)
	}
}
