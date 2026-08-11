// Package integration exercises the gateway end-to-end against in-process
// mock providers (no real API keys, no network).
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/server"
)

// mockProvider is a scriptable upstream that speaks the OpenAI wire format.
type mockProvider struct {
	server     *httptest.Server
	hits       atomic.Int64
	failKey    string       // when set, requests with this key fail with 500
	lastKey    atomic.Value // key of the most recent request (string)
	stream     atomic.Bool  // send SSE responses
	usageChunk atomic.Bool  // append a usage chunk to streams
}

// startMock spins up the upstream mock.
func startMock(t *testing.T) *mockProvider {
	t.Helper()
	m := &mockProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		m.lastKey.Store(key)
		if m.failKey != "" && key == m.failKey {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"mock exploded"}}`)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body.Stream || m.stream.Load() {
			fl, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
			if m.usageChunk.Load() {
				_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"Hello from mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`, body.Model))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// testConfig returns a minimal full-featured config pointing at the mock.
func testConfig(mockURL string) *config.Config {
	d := func(s string) config.Duration {
		dur, _ := time.ParseDuration(s)
		return config.Duration(dur)
	}
	return &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1", Port: 8080,
			ReadTimeout: d("30s"), WriteTimeout: 0, GracefulShutdown: d("5s"),
		},
		Providers: []config.ProviderConfig{{
			Name: "openai-1", Type: "openai", APIKey: "sk-test",
			BaseURL: mockURL + "/v1", Models: []string{"gpt-4o"},
			Weight: 1, MaxRetries: 0, Timeout: d("5s"),
		}},
		Routing: config.RoutingConfig{
			Strategy: "round-robin", FallbackOrder: []string{"openai"},
			CircuitBreaker: config.CircuitBreakerConfig{
				FailureThreshold: 5, RecoveryTimeout: d("30s"), HalfOpenMaxRequests: 3,
			},
		},
		RateLimiting: config.RateLimitConfig{
			Enabled: true, Backend: "memory", TPMEstimation: "char_estimate",
			Global: config.LimitConfig{RPM: 1000, TPM: 1000000},
			PerKey: config.LimitConfig{RPM: 100, TPM: 100000},
		},
		Cache: config.CacheConfig{
			Enabled: true, Backend: "memory",
			ExactMatch: config.ExactMatchConfig{TTL: d("1h"), MaxEntries: 100},
			Semantic:   config.SemanticConfig{Enabled: false},
		},
		VirtualKeys: config.VirtualKeysConfig{
			Enabled: true,
			Keys: []config.VirtualKeyConfig{{
				Key: "gw-test", AllowedModels: []string{"gpt-4o"},
				RateLimit: config.LimitConfig{RPM: 3, TPM: 10000},
			}},
		},
		Logging: config.LoggingConfig{Level: "error", Format: "text"},
		Metrics: config.MetricsConfig{Enabled: false, Path: "/metrics"},
	}
}

// startGateway boots the full gateway against the mock.
func startGateway(t *testing.T, mockURL string) *httptest.Server {
	t.Helper()
	cfg := testConfig(mockURL)
	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	routes, err := server.NewRoutes(cfg, reg, &http.Transport{})
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	ts := httptest.NewServer(routes)
	t.Cleanup(ts.Close)
	return ts
}

// post sends a chat completion request and returns the response body.
func post(t *testing.T, gw *httptest.Server, key, body string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data), resp.StatusCode
}

func TestProxyNonStreaming(t *testing.T) {
	mock := startMock(t)
	gw := startGateway(t, mock.server.URL)
	body, status := post(t, gw, "gw-test", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, "Hello from mock") {
		t.Fatalf("missing mock content: %s", body)
	}
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("missing usage in response: %s", body)
	}
}

func TestProxyStreaming(t *testing.T) {
	mock := startMock(t)
	gw := startGateway(t, mock.server.URL)
	body, status := post(t, gw, "gw-test", `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing terminal marker: %s", body)
	}
}

func TestAuth(t *testing.T) {
	mock := startMock(t)
	gw := startGateway(t, mock.server.URL)

	if _, status := post(t, gw, "", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusUnauthorized {
		t.Fatalf("missing key: status = %d, want 401", status)
	}
	if _, status := post(t, gw, "nope", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusUnauthorized {
		t.Fatalf("bad key: status = %d, want 401", status)
	}
	if _, status := post(t, gw, "gw-test", `{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusForbidden {
		t.Fatalf("disallowed model: status = %d, want 403", status)
	}
}

func TestRateLimit(t *testing.T) {
	mock := startMock(t)
	gw := startGateway(t, mock.server.URL)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		if _, status := post(t, gw, "gw-test", body); status != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, status)
		}
	}
	// The rate limiter runs before the cache, so the 4th request is blocked
	// even though the body would be a cache hit.
	if _, status := post(t, gw, "gw-test", body); status != http.StatusTooManyRequests {
		t.Fatalf("4th request: status = %d, want 429", status)
	}
}

func TestCacheServesSecondRequest(t *testing.T) {
	mock := startMock(t)
	gw := startGateway(t, mock.server.URL)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"cache me"}]}`
	if _, status := post(t, gw, "gw-test", body); status != http.StatusOK {
		t.Fatalf("first: status = %d", status)
	}
	if _, status := post(t, gw, "gw-test", body); status != http.StatusOK {
		t.Fatalf("second: status = %d", status)
	}
	if mock.hits.Load() != 1 {
		t.Fatalf("mock hits = %d, want 1 (second request must be served from cache)", mock.hits.Load())
	}
}

func TestFailoverToSecondInstance(t *testing.T) {
	mock := startMock(t)
	cfg := testConfig(mock.server.URL)
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		Name: "openai-2", Type: "openai", APIKey: "sk-test-2",
		BaseURL: mock.server.URL + "/v1", Models: []string{"gpt-4o"},
		Weight: 1, MaxRetries: 0, Timeout: config.Duration(5 * time.Second),
	})
	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	routes, err := server.NewRoutes(cfg, reg, &http.Transport{})
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	gw := httptest.NewServer(routes)
	t.Cleanup(gw.Close)

	// openai-1's key fails; openai-2 must serve the request (same-type retry).
	mock.failKey = "sk-test"
	body, status := post(t, gw, "gw-test", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if key, _ := mock.lastKey.Load().(string); key != "sk-test-2" {
		t.Fatalf("served by key %q, want sk-test-2 (failover to openai-2)", key)
	}
}
