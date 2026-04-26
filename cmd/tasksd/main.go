// Copyright © 2026 Hanzo AI. MIT License.
//
// tasksd is the Hanzo Tasks daemon: one Go binary, native ZAP transport,
// IAM JWT auth, embedded React UI. Zero gRPC, zero go.temporal.io.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hanzoai/tasks/pkg/tasks"
	tasksui "github.com/hanzoai/tasks/ui"
)

func main() {
	var (
		zapPort  = flag.Int("zap-port", envInt("TASKS_ZAP_PORT", 9999), "ZAP listener port")
		httpAddr = flag.String("http", envStr("TASKS_HTTP_ADDR", ":7243"), "HTTP listen address (UI + healthz)")
		dataDir  = flag.String("data", envStr("TASKS_DATA_DIR", "./tasks-data"), "Tasks persistence directory")
		ns       = flag.String("namespace", envStr("TASKS_NAMESPACE", "default"), "Default namespace")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := tasks.Embed(ctx, tasks.EmbedConfig{
		DataDir:   *dataDir,
		ZAPPort:   *zapPort,
		Namespace: *ns,
		Logger:    logger,
	})
	if err != nil {
		logger.Error("tasks.Embed", "err", err)
		os.Exit(1)
	}
	defer srv.Stop(context.Background())
	logger.Info("zap listener", "port", srv.ZAPPort(), "service", "_tasks._tcp")

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           buildHTTP(*ns),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("http listener", "addr", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// buildHTTP serves /healthz, /v1/tasks/health, and /_/tasks/* (embedded
// React UI). Identity headers (X-User-Id, X-Org-Id) are populated by
// hanzoai/gateway after IAM JWT validation; tasksd trusts them as the
// canonical caller identity.
func buildHTTP(ns string) http.Handler {
	mux := http.NewServeMux()

	probe := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":   "tasks",
			"status":    "ok",
			"namespace": ns,
		})
	}
	mux.HandleFunc("/healthz", probe)
	mux.HandleFunc("/v1/tasks/health", probe)

	mux.Handle("/_/tasks/", http.StripPrefix("/_/tasks", tasksui.Handler()))
	mux.Handle("/", tasksui.Handler())

	return mux
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
