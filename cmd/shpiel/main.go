// Command shpiel is an HF-compatible model relay: it speaks the Hugging
// Face Hub API on the front and writes to pluggable backends on the back,
// so every existing HF tool works unchanged by setting HF_ENDPOINT.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/loewenthal-corp/shpiel/internal/app"
	"github.com/loewenthal-corp/shpiel/internal/buildinfo"
	"github.com/loewenthal-corp/shpiel/internal/config"
)

type cli struct {
	Serve   serveCmd         `cmd:"" help:"Run the relay server."`
	Config  configCmd        `cmd:"" help:"Configuration utilities."`
	Version kong.VersionFlag `help:"Print version and exit."`
	Ver     versionCmd       `cmd:"" name:"version" help:"Print version."`
}

type serveCmd struct {
	Config string `short:"c" env:"SHPIEL_CONFIG" placeholder:"config.yaml" help:"Path to the YAML configuration file."`
	Local  bool   `env:"SHPIEL_LOCAL" help:"Zero-config laptop mode: localhost bind, filesystem backend, pull-through from huggingface.co."`

	// Flag overrides: flags > env > config file > defaults.
	ListenAPI     string `env:"SHPIEL_LISTEN_API" placeholder:":8080" help:"Override listen.api."`
	ListenMetrics string `env:"SHPIEL_LISTEN_METRICS" placeholder:":9090" help:"Override listen.metrics."`
	DataDir       string `env:"SHPIEL_DATA_DIR" placeholder:"~/.shpiel" help:"Storage root for --local mode."`
	LogLevel      string `env:"SHPIEL_LOG_LEVEL" help:"Override log.level (debug|info|warn|error)."`
	LogFormat     string `env:"SHPIEL_LOG_FORMAT" help:"Override log.format (json|text)."`
}

func (c *serveCmd) load() (config.Config, error) {
	var cfg config.Config
	switch {
	case c.Local && c.Config != "":
		return cfg, fmt.Errorf("--local and --config are mutually exclusive")
	case c.Local:
		dataDir := c.DataDir
		if dataDir == "" {
			dataDir = config.DefaultLocalDataDir()
		}
		cfg = config.Local(dataDir)
	case c.Config != "":
		var err error
		cfg, err = config.Load(c.Config)
		if err != nil {
			return cfg, err
		}
	default:
		return cfg, fmt.Errorf("either --config or --local is required")
	}

	if c.ListenAPI != "" {
		cfg.Listen.API = c.ListenAPI
	}
	if c.ListenMetrics != "" {
		cfg.Listen.Metrics = c.ListenMetrics
	}
	if c.LogLevel != "" {
		cfg.Log.Level = c.LogLevel
	}
	if c.LogFormat != "" {
		cfg.Log.Format = c.LogFormat
	}
	return cfg, nil
}

func (c *serveCmd) Run() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	a, err := app.Build(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a.Log.Info("starting shpiel",
		"version", buildinfo.String(),
		"pull_through", a.Relay.PullThroughEnabled(),
		"backends", len(cfg.Backends),
	)
	return a.Server.Run(ctx)
}

type configCmd struct {
	Validate configValidateCmd `cmd:"" help:"Validate a configuration file."`
}

type configValidateCmd struct {
	Config string `arg:"" placeholder:"config.yaml" help:"Path to the YAML configuration file."`
}

func (c *configValidateCmd) Run() error {
	cfg, err := config.Load(c.Config)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s is invalid:\n%w", c.Config, err)
	}
	fmt.Printf("%s is valid\n", c.Config)
	return nil
}

type versionCmd struct{}

func (versionCmd) Run() error {
	fmt.Println("shpiel " + buildinfo.String())
	return nil
}

func main() {
	k := kong.Parse(&cli{},
		kong.Name("shpiel"),
		kong.Description("HF-compatible model relay: Hugging Face API on the front, pluggable backends (OCI, S3, filesystem, upstream HF) on the back."),
		kong.UsageOnError(),
		kong.Vars{"version": "shpiel " + buildinfo.String()},
	)
	if err := k.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "shpiel: "+err.Error())
		os.Exit(1)
	}
}
