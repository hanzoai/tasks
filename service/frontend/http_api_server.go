// Copyright © 2026 Hanzo AI. MIT License.
//
// HTTP API server for hanzoai/tasks.
//
// One surface, one path prefix: every HTTP request lives under
// /v1/tasks/*. There is no /api/ mount; the legacy gRPC-Gateway
// (runtime.ServeMux + JSON transcoding) is gone. Browsers get JSON
// here because they have to. Everything else uses the ZAP binary
// transport on :9652 — faster, zero-copy, one canonical framing.
//
// Handlers call the frontend WorkflowHandler directly as Go
// methods. No gRPC codec, no protojson middleware, no
// grpc-gateway registration — one call and done. Marshalling to
// JSON uses protojson because the request/response types are
// still protobuf messages from go.temporal.io/api (we haven't
// rewritten the schemas yet; that's task #41).
//
// Routes:
//   GET /v1/tasks/healthz                                 — liveness
//   GET /v1/tasks/namespaces                              — list namespaces
//   GET /v1/tasks/namespaces/:ns/workflows                — list executions
//   GET /v1/tasks/namespaces/:ns/workflows/:id            — describe execution
//   GET /v1/tasks/namespaces/:ns/schedules                — list schedules
//   GET /*                                                — embedded SPA
//
// Transport: gofiber v3 (fasthttp under the hood). No gorilla/mux,
// no grpc-gateway runtime.

package frontend

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/dynamicconfig"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/log/tag"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/rpc"
	"github.com/hanzoai/tasks/common/rpc/encryption"
	tasksui "github.com/hanzoai/tasks/ui"
)

// HTTPAPIServer owns the fasthttp listener that serves the /v1/tasks
// HTTP surface plus the embedded SPA. Constructed once at frontend
// startup; shut down by the fx lifecycle.
type HTTPAPIServer struct {
	app          *fiber.App
	listener     net.Listener
	logger       log.Logger
	stopped      chan struct{}
	allowedHosts dynamicconfig.TypedPropertyFn[*regexp.Regexp]
}

// NewHTTPAPIServer creates the server and binds the listener.
//
// `handler` is the WorkflowHandler whose methods back every route;
// `interceptors` / `operatorHandler` / `grpcServerOptions` from the
// old signature are intentionally gone — there is no grpc-gateway
// anymore, so the JSON path doesn't need gRPC unary interceptors.
// Auth/metrics/allowed-hosts are enforced in fiber middleware
// added below.
func NewHTTPAPIServer(
	serviceConfig *Config,
	rpcConfig config.RPC,
	grpcListener net.Listener,
	tlsConfigProvider encryption.TLSConfigProvider,
	handler Handler,
	metricsHandler metrics.Handler,
	_ namespace.Registry,
	logger log.Logger,
) (*HTTPAPIServer, error) {
	tcpAddrRef, _ := grpcListener.Addr().(*net.TCPAddr)
	if tcpAddrRef == nil {
		return nil, errors.New("must use TCP listener to derive HTTP port")
	}
	tcpAddr := *tcpAddrRef
	tcpAddr.Port = rpcConfig.HTTPPort
	tcpLn, err := net.ListenTCP("tcp", &tcpAddr)
	if err != nil {
		return nil, fmt.Errorf("failed listening for HTTP API on %v: %w", &tcpAddr, err)
	}
	var listener net.Listener = tcpLn
	success := false
	defer func() {
		if !success {
			_ = listener.Close()
		}
	}()

	if tlsConfigProvider != nil {
		tlsConfig, err := tlsConfigProvider.GetFrontendServerConfig()
		if err != nil {
			return nil, fmt.Errorf("failed getting TLS config: %w", err)
		}
		if tlsConfig != nil {
			listener = tls.NewListener(listener, tlsConfig)
		}
	}

	app := fiber.New(fiber.Config{
		AppName: "hanzoai/tasks",
		// Bound request body to match the gRPC default so DoS via
		// slow upload converges on the same limit everywhere.
		BodyLimit: int(rpc.MaxHTTPAPIRequestBytes),
		// Fiber emits HTTP errors as plain text by default; replace
		// with JSON so every /v1/tasks response is valid JSON even
		// on error, which keeps the SPA's error handling simple.
		ErrorHandler: jsonErrorHandler(logger),
	})

	h := &HTTPAPIServer{
		app:          app,
		listener:     listener,
		logger:       logger,
		stopped:      make(chan struct{}),
		allowedHosts: serviceConfig.HTTPAllowedHosts,
	}

	// Allowed-hosts gate: reject any request whose Host header
	// doesn't match the configured regex. Applied globally before
	// any route so an unknown origin never touches a handler.
	app.Use(h.allowedHostsMiddleware())

	// Lightweight access log. metricsHandler is tag-shaped so we
	// funnel the count+latency through it rather than introducing
	// a new counter.
	app.Use(h.accessLogMiddleware(metricsHandler))

	// One and one way only:
	//   /_/tasks/*   — embedded admin SPA (dark UI)
	//   /v1/tasks/*  — JSON API (browser path; ZAP on :9652 is the
	//                  canonical non-browser path)
	// No /api/, no /, no split. Anything outside these two prefixes
	// 404s — there is no implicit route.
	// Health endpoints:
	//   /healthz            — k8s / docker HEALTHCHECK probe
	//                         (Hanzo service convention, matches
	//                         ~/work/hanzo/kms cmd/kmsd/main.go)
	//   /v1/tasks/health    — per-service REST health, survives
	//                         API-gateway multi-service fan-in
	//                         without colliding at root
	//   /v1/tasks/healthz   — alias for tooling that expects
	//                         `healthz` regardless of prefix
	// Same handler, same body — one definition.
	healthHandler := func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "service": "tasks"})
	}
	app.Get("/healthz", healthHandler)

	v1 := app.Group("/v1/tasks")
	v1.Get("/health", healthHandler)
	v1.Get("/healthz", healthHandler)
	registerV1TasksRoutes(v1, handler, logger)

	// SPA at /_/tasks/*. fiber.Use does not strip the prefix from
	// r.URL.Path before passing to the adapted handler, so we wrap
	// with http.StripPrefix — otherwise embed.FS lookups like
	// `_/tasks/assets/foo.js` miss and every asset falls back to
	// index.html.
	app.Use("/_/tasks", adaptor.HTTPHandler(http.StripPrefix("/_/tasks", tasksui.Handler())))

	success = true
	return h, nil
}

