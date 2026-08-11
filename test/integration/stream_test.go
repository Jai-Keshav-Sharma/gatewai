package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/server"
)

// TestOpenAIStreamStripsInjectedUsage verifies the §4.1 step 10 STRIPPING
// RULE: when the client did not ask for include_usage, the usage chunk the
// gateway injected upstream must NOT reach the client.
func TestOpenAIStreamStripsInjectedUsage(t *testing.T) {
	mock := startMock(t)
	mock.usageChunk.Store(true)
	gw := startGateway(t, mock.server.URL)

	body, status := post(t, gw, "gw-test", `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(body, `"usage"`) {
		t.Fatalf("injected usage chunk leaked to client:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing terminal marker:\n%s", body)
	}
}

// TestOpenAIStreamForwardsRequestedUsage verifies the converse: a client
// that explicitly requested include_usage receives the usage chunk as-is.
func TestOpenAIStreamForwardsRequestedUsage(t *testing.T) {
	mock := startMock(t)
	mock.usageChunk.Store(true)
	gw := startGateway(t, mock.server.URL)

	body, _ := post(t, gw, "gw-test", `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("requested usage chunk missing:\n%s", body)
	}
}

// TestAnthropicStreamTranslation feeds an Anthropic-style SSE stream through
// the gateway and verifies it is translated to OpenAI format with the
// canonical terminal marker.
func TestAnthropicStreamTranslation(t *testing.T) {
	// A minimal Anthropic /v1/messages endpoint.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fl, _ := w.(http.Flusher)
		writeEvent := func(event, payload string) {
			_, _ = fmt.Fprintf(w, "event: %s\n", event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			fl.Flush()
		}
		writeEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-20250514","usage":{"input_tokens":7}}}`)
		writeEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Bonjour"}}`)
		writeEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
		writeEvent("message_stop", `{"type":"message_stop"}`)
	}))
	defer mock.Close()

	cfg := testConfig(mock.URL)
	cfg.Providers = []config.ProviderConfig{{
		Name: "anthropic-1", Type: "anthropic", APIKey: "sk-ant",
		BaseURL: mock.URL, Models: []string{"claude-sonnet-4-20250514"},
		Weight: 1, MaxRetries: 0, Timeout: config.Duration(5 * 1e9),
		DefaultMaxTokens: intPtr(8192),
	}}
	cfg.Routing.FallbackOrder = []string{"anthropic"}
	cfg.VirtualKeys.Keys[0].AllowedModels = []string{"*"}
	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	routes, err := server.NewRoutes(cfg, reg, &http.Transport{})
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	gw := httptest.NewServer(routes)
	defer gw.Close()

	body, status := post(t, gw, "gw-test", `{"model":"claude-sonnet-4-20250514","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"delta":{"role":"assistant"}`, `"content":"Bonjour"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in translated stream:\n%s", want, body)
		}
	}
}

// TestGeminiStreamTranslation feeds a Gemini-style SSE stream and verifies
// the terminal marker is synthesized (Gemini never sends one).
func TestGeminiStreamTranslation(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fl, _ := w.(http.Flusher)
		chunk := `{"candidates":[{"content":{"role":"model","parts":[{"text":"Ciao"}]}}]}`
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		final := `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11}}`
		_, _ = fmt.Fprintf(w, "data: %s\n\n", final)
		fl.Flush()
	}))
	defer mock.Close()

	cfg := testConfig(mock.URL)
	cfg.Providers = []config.ProviderConfig{{
		Name: "gemini-1", Type: "gemini", APIKey: "sk-gem",
		BaseURL: mock.URL, Models: []string{"gemini-2.5-pro"},
		Weight: 1, MaxRetries: 0, Timeout: config.Duration(5 * 1e9),
	}}
	cfg.Routing.FallbackOrder = []string{"gemini"}
	cfg.VirtualKeys.Keys[0].AllowedModels = []string{"*"}
	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	routes, err := server.NewRoutes(cfg, reg, &http.Transport{})
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	gw := httptest.NewServer(routes)
	defer gw.Close()

	body, status := post(t, gw, "gw-test", `{"model":"gemini-2.5-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"content":"Ciao"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in translated stream:\n%s", want, body)
		}
	}
}

// TestModelsEndpoint lists models across providers with auth required.
func TestModelsEndpoint(t *testing.T) {
	mock := startMock(t)
	gw := startGateway(t, mock.server.URL)

	// No key → 401.
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", resp.StatusCode)
	}

	// With key → the mock model is listed.
	req, _ = http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw-test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, m := range out.Data {
		if m.ID == "gpt-4o" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gpt-4o not listed: %+v", out.Data)
	}
}

func intPtr(n int) *int { return &n }
