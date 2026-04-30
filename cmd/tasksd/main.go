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
	"strconv"
	"syscall"
	"time"

	"github.com/hanzoai/tasks/pkg/auth"
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

	requireID := envBool("TASKSD_REQUIRE_IDENTITY", false)
	validator := auth.NewValidator(auth.JWTConfig{
		JWKSURL:  envStr("TASKSD_JWKS_URL", ""),
		Issuer:   envStr("TASKSD_JWT_ISSUER", ""),
		Audience: envStr("TASKSD_JWT_AUDIENCE", ""),
	})

	srv, err := tasks.Embed(ctx, tasks.EmbedConfig{
		DataDir:         *dataDir,
		ZAPPort:         *zapPort,
		Namespace:       *ns,
		Logger:          logger,
		JWTValidator:    validator,
		RequireIdentity: requireID,
	})
	if err != nil {
		logger.Error("tasks.Embed", "err", err)
		os.Exit(1)
	}
	defer srv.Stop(context.Background())
	logger.Info("zap listener", "port", srv.ZAPPort(), "service", "_tasks._tcp")

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           buildHTTP(*ns, srv, validator, requireID),
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

// buildHTTP serves /healthz, /v1/tasks/* (browser JSON shim), and
// /_/tasks/* (embedded React UI). The /v1/tasks/* surface is the same
// data the ZAP node serves on :9999 — same model functions, no drift.
// Identity headers (X-User-Id, X-Org-Id, X-User-Email) are populated by
// hanzoai/gateway after IAM JWT validation; auth.RequireIdentity attaches
// them to the request context. TASKSD_REQUIRE_IDENTITY=true (production)
// rejects requests without identity headers; default false keeps the
// embedded/dev path working without a gateway.
//
// /healthz, /v1/tasks/health, and /_/tasks/* are intentionally
// unauthenticated: probes and the SPA shell run before any session
// exists.
func buildHTTP(ns string, srv *tasks.Embedded, validator *auth.Validator, requireID bool) http.Handler {
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

	identity := auth.RequireIdentity(validator, requireID)

	mux.Handle("/v1/tasks/", identity(srv.HTTPHandler()))
	mux.Handle("/v1/tasks/mcp", identity(srv.MCPHandler()))
	mux.Handle("/v1/tasks/events", identity(srv.EventsHandler()))

	// React Router has basename="/_/tasks". The bundle is only valid at
	// that prefix; serving it at "/" produces a blank page because
	// the router refuses to match. Redirect "/" → "/_/tasks/".
	mux.Handle("/_/tasks/", http.StripPrefix("/_/tasks", tasksui.Handler()))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/_/tasks/", http.StatusFound)
	})

	return mux
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
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
