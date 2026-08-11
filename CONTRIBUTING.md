# Contributing to Gatewai

Thanks for contributing! A few guidelines to keep the codebase healthy.

## Design principles

Every change must serve at least one of the project's three principles
(see the implementation plan, §3.2):

1. **Reliability** — works correctly when things go wrong (failover,
   circuit breakers, context cancellation, graceful shutdown).
2. **Scalability** — handles growing load gracefully (stateless core,
   Redis-backed shared state, pooling, bounded memory).
3. **Maintainability** — clean interface boundaries, single responsibility
   per file, no duplication.

## Rules

- **No hardcoding** (§3.1): behavior is config-driven or interface-driven.
  Never embed URLs, keys, limits, or model pricing in logic.
- **No regex or keyword-list guardrails** — classifiers are pluggable
  interfaces only.
- Keep functions small and focused; a file has one job.
- Follow the existing style: `gofmt` output, `go vet` clean, comments that
  explain WHY, not what.
- Streaming is sacred: never break SSE flushing, context cancellation, or
  the `write_timeout: 0` rule.

## Workflow

1. Fork the repo and create a feature branch.
2. Run checks locally:

   ```bash
   make lint
   make test
   make bench
   ```

3. Open a pull request with a clear description of the change and why it
   serves the design principles.

## Running the gateway locally

See the [README](README.md) quickstart.
