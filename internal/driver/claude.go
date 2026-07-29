package driver

import "context"

// ClaudeDriver manages a claude-agent-acp subprocess.
type ClaudeDriver struct {
	path string   // path to the claude-agent-acp binary
	args []string // additional driver-specific arguments
}

// Type 返回该 Driver 管理的 claude agent 类型。
func (d *ClaudeDriver) Type() AgentType { return AgentTypeClaude }

// Start 启动配置的 claude-agent-acp 命令；返回对象独占子进程回收职责。
func (d *ClaudeDriver) Start(ctx context.Context) (AgentProcess, error) {
	exe, initialArgs := splitCommand(d.path)
	return startAgentProcess(ctx, d.Type(), exe, append(initialArgs, d.args...))
}

// Capabilities 返回 claude-agent-acp 当前接入的 ACP 扩展能力集合。
func (d *ClaudeDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
