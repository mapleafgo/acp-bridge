package driver

import (
	"context"
	"io"
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

func (d *CodexDriver) Start(ctx context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	exe, initialArgs := splitCommand(d.path)
	return startPipes(ctx, exe, append(initialArgs, d.args...))
}

func (d *CodexDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
