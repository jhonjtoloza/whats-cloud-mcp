// Command gateway is the only binary in this project.
//
// It owns the whatsmeow sockets, persists events, serves the admin and tenant
// REST APIs, and mounts the MCP server at /mcp over Streamable HTTP. Running
// MCP inside this process removes a network hop, a second credential and a
// second container, which matters on a small shared box.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/config"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/httpapi"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

func main() {
	// The distroless runtime image has no shell and no curl, so the binary
	// probes its own health endpoint for the container healthcheck.
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	flag.Parse()

	if *healthcheck {
		if err := probeHealth(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("gateway stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// probeHealth performs a local GET /healthz and reports a non-zero exit code
// when the gateway is not serving.
//
// It reads the port straight from the environment rather than going through
// config.LoadGateway, so that a healthcheck never fails merely because an
// unrelated setting is missing.
func probeHealth() error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + healthcheckPort() + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

// healthcheckPort resolves the port the gateway listens on, preferring the port
// half of BIND_ADDR and falling back to PORT and then the default.
func healthcheckPort() string {
	if bindAddr := strings.TrimSpace(os.Getenv("BIND_ADDR")); bindAddr != "" {
		if _, port, err := net.SplitHostPort(bindAddr); err == nil && port != "" {
			return port
		}
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return port
	}
	return "8080"
}

func run() error {
	cfg, err := config.LoadGateway()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.MediaDir, 0o750); err != nil {
		return err
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.Migrate(ctx); err != nil {
		return err
	}
	logger.Info("database ready", slog.String("path", cfg.DBPath))

	// whatsmeow manages its own whatsmeow_* tables inside the same file.
	manager, err := wa.NewManager(ctx, db, logger, wa.ManagerOptions{
		HistoryScope:   cfg.HistorySyncScope,
		HistoryTimeout: cfg.HistorySyncTimeout,
	})
	if err != nil {
		return err
	}
	defer manager.Close()

	if err := manager.RestoreSessions(ctx); err != nil {
		logger.Error("could not restore sessions", slog.String("error", err.Error()))
	}

	api, err := httpapi.New(httpapi.Config{
		AdminToken:        cfg.AdminToken,
		DB:                db,
		Sessions:          manager,
		Logger:            logger,
		MCPAllowedOrigins: cfg.MCPAllowedOrigins,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening",
			slog.String("addr", srv.Addr),
			slog.Int("mcp_allowed_origins", len(cfg.MCPAllowedOrigins)))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
