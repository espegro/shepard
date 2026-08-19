package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shepard/internal/config"
	"shepard/internal/gateway"
)

func main() {
	configPath := flag.String("config", "shepard.yaml", "path to the configuration file")
	check := flag.Bool("check", false, "validate the configuration and exit")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}
	if *check {
		logger.Info("configuration is valid", "path", *configPath)
		return
	}
	if len(cfg.Server.InboundAPIKeys) == 0 {
		logger.Warn("inbound authentication is disabled; Shepard is accepting unauthenticated requests")
	}

	gw, err := gateway.New(cfg, logger)
	if err != nil {
		logger.Error("initialize gateway", "error", err)
		os.Exit(1)
	}
	defer gw.Close()
	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           gw,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       cfg.Server.ReadTimeout.Duration,
		IdleTimeout:       cfg.Server.IdleTimeout.Duration,
	}

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	go func() {
		for range reload {
			next, err := config.Load(*configPath)
			if err != nil {
				logger.Error("reload configuration", "error", err)
				continue
			}
			if err := gw.Reload(next); err != nil {
				logger.Error("reload configuration", "error", err)
				continue
			}
			logger.Info("configuration reloaded")
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("shutdown", "error", err)
		}
	}()

	logger.Info("shepard listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("serve", "error", err)
		os.Exit(1)
	}
}
