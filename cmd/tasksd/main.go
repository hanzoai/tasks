package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	_ "time/tzdata" // embed tzdata as a fallback

	"github.com/urfave/cli/v2"
	"github.com/hanzoai/tasks/common/authorization"
	"github.com/hanzoai/tasks/common/build"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/debug"
	"github.com/hanzoai/tasks/common/dynamicconfig"
	"github.com/hanzoai/tasks/common/headers"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/log/tag"
	_ "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/mysql"      // needed to load mysql plugin
	_ "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/postgresql" // needed to load postgresql plugin
	_ "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/sqlite"     // needed to load sqlite plugin
	"github.com/hanzoai/tasks/temporal"
	embedded "github.com/hanzoai/tasks/temporaltest/embedded"
)

// main entry point for the Hanzo Tasks server
func main() {
	app := buildCLI()
	_ = app.Run(os.Args)
}

// buildCLI is the main entry point for the Hanzo Tasks server
func buildCLI() *cli.App {
	app := cli.NewApp()
	app.Name = "tasksd"
	app.Usage = "Hanzo Tasks server"
	app.Version = headers.ServerVersion
	app.ArgsUsage = " "
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "root",
			Aliases: []string{"r"},
			Value:   ".",
			Usage:   "root directory of execution environment (deprecated)",
			EnvVars: []string{config.EnvKeyRoot},
		},
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Value:   "config",
			Usage:   "config dir path relative to root (deprecated)",
			EnvVars: []string{config.EnvKeyConfigDir},
		},
		&cli.StringFlag{
			Name:    "env",
			Aliases: []string{"e"},
			Value:   "development",
			Usage:   "runtime environment (deprecated)",
			EnvVars: []string{config.EnvKeyEnvironment},
		},
		&cli.StringFlag{
			Name:    "zone",
			Aliases: []string{"az"},
			Usage:   "availability zone (deprecated)",
			EnvVars: []string{config.EnvKeyAvailabilityZone, config.EnvKeyAvailabilityZoneTypo},
		},
		&cli.StringFlag{
			Name:    "config-file",
			Usage:   "path to config file (absolute or relative to current working directory)",
			EnvVars: []string{config.EnvKeyConfigFile},
		},
		&cli.BoolFlag{
			Name:    "allow-no-auth",
			Usage:   "allow no authorizer",
			EnvVars: []string{config.EnvKeyAllowNoAuth},
		},
	}

	app.Commands = []*cli.Command{
		{
			Name:      "validate-dynamic-config",
			Usage:     "Validate a dynamic config file[s] with known keys and types",
			ArgsUsage: "<file> ...",
			Action: func(c *cli.Context) error {
				total := 0
				for _, fileName := range c.Args().Slice() {
					contents, err := os.ReadFile(fileName)
					if err != nil {
						return err
					}
					result := dynamicconfig.LoadYamlFile(contents)
					total += len(result.Errors)
					fmt.Println(fileName)
					t := template.Must(template.New("").Parse(
						"{{range .Errors}}  error: {{.}}\n" +
							"{{end}}{{range .Warnings}}  warning: {{.}}\n" +
							"{{end}}",
					))
					_ = t.Execute(os.Stdout, result)
				}
				if total > 0 {
					return fmt.Errorf("%d total errors", total)
				}
				return nil
			},
		},
		{
			Name:      "render-config",
			Usage:     "Render server config template",
			ArgsUsage: " ",
			Action: func(c *cli.Context) error {
				cfg, err := config.Load(
					config.WithEnv(c.String("env")),
					config.WithConfigDir(c.String("config")),
					config.WithZone(c.String("zone")),
				)
				if err != nil {
					return cli.Exit(fmt.Errorf("Unable to load configuration: %w", err), 1)
				}
				fmt.Println(cfg.String())
				return nil
			},
		},
		{
			Name:      "start",
			Usage:     "Start Hanzo Tasks server",
			ArgsUsage: " ",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:    "services",
					Aliases: []string{"s"},
					Usage:   "comma separated list of services to start. Deprecated",
					Hidden:  true,
				},
				&cli.StringSliceFlag{
					Name:    "service",
					Aliases: []string{"svc"},
					Value:   cli.NewStringSlice(temporal.DefaultServices...),
					Usage:   "service(s) to start",
					EnvVars: []string{"TASKS_SERVICES"},
				},
			},
			Before: func(c *cli.Context) error {
				if c.Args().Len() > 0 {
					return cli.Exit("ERROR: start command doesn't support arguments. Use --service flag instead.", 1)
				}

				if c.IsSet("config-file") && (c.IsSet("config") || c.IsSet("env") || c.IsSet("zone") || c.IsSet("root")) {
					return cli.Exit("ERROR: can not use --config, --env, --zone, or --root with --config-file", 1)
				}
				return nil
			},
			Action: func(c *cli.Context) error {
				services := c.StringSlice("service")
				allowNoAuth := c.Bool("allow-no-auth")

				// For backward compatibility to support old flag format (i.e. `--services=frontend,history,matching`).
				if c.IsSet("services") {
					log.NewCLILogger().Warn("--services flag is deprecated; pass multiple --service flags instead")
					services = strings.Split(c.String("services"), ",")
				}

				var cfg *config.Config
				var err error

				switch {
				case c.IsSet("config-file"):
					cfg, err = config.Load(config.WithConfigFile(c.String("config-file")))
				case c.IsSet("config") || c.IsSet("env") || c.IsSet("zone"):
					cfg, err = config.Load(
						config.WithEnv(c.String("env")),
						config.WithConfigDir(path.Join(c.String("root"), c.String("config"))),
						config.WithZone(c.String("zone")),
					)
				default:
					cfg, err = config.Load(config.WithEmbedded())
				}

				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to load configuration: %v.", err), 1)
				}

				logger := log.NewZapLogger(log.BuildZapLogger(cfg.Log))
				logger.Info("Build info.",
					tag.Time("git-time", build.InfoData.GitTime),
					tag.String("git-revision", build.InfoData.GitRevision),
					tag.Bool("git-modified", build.InfoData.GitModified),
					tag.String("go-arch", build.InfoData.GoArch),
					tag.String("go-os", build.InfoData.GoOs),
					tag.String("go-version", build.InfoData.GoVersion),
					tag.Bool("cgo-enabled", build.InfoData.CgoEnabled),
					tag.String("server-version", headers.ServerVersion),
					tag.Bool("debug-mode", debug.Enabled),
				)

				authorizer, err := authorization.GetAuthorizerFromConfig(
					&cfg.Global.Authorization,
				)
				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to instantiate authorizer. Error: %v", err), 1)
				}
				if authorization.IsNoopAuthorizer(authorizer) && !allowNoAuth {
					logger.Warn(
						"Not using any authorizer and flag `--allow-no-auth` not detected. " +
							"Future versions will require using the flag `--allow-no-auth` " +
							"if you do not want to set an authorizer.",
					)
				}

				// Authorization mappers: claim and audience
				claimMapper, err := authorization.GetClaimMapperFromConfig(&cfg.Global.Authorization, logger)
				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to instantiate claim mapper: %v.", err), 1)
				}

				audienceMapper, err := authorization.GetAudienceMapperFromConfig(&cfg.Global.Authorization)
				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to instantiate audience mapper: %v.", err), 1)
				}

				s, err := temporal.NewServer(
					temporal.ForServices(services),
					temporal.WithConfig(cfg),
					temporal.WithLogger(logger),
					temporal.InterruptOn(temporal.InterruptCh()),
					temporal.WithAuthorizer(authorizer),
					temporal.WithClaimMapper(func(cfg *config.Config) authorization.ClaimMapper {
						return claimMapper
					}),
					temporal.WithAudienceGetter(func(cfg *config.Config) authorization.JWTAudienceMapper {
						return audienceMapper
					}),
				)
				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to create server. Error: %v.", err), 1)
				}

				err = s.Start()
				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to start server. Error: %v", err), 1)
				}
				return cli.Exit("All services are stopped.", 0)
			},
		},
		{
			// embedded-sqlite wraps temporaltest/embedded.LiteServer so schema
			// migrations (sqlite/v1/...) run automatically on first boot. The
			// plain `start` command loads config-driven persistence but does not
			// bootstrap a fresh sqlite database. Use this command for single-pod
			// deployments backed by a persistent volume; HA callers should front
			// with a K8s Lease and run worker-only replicas.
			Name:      "embedded-sqlite",
			Usage:     "Start Hanzo Tasks with embedded sqlite persistence (auto schema bootstrap)",
			ArgsUsage: " ",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:    "namespace",
					Value:   "default",
					Usage:   "primary namespace to pre-register",
					EnvVars: []string{"TASKS_NAMESPACE"},
				},
				&cli.StringFlag{
					Name:    "db-path",
					Value:   "/data/tasks.db",
					Usage:   "sqlite database file path (parent dir must exist and be writable)",
					EnvVars: []string{"TASKS_DB_PATH"},
				},
				&cli.StringFlag{
					Name:    "bind-ip",
					Value:   "0.0.0.0",
					Usage:   "frontend gRPC bind address",
					EnvVars: []string{"TASKS_BIND_IP"},
				},
				&cli.IntFlag{
					Name:    "port",
					Value:   7233,
					Usage:   "frontend gRPC port",
					EnvVars: []string{"TASKS_PORT"},
				},
				&cli.IntFlag{
					Name:    "http-port",
					Value:   7234,
					Usage:   "frontend HTTP API port (0 disables)",
					EnvVars: []string{"TASKS_HTTP_PORT"},
				},
			},
			Action: func(c *cli.Context) error {
				ns := c.String("namespace")
				dbPath := c.String("db-path")
				if dir := filepath.Dir(dbPath); dir != "" {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return cli.Exit(fmt.Sprintf("mkdir %s: %v", dir, err), 1)
					}
				}

				logger := log.NewCLILogger()
				logger.Info("tasksd embedded-sqlite",
					tag.NewStringTag("namespace", ns),
					tag.NewStringTag("db", dbPath),
					tag.NewStringTag("bind", c.String("bind-ip")),
					tag.NewInt("port", c.Int("port")),
					tag.NewInt("http-port", c.Int("http-port")))

				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				defer stop()

				srv, err := embedded.Start(ctx, embedded.Config{
					Namespace:        ns,
					Backend:          embedded.BackendSQLite,
					Path:             dbPath,
					FrontendIP:       c.String("bind-ip"),
					FrontendPort:     c.Int("port"),
					FrontendHTTPPort: c.Int("http-port"),
				})
				if err != nil {
					return cli.Exit(fmt.Sprintf("embedded-sqlite start: %v", err), 1)
				}

				logger.Info("tasksd embedded-sqlite ready",
					tag.NewStringTag("addr", srv.FrontendHostPort()))

				<-ctx.Done()
				logger.Info("tasksd shutdown signal received, stopping...")
				if err := srv.Stop(); err != nil {
					logger.Error("tasksd stop error", tag.Error(err))
					return cli.Exit("shutdown returned error", 1)
				}
				return cli.Exit("tasksd stopped", 0)
			},
		},
	}
	return app
}
