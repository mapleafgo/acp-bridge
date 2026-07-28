package driver

import "context"

// ClaudeDriver manages a claude-agent-acp subprocess.
type ClaudeDriver struct {
	path string   // path to the claude-agent-acp binary
	args []string // additional driver-specific arguments
}

func (d *ClaudeDriver) Type() AgentType { return AgentTypeClaude }

func (d *ClaudeDriver) Start(ctx context.Context) (AgentProcess, error) {
	return startProcess(ctx, d.path, d.args)
}

func (d *ClaudeDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
