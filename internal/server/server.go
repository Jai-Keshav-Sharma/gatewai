// Package server wires the HTTP layer: the listening server with tuned
// timeouts and the shared upstream transport for provider connections.
package server

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
)

// NewServer builds the http.Server from configuration.
//
// WriteTimeout is deliberately NOT set from config — validation guarantees
// server.write_timeout is 0 (disabled). Go's WriteTimeout covers the ENTIRE
// response lifetime, and LLM streams can run for minutes; a non-zero value
// would sever active streams (§7).
func NewServer(cfg config.ServerConfig, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)),
		Handler: handler,
		// Time to read the full request (body included).
		ReadTimeout: time.Duration(cfg.ReadTimeout),
		// WriteTimeout: 0 (disabled) — enforced by config validation.
		// Bound on how long the request HEADERS may be read — a cheap defense
		// against slowloris-style clients without affecting long streams.
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// NewUpstreamTransport creates the shared transport for all connections to
// upstream providers. It is a singleton: created ONCE at startup and reused
// by every request so TCP/TLS connections are pooled instead of re-created
// per request (§10.1).
func NewUpstreamTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   200,
		MaxConnsPerHost:       0, // unlimited — controlled by rate limiter (Phase 4)
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}
