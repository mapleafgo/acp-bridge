package config

import (
	"testing"
	"time"
)

// --- Default values ---

func TestLoadDefaults(t *testing.T) {
	cfg := Load()

	if cfg.CodexPath != "npx -y @agentclientprotocol/codex-acp" {
		t.Errorf("CodexPath = %q, want %q", cfg.CodexPath, "npx -y @agentclientprotocol/codex-acp")
	}
	if cfg.ClaudeAgentPath != "npx -y @agentclientprotocol/claude-agent-acp" {
		t.Errorf("ClaudeAgentPath = %q, want %q", cfg.ClaudeAgentPath, "npx -y @agentclientprotocol/claude-agent-acp")
	}
	if cfg.GeminiAgentPath != "npx -y @agentclientprotocol/gemini-agent-acp" {
		t.Errorf("GeminiAgentPath = %q, want %q", cfg.GeminiAgentPath, "npx -y @agentclientprotocol/gemini-agent-acp")
	}
	if cfg.OpenCodePath != "npx -y opencode-ai acp" {
		t.Errorf("OpenCodePath = %q, want %q", cfg.OpenCodePath, "npx -y opencode-ai acp")
	}
	if cfg.DefaultTimeout != 300*time.Second {
		t.Errorf("DefaultTimeout = %v, want %v", cfg.DefaultTimeout, 300*time.Second)
	}
	if cfg.MaxSessions != 10 {
		t.Errorf("MaxSessions = %d, want %d", cfg.MaxSessions, 10)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "text")
	}
}

// --- Environment variable overrides ---

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("ACP_BRIDGE_CODEX_PATH", "/custom/codex")
	t.Setenv("ACP_BRIDGE_CLAUDE_PATH", "/custom/claude")
	t.Setenv("ACP_BRIDGE_GEMINI_PATH", "/custom/gemini")
	t.Setenv("ACP_BRIDGE_OPENCODE_PATH", "/custom/opencode acp")
	t.Setenv("ACP_BRIDGE_DEFAULT_TIMEOUT", "600s")
	t.Setenv("ACP_BRIDGE_MAX_SESSIONS", "42")
	t.Setenv("ACP_BRIDGE_LOG_LEVEL", "debug")
	t.Setenv("ACP_BRIDGE_LOG_FORMAT", "json")

	cfg := Load()

	if cfg.CodexPath != "/custom/codex" {
		t.Errorf("CodexPath = %q, want %q", cfg.CodexPath, "/custom/codex")
	}
	if cfg.ClaudeAgentPath != "/custom/claude" {
		t.Errorf("ClaudeAgentPath = %q, want %q", cfg.ClaudeAgentPath, "/custom/claude")
	}
	if cfg.GeminiAgentPath != "/custom/gemini" {
		t.Errorf("GeminiAgentPath = %q, want %q", cfg.GeminiAgentPath, "/custom/gemini")
	}
	if cfg.OpenCodePath != "/custom/opencode acp" {
		t.Errorf("OpenCodePath = %q, want %q", cfg.OpenCodePath, "/custom/opencode acp")
	}
	if cfg.DefaultTimeout != 600*time.Second {
		t.Errorf("DefaultTimeout = %v, want %v", cfg.DefaultTimeout, 600*time.Second)
	}
	if cfg.MaxSessions != 42 {
		t.Errorf("MaxSessions = %d, want %d", cfg.MaxSessions, 42)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
}

func TestValidateRejectsNegativeMaxSessions(t *testing.T) {
	cfg := &Config{MaxSessions: -1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

// --- []string parsing ---

func TestLoadArgsParsing(t *testing.T) {
	t.Setenv("ACP_BRIDGE_CODEX_ARGS", "--flag value --verbose")

	cfg := Load()

	if len(cfg.CodexArgs) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(cfg.CodexArgs), cfg.CodexArgs)
	}
	if cfg.CodexArgs[0] != "--flag" {
		t.Errorf("arg[0] = %q, want %q", cfg.CodexArgs[0], "--flag")
	}
	if cfg.CodexArgs[2] != "--verbose" {
		t.Errorf("arg[2] = %q, want %q", cfg.CodexArgs[2], "--verbose")
	}
}

func TestLoadArgsEmpty(t *testing.T) {
	cfg := Load()

	if cfg.CodexArgs != nil {
		t.Errorf("expected nil CodexArgs, got %v", cfg.CodexArgs)
	}
}
