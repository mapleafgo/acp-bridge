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

// Type 返回该 Driver 管理的 codex agent 类型。
func (d *CodexDriver) Type() AgentType { return AgentTypeCodex }

// Start 启动配置的 codex-acp 命令；返回对象独占子进程回收职责。
func (d *CodexDriver) Start(ctx context.Context) (AgentProcess, error) {
	exe, initialArgs := splitCommand(d.path)
	return startAgentProcess(ctx, d.Type(), exe, append(initialArgs, d.args...))
}

// Capabilities 返回 codex-acp 当前接入的 ACP 扩展能力集合。
func (d *CodexDriver) Capabilities() AgentCapabilities {
	return defaultCapabilities()
}
