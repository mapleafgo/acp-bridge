package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/instance"
	"github.com/mapleafgo/acp-bridge/internal/mcp"
)

type lifecycleManager interface {
	Close(context.Context) error
}

type runnableServer interface {
	Run() error
}

func run() error {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}

	var h slog.Handler
	switch strings.ToLower(cfg.LogFormat) {
	case "json":
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)})
	default:
		h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)})
	}
	slog.SetDefault(slog.New(h))

	manager := instance.NewManager(cfg, instance.DefaultFactory(cfg))
	server := mcp.NewServer(cfg, manager)

	slog.Info("acp-bridge starting",
		"codex_path", cfg.CodexPath,
		"log_level", cfg.LogLevel,
		"max_sessions", cfg.MaxSessions,
	)

	return runWith(manager, server)
}

func runWith(manager lifecycleManager, server runnableServer) error {
	runErr := server.Run()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(runErr, manager.Close(shutdownCtx))
}

func main() {
	if err := run(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
