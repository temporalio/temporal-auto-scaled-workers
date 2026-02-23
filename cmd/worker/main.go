package main

import (
	"fmt"
	"os"
	_ "time/tzdata" // embed tzdata as a fallback

	"github.com/temporalio/temporal-managed-workers/wci"
	"github.com/urfave/cli/v2"
	"go.temporal.io/server/common/authorization"
	"go.temporal.io/server/common/build"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/debug"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/headers"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/mysql"      // needed to load mysql plugin
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/postgresql" // needed to load postgresql plugin
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite"     // needed to load sqlite plugin
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/temporal"
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
		&cli.BoolFlag{
			Name:    "allow-no-auth",
			Usage:   "allow no authorizer",
			EnvVars: []string{config.EnvKeyAllowNoAuth},
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
				allowNoAuth := c.Bool("allow-no-auth")

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
					temporal.ForServices([]string{}),
					temporal.WithExternalService(primitives.WorkerControllerService, wci.Module),
					temporal.WithConfig(cfg),
					temporal.WithDynamicConfigClient(dynamicConfigClient),
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
	}
	return app
}
