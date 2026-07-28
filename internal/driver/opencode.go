package driver

import (
	"context"
	"io"
)

// OpenCodeDriver manages an opencode-acp subprocess.
type OpenCodeDriver struct {
	path string   // the full command string, e.g. "npx -y opencode-ai acp"
	args []string // additional driver-specific arguments
}

func (d *OpenCodeDriver) Type() AgentType { return AgentTypeOpenCode }

func (d *OpenCodeDriver) Start(ctx context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	exe, initialArgs := splitCommand(d.path)
	return startPipes(ctx, exe, append(initialArgs, d.args...))
}

func (d *OpenCodeDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
