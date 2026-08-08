// Command gatewai is the entry point: load config, wire dependencies, start
// the HTTP server, and shut down gracefully.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/server"
)

func main() {
	configPath := flag.String("config", "gatewai.yaml", "path to the gatewai config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("failed to load config", err)
	}
	setupLogger(cfg.Logging)

	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		fatal("failed to build provider registry", err)
	}

	// The upstream transport is a shared singleton (§10.1).
	transport := server.NewUpstreamTransport()
	handler := server.NewRoutes(cfg, reg, transport)
	srv := server.NewServer(cfg.Server, handler)

	go func() {
		slog.Info("gatewai listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("server failed", err)
		}
	}()

	// Graceful shutdown: wait for SIGINT/SIGTERM, then give in-flight
	// requests the configured window to complete before closing (§10.4).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down", "grace_period", cfg.Server.GracefulShutdown.String())
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Server.GracefulShutdown))
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("graceful shutdown did not complete", "err", err)
	}
	slog.Info("shutdown complete")
}

// setupLogger configures the process-wide slog logger from config.
func setupLogger(cfg config.LoggingConfig) {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts) // default: json
	}
	slog.SetDefault(slog.New(handler))
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
