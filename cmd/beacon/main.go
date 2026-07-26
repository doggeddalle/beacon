// Command beacon is a lightweight, self-updating DLNA/UPnP-AV media server
// tuned for low-RAM ARM NAS hardware (built for the Asustor AS1102TL).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"beacon/internal/config"
	"beacon/internal/logging"
	"beacon/internal/server"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "beacon: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "beacon.toml", "path to the TOML config file")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("beacon %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log, logRing := logging.Setup(cfg.Log.Level, cfg.Log.Format)
	log.Info("starting beacon",
		"version", version,
		"os", runtime.GOOS,
		"arch", runtime.GOARCH,
		"config", cfg.Path(),
	)
	log.Info("server config",
		"friendly_name", cfg.Server.FriendlyName,
		"uuid", cfg.Server.UUID,
		"http_port", cfg.Server.HTTPPort,
		"data_dir", cfg.Server.DataDir,
	)
	log.Info("library config", "folders", len(cfg.Library.Folders))
	for _, f := range cfg.Library.Folders {
		log.Info("watched folder", "name", f.Name, "path", f.Path)
	}
	log.Info("index config",
		"workers", cfg.Index.Workers,
		"reconcile_interval", cfg.Index.ReconcileInterval.String(),
		"integrity_interval", cfg.Index.IntegrityInterval.String(),
		"write_settle_delay", cfg.Index.WriteSettleDelay.String(),
	)

	// Graceful shutdown on Ctrl-C / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(cfg.Library.Folders) == 0 {
		log.Warn("no library folders configured yet — add some under [[library.folders]] in the config file")
	}

	srv, err := server.New(cfg, log, logRing, version)
	if err != nil {
		return err
	}

	log.Info("beacon is up; press Ctrl-C to stop")
	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info("shutting down")
	return nil
}
