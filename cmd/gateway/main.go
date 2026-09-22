package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML configuration file")
	flag.Parse()

	// Setup JSON structured logger adhering to company SRE observability standards
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Check if config exists, fallback to config.example.yaml if config.yaml missing
	if _, err := os.Stat(*configPath); os.IsNotExist(err) && *configPath == "config.yaml" {
		if _, err := os.Stat("config.example.yaml"); err == nil {
			*configPath = "config.example.yaml"
			logger.Info("Default config.yaml not found, falling back to config.example.yaml")
		}
	}

	logger.Info("Loading AI Gateway configuration", "path", *configPath)
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logger.Error("Failed to initialize gateway configuration", "error", err)
		os.Exit(1)
	}

	gatewayServer := server.NewServer(cfg, logger)

	// Channel to listen for termination signals
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	// Launch HTTP server in background goroutine
	serverErrChan := make(chan error, 1)
	go func() {
		if err := gatewayServer.Start(); err != nil {
			serverErrChan <- err
		}
	}()

	logger.Info("AI Gateway Layer 2 (SRE Resilience & Health Engine) is operational",
		"address", fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
	)

	// Block until signal or fatal server error
	select {
	case sig := <-stopChan:
		logger.Info("Received shutdown signal", "signal", sig.String())
	case err := <-serverErrChan:
		logger.Error("Fatal server error encountered", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown context
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := gatewayServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Graceful shutdown encountered errors", "error", err)
		os.Exit(1)
	}

	logger.Info("AI Gateway terminated cleanly")
}
