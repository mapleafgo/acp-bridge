package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds all configuration for acp-bridge, loaded from ACP_BRIDGE_* env vars.
type Config struct {
	// Agent paths
	CodexPath       string   `env:"ACP_BRIDGE_CODEX_PATH" envDefault:"npx -y @agentclientprotocol/codex-acp"`
	CodexArgs       []string `env:"ACP_BRIDGE_CODEX_ARGS" envSeparator:" "`
	ClaudeAgentPath string   `env:"ACP_BRIDGE_CLAUDE_PATH" envDefault:"npx -y @agentclientprotocol/claude-agent-acp"`
	ClaudeAgentArgs []string `env:"ACP_BRIDGE_CLAUDE_ARGS" envSeparator:" "`
	GeminiAgentPath string   `env:"ACP_BRIDGE_GEMINI_PATH" envDefault:"npx -y @agentclientprotocol/gemini-agent-acp"`
	GeminiAgentArgs []string `env:"ACP_BRIDGE_GEMINI_ARGS" envSeparator:" "`
	OpenCodePath    string   `env:"ACP_BRIDGE_OPENCODE_PATH" envDefault:"npx -y opencode-ai acp"`
	OpenCodeArgs    []string `env:"ACP_BRIDGE_OPENCODE_ARGS" envSeparator:" "`

	// Behaviour
	DefaultTimeout time.Duration `env:"ACP_BRIDGE_DEFAULT_TIMEOUT" envDefault:"300s"`
	SessionTTL     time.Duration `env:"ACP_BRIDGE_SESSION_TTL" envDefault:"1800s"`
	MaxSessions    int           `env:"ACP_BRIDGE_MAX_SESSIONS" envDefault:"10"`

	// Diagnostics
	LogLevel  string `env:"ACP_BRIDGE_LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"ACP_BRIDGE_LOG_FORMAT" envDefault:"text"`
}

// Load reads ACP_BRIDGE_* environment variables and returns a Config with defaults.
func Load() *Config {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		panic(err)
	}
	return cfg
}
