package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/mcp"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

func main() {
	cfg := config.Load()

	var h slog.Handler
	switch strings.ToLower(cfg.LogFormat) {
	case "json":
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)})
	default:
		h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)})
	}
	slog.SetDefault(slog.New(h))

	pool := session.NewPool(cfg)
	server := mcp.NewServer(cfg, pool)

	slog.Info("acp-bridge starting",
		"codex_path", cfg.CodexPath,
		"log_level", cfg.LogLevel,
		"max_sessions", cfg.MaxSessions,
	)

	if err := server.Run(); err != nil {
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