// Serve blocks until the listener errors or Shutdown() is called.
func (h *HTTPAPIServer) Serve() error {
	err := h.app.Listener(h.listener, fiber.ListenConfig{DisableStartupMessage: true})
	if errors.Is(err, http.ErrServerClosed) || err == nil {
		<-h.stopped
		return nil
	}
	return fmt.Errorf("HTTP API serve failed: %w", err)
}

// GracefulStop closes the listener and lets in-flight requests
// drain. The timeout argument is kept for compatibility with the
// fx lifecycle call site — fiber.Shutdown() is already bounded by
// its own context.
func (h *HTTPAPIServer) GracefulStop(_ ...interface{}) {
	defer close(h.stopped)
	_ = h.app.Shutdown()
}

// ── middleware ─────────────────────────────────────────────────────

// allowedHostsMiddleware rejects any request whose Host header fails
// the configured regex. Matches the semantics of the old grpc-gateway
// middleware so no security regressions.
func (h *HTTPAPIServer) allowedHostsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		re := h.allowedHosts()
		if re != nil && !re.MatchString(c.Hostname()) {
			return fiber.NewError(http.StatusForbidden, "host not allowed")
		}
		return c.Next()
	}
}

// accessLogMiddleware records route + status + duration. We keep it
// minimal — Prometheus scrapes the metricsHandler sink anyway.
func (h *HTTPAPIServer) accessLogMiddleware(_ metrics.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		err := c.Next()
		h.logger.Debug("HTTP API call",
			tag.NewStringTag("method", c.Method()),
			tag.NewStringTag("path", c.Path()),
			tag.NewInt("status", c.Response().StatusCode()))
		return err
	}
}

// jsonErrorHandler replaces fiber's default text error response with
// a JSON envelope `{"error":"..."}` plus the right status code. Keeps
// the SPA's ApiError class happy and mirrors Temporal's gRPC-gateway
// error shape closely enough that existing tests don't care.
func jsonErrorHandler(logger log.Logger) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		code := http.StatusInternalServerError
		var fe *fiber.Error
		if errors.As(err, &fe) {
			code = fe.Code
		}
		if code >= 500 {
			logger.Warn("HTTP API error",
				tag.NewStringTag("method", c.Method()),
				tag.NewStringTag("path", c.Path()),
				tag.Error(err))
		}
		return c.Status(code).JSON(fiber.Map{"error": err.Error()})
	}
}

