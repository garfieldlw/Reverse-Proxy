package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/logger"
	"github.com/garfieldlw/reverse-proxy/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		fmt.Fprintf(os.Stderr, "error initializing logger: %v\n", err)
		os.Exit(1)
	}

	log := slog.Default()
	log.Info("starting reverse proxy", "config", *configPath)

	srv, err := server.NewServer(cfg, log)
	if err != nil {
		log.Error("error creating server", "error", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		log.Error("error starting server", "error", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info("received shutdown signal", "signal", sig)

	if err := srv.Shutdown(30 * time.Second); err != nil {
		log.Error("error during shutdown", "error", err)
		os.Exit(1)
	}

	log.Info("reverse proxy stopped")
}
