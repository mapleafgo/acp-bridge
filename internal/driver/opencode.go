package driver

import "context"

// OpenCodeDriver manages an opencode-acp subprocess.
type OpenCodeDriver struct {
	path string   // the full command string, e.g. "npx -y opencode-ai acp"
	args []string // additional driver-specific arguments
}

// Type 返回该 Driver 管理的 opencode agent 类型。
func (d *OpenCodeDriver) Type() AgentType { return AgentTypeOpenCode }

// Start 启动配置的 opencode ACP 命令；返回对象独占子进程回收职责。
func (d *OpenCodeDriver) Start(ctx context.Context) (AgentProcess, error) {
	exe, initialArgs := splitCommand(d.path)
	return startAgentProcess(ctx, d.Type(), exe, append(initialArgs, d.args...))
}

// Capabilities 返回 opencode 当前接入的 ACP 扩展能力集合。
func (d *OpenCodeDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
