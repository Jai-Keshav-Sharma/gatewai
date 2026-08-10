package server

import (
	"fmt"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/middleware"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/proxy"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/router"
)

// NewRoutes registers all routes and composes the middleware chain (§4.1).
//
// The chain grows phase by phase: Phase 1 has only the body parser. Later
// phases insert request-id, logging, metrics, auth, rate limiting and cache
// middlewares — in the exact order defined by the plan — via this function.
func NewRoutes(cfg *config.Config, reg *provider.Registry, transport *http.Transport) http.Handler {
	mux := http.NewServeMux()

	chat := proxy.NewHandler(router.New(cfg, reg, transport))
	mux.Handle("POST /v1/chat/completions", middleware.Chain(chat, middleware.BodyParser))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	return mux
}
