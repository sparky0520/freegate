package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gateway/internal/cli"
	"gateway/internal/config"
	"gateway/internal/provider"
	"gateway/internal/registry"
	"gateway/internal/server"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			if err := cli.InitCommand(os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return

		case "test":
			fs := flag.NewFlagSet("test", flag.ExitOnError)
			confPath := fs.String("config", "", "path to config.toml")
			modelsPath := fs.String("models", "", "path to models.toml")
			_ = fs.Parse(os.Args[2:])
			if err := cli.TestCommand(*confPath, *modelsPath, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return

		case "status":
			fs := flag.NewFlagSet("status", flag.ExitOnError)
			addr := fs.String("addr", "127.0.0.1:8080", "gateway address")
			_ = fs.Parse(os.Args[2:])
			if err := cli.StatusCommand(*addr, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return

		case "models":
			fs := flag.NewFlagSet("models", flag.ExitOnError)
			addr := fs.String("addr", "127.0.0.1:8080", "gateway address")
			_ = fs.Parse(os.Args[2:])
			if err := cli.ModelsCommand(*addr, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return

		case "start":
			runServer(os.Args[2:])
			return

		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	// Default: run server with args
	runServer(os.Args[1:])
}

func printHelp() {
	fmt.Println(`freegate - local OpenAI-compatible proxy for free-tier LLMs

Usage:
  freegate init
  freegate start [--addr 127.0.0.1:8080] [--config path] [--models path] [--private] [--debug]
  freegate status [--addr 127.0.0.1:8080]
  freegate models [--addr 127.0.0.1:8080]
  freegate test [--config path] [--models path]
  freegate help`)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("freegate", flag.ExitOnError)
	addr := fs.String("addr", "", "address to listen on (default from config)")
	confPath := fs.String("config", "", "path to config.toml")
	modelsPath := fs.String("models", "", "path to models.toml")
	private := fs.Bool("private", false, "force private mode")
	debug := fs.Bool("debug", false, "enable debug logging")
	noAuth := fs.Bool("no-auth", false, "do not set upstream authorization (for testing)")
	_ = fs.Parse(args)

	// Load local .env if present
	if !*noAuth {
		config.LoadDotEnv(".env")
	}

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Load config
	cfg, err := config.LoadConfig(*confPath)
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	if *addr != "" {
		cfg.Listen = *addr
	}
	if *private {
		cfg.PrivateMode = true
	}

	// Load model metadata
	metadata, err := config.LoadModelMetadata(*modelsPath)
	if err != nil {
		logger.Error("failed to load model metadata", "err", err)
		os.Exit(1)
	}

	// Initialize Registry
	reg := registry.New(cfg, metadata)

	// Run initial discovery and background worker
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}
	go provider.StartDiscoveryWorker(ctx, cfg, reg, client, logger)

	// Create and start HTTP Server
	srv := server.NewServer(cfg, reg, nil, logger)
	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	var activeProviders []string
	for _, p := range cfg.Providers {
		if p.Local || (p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "") {
			activeProviders = append(activeProviders, p.Name)
		}
	}

	logger.Info("freegate gateway started",
		"listen", cfg.Listen,
		"buckets", len(reg.AllBuckets()),
		"active_providers", activeProviders,
		"private_mode", cfg.PrivateMode,
	)
	if len(activeProviders) == 0 || (len(activeProviders) == 1 && activeProviders[0] == "ollama") {
		logger.Warn("NO REMOTE API KEYS CONFIGURED! Add GROQ_API_KEY, OPENROUTER_API_KEY, or GEMINI_API_KEY to .env or ~/.config/freegate/.env")
	}

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
