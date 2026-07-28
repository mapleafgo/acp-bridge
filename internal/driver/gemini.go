package driver

import (
	"context"
	"io"
)

// GeminiDriver manages a gemini-agent-acp subprocess.
type GeminiDriver struct {
	path string   // path to the gemini-agent-acp binary
	args []string // additional driver-specific arguments
}

func (d *GeminiDriver) Type() AgentType { return AgentTypeGemini }

func (d *GeminiDriver) Start(ctx context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	return startPipes(ctx, d.path, d.args)
}

func (d *GeminiDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
