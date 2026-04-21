// Package embedded exposes a production-safe wrapper around the internal
// temporalite LiteServer. It is the single supported way for external
// Go services to run an in-process Temporal-compatible server alongside
// their main binary ("embedded mode", no separate tasksd Deployment).
//
// Use case: services that need durable, checkpointed scheduled work
// (regulatory retention jobs, settlement watchdogs, reconciliation)
// without operating a separate Temporal cluster.
//
//	srv, err := embedded.Start(ctx, embedded.Config{
//	    Namespace: "myapp",
//	    Backend:   embedded.BackendSQLite,
//	    Path:      "/data/durable.db",
//	})
//	if err != nil { ... }
//	defer srv.Stop()
//
//	c, _ := srv.NewClient(ctx)
//	w := worker.New(c, "myapp-main", worker.Options{})
//	w.RegisterWorkflow(MyWorkflow)
//	_ = w.Start()
//
// The Postgres backend is a roadmap item (see TODO below). SQLite is
// appropriate for single-leader deployments; for HA call sites should
// front the server with a K8s Lease and run a worker-only replica on
// non-leaders that dials the leader pod.
package embedded

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"github.com/hanzoai/tasks/common/dynamicconfig"
	"github.com/hanzoai/tasks/common/log"
	temporalite "github.com/hanzoai/tasks/temporaltest/internal"
)

// Backend selects the persistence driver.
type Backend string

const (
	// BackendSQLite persists history+visibility to a local SQLite file.
	// Appropriate for single-leader deployments and dev.
	BackendSQLite Backend = "sqlite"

	// BackendPostgres is reserved for HA production. Not yet implemented;
	// callers should keep using BackendSQLite behind a K8s leader-election
	// Lease until Postgres plumbing lands.
	//
	// TODO(embedded-postgres): wire common/persistence/sql/sqlplugin/postgresql
	// through a new LiteServerConfig variant, preserving schema migrations.
	BackendPostgres Backend = "postgres"
)

// Config is the embedded-server options struct. Zero values are invalid
// for required fields; see field docs.
type Config struct {
	// Namespace is pre-registered on the server and is the namespace a
	// default client will dial. Required.
	Namespace string

	// Backend selects the persistence plugin. Required. Currently only
	// BackendSQLite is supported; BackendPostgres returns an error.
	Backend Backend

	// Path is the SQLite database file path. Required when Backend=sqlite.
	// The parent directory must exist. Schema migrations run automatically
	// on first create.
	Path string

	// PostgresDSN is the full pgx DSN (postgres://user:pass@host:port/db)
	// for Backend=postgres. Reserved; see Backend doc.
	PostgresDSN string

	// FrontendIP is the address on which the frontend gRPC listens.
	// Defaults to 127.0.0.1. Do NOT expose publicly — the embedded
	// frontend is meant for in-process use only.
	FrontendIP string

	// FrontendPort forces a specific port. Leave 0 to auto-select a free
	// port; auto-select is recommended for multi-replica pods (each pod
	// gets its own loopback-only frontend).
	FrontendPort int

	// SearchAttributes registers custom search attributes on the default
	// namespace at startup (workflow.Info queries). Optional.
	SearchAttributes map[string]enumspb.IndexedValueType

	// Logger overrides the default structured logger. Optional.
	Logger log.Logger
}

// Server is a running embedded Temporal server. Safe to hold across the
// lifetime of a process; call Stop() on shutdown.
type Server struct {
	lite      *temporalite.LiteServer
	namespace string
}

// Start spins up an in-process Temporal-compatible server using the
// selected backend and pre-registers cfg.Namespace.
//
// Start blocks until the server is healthy or errors. The caller should
// run Stop() on shutdown to flush persistence cleanly.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Namespace == "" {
		return nil, errors.New("embedded: Namespace is required")
	}
	if cfg.Backend == "" {
		return nil, errors.New("embedded: Backend is required (sqlite|postgres)")
	}
	if cfg.Backend == BackendPostgres {
		return nil, errors.New("embedded: BackendPostgres not yet implemented; use BackendSQLite behind a K8s Lease for HA")
	}
	if cfg.Backend != BackendSQLite {
		return nil, fmt.Errorf("embedded: unknown backend %q", cfg.Backend)
	}
	if cfg.Path == "" {
		return nil, errors.New("embedded: Path is required for sqlite backend")
	}

	// Ensure the parent directory exists before handing off to temporalite,
	// which requires it.
	dir := filepath.Dir(cfg.Path)
	if dir == "" || dir == "." {
		return nil, fmt.Errorf("embedded: Path %q must include a directory", cfg.Path)
	}

	frontendIP := cfg.FrontendIP
	if frontendIP == "" {
		frontendIP = "127.0.0.1"
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.NewZapLogger(log.BuildZapLogger(log.Config{
			Stdout: true,
			Level:  "info",
		}))
	}

	liteCfg := &temporalite.LiteServerConfig{
		Ephemeral:        false,
		DatabaseFilePath: cfg.Path,
		FrontendIP:       frontendIP,
		FrontendPort:     cfg.FrontendPort,
		Namespaces:       []string{cfg.Namespace},
		Logger:           logger,
		SearchAttributes: cfg.SearchAttributes,
		// Force-refresh search attribute cache on read so registrations
		// become visible to workers immediately.
		DynamicConfig: dynamicconfig.StaticClient{
			dynamicconfig.ForceSearchAttributesCacheRefreshOnRead.Key(): []dynamicconfig.ConstrainedValue{{Value: true}},
		},
	}

	lite, err := temporalite.NewLiteServer(liteCfg)
	if err != nil {
		return nil, fmt.Errorf("embedded: create lite server: %w", err)
	}
	if err := lite.Start(); err != nil {
		return nil, fmt.Errorf("embedded: start lite server: %w", err)
	}

	return &Server{lite: lite, namespace: cfg.Namespace}, nil
}

// Stop gracefully shuts down the server. Safe to call on a nil receiver.
func (s *Server) Stop() error {
	if s == nil || s.lite == nil {
		return nil
	}
	return s.lite.Stop()
}

// Namespace returns the default namespace pre-registered at startup.
func (s *Server) Namespace() string {
	if s == nil {
		return ""
	}
	return s.namespace
}

// FrontendHostPort returns the host:port of the frontend gRPC for use
// by same-process clients. External callers should prefer NewClient().
func (s *Server) FrontendHostPort() string {
	if s == nil || s.lite == nil {
		return ""
	}
	return s.lite.FrontendHostPort()
}

// NewClient returns a Temporal SDK client dialed to the embedded server
// in the default namespace.
func (s *Server) NewClient(ctx context.Context) (client.Client, error) {
	if s == nil || s.lite == nil {
		return nil, errors.New("embedded: server not started")
	}
	return s.lite.NewClient(ctx, s.namespace)
}

// NewClientInNamespace returns a client dialed to an arbitrary namespace
// on this embedded server. The namespace must have been registered at
// start or via Temporal's register-namespace RPC.
func (s *Server) NewClientInNamespace(ctx context.Context, namespace string) (client.Client, error) {
	if s == nil || s.lite == nil {
		return nil, errors.New("embedded: server not started")
	}
	return s.lite.NewClientWithOptions(ctx, client.Options{Namespace: namespace})
}
