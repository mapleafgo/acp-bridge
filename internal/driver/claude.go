package driver

import (
	"context"
	"io"
)

// ClaudeDriver manages a claude-agent-acp subprocess.
type ClaudeDriver struct {
	path string   // path to the claude-agent-acp binary
	args []string // additional driver-specific arguments
}

func (d *ClaudeDriver) Type() AgentType { return AgentTypeClaude }

func (d *ClaudeDriver) Start(ctx context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	return startPipes(ctx, d.path, d.args)
}

func (d *ClaudeDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
