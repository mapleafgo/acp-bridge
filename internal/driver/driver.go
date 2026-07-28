// Package driver defines the AgentDriver interface and provides
// implementations for codex-acp, claude-agent-acp, and gemini-agent-acp.
package driver

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/mapleafgo/acp-bridge/internal/config"
)

// AgentType identifies the ACP-compatible agent kind.
type AgentType string

const (
	AgentTypeCodex    AgentType = "codex"
	AgentTypeClaude   AgentType = "claude"
	AgentTypeGemini   AgentType = "gemini"
	AgentTypeOpenCode AgentType = "opencode"
)

// AgentCapabilities describes which optional ACP features the agent supports.
type AgentCapabilities struct {
	SupportsLoadSession  bool
	SupportsFork         bool
	SupportsResume       bool
	SupportsListSessions bool
	SupportsSetModel     bool
	SupportsAuthenticate bool
}

// AgentDriver is the abstraction that unifies the startup and lifecycle
// management of the three supported ACP agent types.
type AgentDriver interface {
	// Type returns which agent this driver handles.
	Type() AgentType

	// Start launches the agent subprocess and returns pipes for
	// communicating with it via the ACP JSON-RPC 2.0 protocol over stdio.
	Start(ctx context.Context) (stdout io.ReadCloser, stdin io.WriteCloser, stderr io.ReadCloser, err error)

	// Capabilities reports which ACP protocol extensions this agent supports.
	Capabilities() AgentCapabilities
}

// StartError contains detailed information about a failed agent subprocess start.
type StartError struct {
	AgentType AgentType
	Path      string
	ExitCode  int
	Stderr    string
	Err       error
}

func (e *StartError) Error() string {
	return fmt.Sprintf("failed to start %s agent (path=%s, exit=%d): %s\nstderr: %s",
		e.AgentType, e.Path, e.ExitCode, e.Err, e.Stderr)
}

// defaultCapabilities returns the common capability set shared by all agents.
func defaultCapabilities() AgentCapabilities {
	return AgentCapabilities{
		SupportsLoadSession:  true,
		SupportsFork:         true,
		SupportsResume:       true,
		SupportsListSessions: true,
		SupportsSetModel:     true,
	}
}

// NewDriver creates the appropriate AgentDriver for the given agent type.
func NewDriver(agentType AgentType, cfg *config.Config) (AgentDriver, error) {
	switch agentType {
	case AgentTypeCodex:
		return &CodexDriver{path: cfg.CodexPath, args: cfg.CodexArgs}, nil
	case AgentTypeClaude:
		return &ClaudeDriver{path: cfg.ClaudeAgentPath, args: cfg.ClaudeAgentArgs}, nil
	case AgentTypeGemini:
		return &GeminiDriver{path: cfg.GeminiAgentPath, args: cfg.GeminiAgentArgs}, nil
	case AgentTypeOpenCode:
		return &OpenCodeDriver{path: cfg.OpenCodePath, args: cfg.OpenCodeArgs}, nil
	default:
		return nil, fmt.Errorf("unknown agent type: %s", agentType)
	}
}

// startPipes launches a subprocess and returns stdout/stdin/stderr pipes.
// Shared by all three driver implementations.
func startPipes(ctx context.Context, exe string, args []string) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	cmd, err := buildCmd(ctx, exe, args)
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return stdout, stdin, stderr, nil
}

// buildCmd constructs an exec.Cmd with binary existence checking and
// INITIAL_AGENT_MODE passthrough. Returns a StartError if the binary
// cannot be found.
func buildCmd(ctx context.Context, exe string, args []string) (*exec.Cmd, error) {
	// Binary existence check: skip for "npx" (validated at runtime by npx itself).
	if exe != "npx" {
		if _, err := exec.LookPath(exe); err != nil {
			return nil, &StartError{
				Path: exe,
				Err:  fmt.Errorf("binary not found: %w", err),
			}
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	// Pass through INITIAL_AGENT_MODE if set in the environment.
	if mode := os.Getenv("INITIAL_AGENT_MODE"); mode != "" {
		cmd.Env = append(os.Environ(), "INITIAL_AGENT_MODE="+mode)
	}
	return cmd, nil
}
