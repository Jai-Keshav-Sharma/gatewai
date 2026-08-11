# Gatewai — High-performance AI Gateway

A reverse proxy that sits between your applications and AI providers
(OpenAI, Anthropic, Google Gemini), exposing a **single unified
OpenAI-compatible API**. Apps change one line — the base URL — and Gatewai
handles routing, failover, caching, rate limiting, cost tracking, content
safety, and observability behind the scenes.

```
┌──────────┐       ┌───────────┐       ┌──────────┐
│ Your App │──────▶│  Gatewai  │──────▶│  OpenAI  │
│ (any     │  one  │  (proxy)  │       ├──────────┤
│ language)│  API  │           │──────▶│Anthropic │
└──────────┘       │           │       ├──────────┤
                   │           │──────▶│  Gemini  │
                   └───────────┘       └──────────┘
```

## Features

| Area | What it does |
|---|---|
| **Unified API** | OpenAI-format `/v1/chat/completions` for every provider; per-provider request/response/SSE translation lives in adapters |
| **Routing** | Round-robin / weighted / least-latency load balancing; circuit breakers; same-type retries and cross-provider failover with `model_mapping` |
| **Governance** | Virtual keys with per-model permissions; RPM/TPM rate limiting (in-memory or Redis); exact-match and semantic response caching |
| **Guardrails** | Pluggable classifiers (LLM-based, webhook, provider moderation) for prompt and response safety — no regex, ever |
| **Observability** | Request IDs, structured slog logs, Prometheus metrics with token/cost accounting, streaming usage extraction |

## Quickstart

```bash
# 1. Copy the example config and set your provider keys
cp configs/gatewai.example.yaml gatewai.yaml
export OPENAI_API_KEY_1="sk-..."
export ANTHROPIC_API_KEY="sk-ant-..."
export GEMINI_API_KEY="..."

# 2. Run
go run ./cmd/gatewai -config gatewai.yaml
```

Point your app at `http://localhost:8080/v1` — that's the whole migration:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer gw-team-backend" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}'
```

With virtual keys enabled, the `Authorization` header carries a **virtual
key** from `virtual_keys` in the config (the example ships
`gw-team-frontend` and `gw-team-backend`).

## Architecture

```
HTTP Server ──▶ Middleware chain (§4.1 order)
                  0 bodyparser → 1 requestid → 2 logger → 3 metrics
                  → 4 auth → 5 rate limit → 6 cache → 7 guardrail
                  → Router (strategies, circuit breakers, retries,
                    failover with model_mapping)
                  → Provider adapter (OpenAI / Anthropic / Gemini)
                  → Upstream provider
```

The request body is read exactly once (bodyparser); every downstream stage
consumes `RequestContext` from the context. Streaming responses tee through
the chain chunk-by-chunk with immediate flushes, translated per provider,
and end with the canonical `data: [DONE]`.

## Configuration

`configs/gatewai.example.yaml` documents every option. Highlights:

- **Providers**: instances of `openai` / `anthropic` / `gemini` types with
  `base_url`, `api_key` (`${ENV_VAR}` interpolation), `models`, `weight`,
  `max_retries`, `timeout`. `default_max_tokens` is required for anthropic.
- **Routing**: `strategy`, `fallback_order`, `circuit_breaker`.
- **Rate limiting & cache**: `memory` or `redis` backends; semantic caching
  via a configured embedding provider instance.
- **Guardrails**: `llm` / `webhook` / `provider` classifier lists for
  pre-request and post-response (streams opt into `buffer_mode`).
- Environment variables referenced by enabled features MUST be set or the
  gateway refuses to start with a clear error.

## Operations

- `GET /health` — liveness probe.
- `GET /metrics` — Prometheus scrape endpoint (label cardinality is
  deliberately bounded: provider, model, status, cache_hit; keys hashed).
- Structured logs carry `request_id` through every line for tracing.
- Graceful shutdown: in-flight requests finish within
  `server.graceful_shutdown`.

## Development

```bash
make build    # build binary
make test     # go test -race with coverage
make bench    # hot-path benchmarks
make lint     # golangci-lint
make docker   # multi-stage image (distroless + CA certs)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines.
