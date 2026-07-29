package driver

import "context"

// GeminiDriver manages a gemini-agent-acp subprocess.
type GeminiDriver struct {
	path string   // path to the gemini-agent-acp binary
	args []string // additional driver-specific arguments
}

// Type 返回该 Driver 管理的 gemini agent 类型。
func (d *GeminiDriver) Type() AgentType { return AgentTypeGemini }

// Start 启动配置的 gemini-agent-acp 命令；返回对象独占子进程回收职责。
func (d *GeminiDriver) Start(ctx context.Context) (AgentProcess, error) {
	exe, initialArgs := splitCommand(d.path)
	return startAgentProcess(ctx, d.Type(), exe, append(initialArgs, d.args...))
}

// Capabilities 返回 gemini-agent-acp 当前接入的 ACP 扩展能力集合。
func (d *GeminiDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
