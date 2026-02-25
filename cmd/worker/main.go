package main

import (
	"fmt"
	"os"
	_ "time/tzdata" // embed tzdata as a fallback

	"github.com/temporalio/temporal-managed-workers/wci"
	"github.com/urfave/cli/v2"
	"go.temporal.io/server/common/build"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/debug"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/headers"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/membership/ringpop"
	"go.temporal.io/server/common/metrics"
	persistenceClient "go.temporal.io/server/common/persistence/client"
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/mysql"      // needed to load mysql plugin
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/postgresql" // needed to load postgresql plugin
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite"     // needed to load sqlite plugin
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/resolver"
	"go.temporal.io/server/common/rpc/encryption"
	"go.temporal.io/server/temporal"
	"go.uber.org/fx"
)

// main entry point for the temporal server
func main() {
	app := buildCLI()
	_ = app.Run(os.Args)
}

// buildCLI is the main entry point for the temporal server
func buildCLI() *cli.App {
	app := cli.NewApp()
	app.Name = "temporal-managed-workers"
	app.Usage = "Temporal Managed workers - worker"
	app.Version = headers.ServerVersion
	app.ArgsUsage = " "
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "config-file",
			Usage:   "path to config file (absolute or relative to current working directory)",
			EnvVars: []string{config.EnvKeyConfigFile},
		},
	}

	app.Commands = []*cli.Command{
		{
			Name:      "start",
			Usage:     "Start Managed Temporal Workers support services",
			ArgsUsage: " ",
			Flags:     []cli.Flag{},
			Before: func(c *cli.Context) error {
				if c.Args().Len() > 0 {
					return cli.Exit("ERROR: start command doesn't support arguments.", 1)
				}
				return nil
			},
			Action: func(c *cli.Context) error {
				var cfg *config.Config
				var err error

				switch {
				case c.IsSet("config-file"):
					cfg, err = config.Load(config.WithConfigFile(c.String("config-file")))
				default:
					cfg, err = config.Load(config.WithEmbedded())
				}

				if err != nil {
					return cli.Exit(fmt.Sprintf("Unable to load configuration: %v.", err), 1)
				}

				logger := log.NewZapLogger(log.BuildZapLogger(cfg.Log))
				logger.Info("Build info.",
					tag.NewTimeTag("git-time", build.InfoData.GitTime),
					tag.NewStringTag("git-revision", build.InfoData.GitRevision),
					tag.NewBoolTag("git-modified", build.InfoData.GitModified),
					tag.NewStringTag("go-arch", build.InfoData.GoArch),
					tag.NewStringTag("go-os", build.InfoData.GoOs),
					tag.NewStringTag("go-version", build.InfoData.GoVersion),
					tag.NewBoolTag("cgo-enabled", build.InfoData.CgoEnabled),
					tag.NewStringTag("server-version", headers.ServerVersion),
					tag.NewBoolTag("debug-mode", debug.Enabled),
				)

				ringpop.RegisterServiceNameToServiceTypeEnum(wci.ServiceName, wci.ServiceType)

				var dynamicConfigClient dynamicconfig.Client
				if cfg.DynamicConfigClient != nil {
					dynamicConfigClient, err = dynamicconfig.NewFileBasedClient(cfg.DynamicConfigClient, logger, temporal.InterruptCh())
					if err != nil {
						return cli.Exit(fmt.Sprintf("Unable to create dynamic config client. Error: %v", err), 1)
					}
				} else {
					dynamicConfigClient = dynamicconfig.NewNoopClient()
					logger.Info("Dynamic config client is not configured. Using noop client.")
				}

				metricsHandler, err := metrics.MetricsHandlerFromConfig(logger, cfg.Global.Metrics)
				if err != nil {
					return cli.Exit(fmt.Sprintf("unable to create metrics handler: %v.", err), 1)
				}

				serviceResolver := resolver.NewNoopResolver()

				tlsConfigProvider, err := encryption.NewTLSConfigProviderFromConfig(cfg.Global.TLS, metricsHandler, logger, nil)
				if err != nil {
					return cli.Exit(fmt.Sprintf("unable to create TLS config provider: %v.", err), 1)
				}

				app := fx.New(
					fx.Supply(logger, cfg, &cfg.Global.PProf, &cfg.Persistence, cfg.ClusterMetadata),
					config.Module,
					temporal.FxLogAdapter,
					ringpop.MembershipModule,
					fx.Provide(
						func() primitives.ServiceName {
							return wci.ServiceName
						},
						func() log.Logger {
							return logger
						},
						func() metrics.Handler {
							return metricsHandler.WithTags(metrics.ServiceNameTag(wci.ServiceName))
						},
						func() log.SnTaggedLogger {
							return log.With(logger, tag.Service(wci.ServiceName))
						},
						func() resolver.ServiceResolver {
							return serviceResolver
						},
						func() persistenceClient.AbstractDataStoreFactory {
							return nil
						},
						func() dynamicconfig.Client {
							return dynamicConfigClient
						},
						func() encryption.TLSConfigProvider {
							return tlsConfigProvider
						},
					),
					wci.Module,
				)
				if err := app.Err(); err != nil {
					return cli.Exit(fmt.Sprintf("Unable to create server. Error: %v.", err), 1)
				}

				if err = app.Start(c.Context); err != nil {
					return cli.Exit(fmt.Sprintf("Unable to start server. Error: %v", err), 1)
				}

				logger.Info("Running - waiting for interrupt signal")
				interruptChannel := temporal.InterruptCh()
				interruptSignal := <-interruptChannel
				logger.Info("Received interrupt signal, stopping the server.", tag.Value(interruptSignal))

				if err := app.Stop(c.Context); err != nil {
					return cli.Exit(fmt.Sprintf("Unable to stop server. Error: %v", err), 1)
				}

				return cli.Exit("All services are stopped.", 0)
			},
		},
	}
	return app
}
