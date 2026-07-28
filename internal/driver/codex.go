package driver

import (
	"context"
	"strings"
)

// splitCommand splits a command string into the executable path and its initial arguments.
// e.g., "npx @agentclientprotocol/codex-acp" → ("npx", ["@agentclientprotocol/codex-acp"])
func splitCommand(cmd string) (string, []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

// CodexDriver manages a codex-acp subprocess.
type CodexDriver struct {
	path string   // the full command string, e.g. "npx @agentclientprotocol/codex-acp"
	args []string // additional driver-specific arguments
}

func (d *CodexDriver) Type() AgentType { return AgentTypeCodex }

func (d *CodexDriver) Start(ctx context.Context) (AgentProcess, error) {
	exe, initialArgs := splitCommand(d.path)
	return startProcess(ctx, exe, append(initialArgs, d.args...))
}

func (d *CodexDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