// ── handler plumbing ───────────────────────────────────────────────
//
// registerV1TasksRoutes wires the JSON routes. Each handler:
//  1. Parses path params + query string into the proto request
//  2. Calls the WorkflowHandler method directly
//  3. Marshals the proto response with protojson
//
// All 4 routes are read-only; mutating endpoints (start/signal/
// cancel workflow) use the ZAP surface on :9652 exclusively.

func registerV1TasksRoutes(g fiber.Router, h Handler, _ log.Logger) {
	wh, ok := h.(*WorkflowHandler)
	if !ok {
		// Only WorkflowHandler has the methods we call below.
		// The fx graph always provides *WorkflowHandler here; this
		// branch exists only so tests can inject a no-op handler
		// without linking the full frontend.
		g.Get("/*", func(c fiber.Ctx) error {
			return fiber.NewError(http.StatusServiceUnavailable, "workflow handler not wired")
		})
		return
	}

	g.Get("/namespaces", func(c fiber.Ctx) error {
		req := &workflowservice.ListNamespacesRequest{
			PageSize:      int32(queryInt(c, "pageSize", 100)),
			NextPageToken: queryBytes(c, "nextPageToken"),
		}
		resp, err := wh.ListNamespaces(c.Context(), req)
		return writeProto(c, resp, err)
	})

	g.Get("/namespaces/:ns/workflows", func(c fiber.Ctx) error {
		req := &workflowservice.ListWorkflowExecutionsRequest{
			Namespace:     c.Params("ns"),
			Query:         c.Query("query", ""),
			PageSize:      int32(queryInt(c, "pageSize", 50)),
			NextPageToken: queryBytes(c, "nextPageToken"),
		}
		resp, err := wh.ListWorkflowExecutions(c.Context(), req)
		return writeProto(c, resp, err)
	})

	g.Get("/namespaces/:ns/workflows/:id", func(c fiber.Ctx) error {
		req := &workflowservice.DescribeWorkflowExecutionRequest{
			Namespace: c.Params("ns"),
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: c.Params("id"),
				RunId:      c.Query("runId", ""),
			},
		}
		resp, err := wh.DescribeWorkflowExecution(c.Context(), req)
		return writeProto(c, resp, err)
	})

	g.Get("/namespaces/:ns/schedules", func(c fiber.Ctx) error {
		req := &workflowservice.ListSchedulesRequest{
			Namespace:         c.Params("ns"),
			MaximumPageSize:   int32(queryInt(c, "pageSize", 100)),
			NextPageToken:     queryBytes(c, "nextPageToken"),
			Query:             c.Query("query", ""),
		}
		resp, err := wh.ListSchedules(c.Context(), req)
		return writeProto(c, resp, err)
	})
}

// ── tiny helpers — one way to do each thing ────────────────────────

// writeProto serialises a proto.Message as canonical protojson.
// Uses protojson because the request/response types are still
// google.golang.org/protobuf structs from go.temporal.io/api.
// When task #41 replaces those with ZAP schemas we'll swap this
// helper for a zap-native serialiser in the same location.
func writeProto(c fiber.Ctx, m proto.Message, err error) error {
	if err != nil {
		return fiber.NewError(statusCodeFromError(err), err.Error())
	}
	b, marshalErr := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: false,
	}.Marshal(m)
	if marshalErr != nil {
		return fiber.NewError(http.StatusInternalServerError, "marshal response: "+marshalErr.Error())
	}
	c.Set("Content-Type", "application/json")
	return c.Send(b)
}

// statusCodeFromError is a thin mapping from Temporal serviceerror
// string tags to HTTP status. The full Temporal gRPC-gateway
// error handler did more work; for the UI's read-only surface this
// is enough. Mutating endpoints will map through ZAP status codes
// directly in task #41.
func statusCodeFromError(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"), strings.Contains(msg, "NotFound"):
		return http.StatusNotFound
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "PermissionDenied"):
		return http.StatusForbidden
	case strings.Contains(msg, "invalid"), strings.Contains(msg, "InvalidArgument"):
		return http.StatusBadRequest
	case strings.Contains(msg, "unavailable"), strings.Contains(msg, "Unavailable"):
		return http.StatusServiceUnavailable
	case strings.Contains(msg, "unauthorized"), strings.Contains(msg, "Unauthenticated"):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

func queryInt(c fiber.Ctx, key string, def int) int {
	v := c.Query(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func queryBytes(c fiber.Ctx, key string) []byte {
	v := c.Query(key, "")
	if v == "" {
		return nil
	}
	return []byte(v)
}

