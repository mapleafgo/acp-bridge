# acp-bridge

将 ACP 兼容 agent（codex-acp / claude-agent-acp / gemini-agent-acp / opencode）桥接到 stdio **MCP** 的 Go 服务。

MCP 宿主（如 Hermes）启动本二进制后，由 bridge 按需拉起 agent 子进程，把 `acp_*` 工具调用翻译为 ACP JSON-RPC 请求。

## 特性

- 每种 agent **一个常驻实例**，多 Session 共享同一 ACP 连接与子进程
- 公开 Session ID 为 **qualified ID**：`<agent_type>:<agent_session_id>`
- Session **永久保留**（无 TTL / LRU）；满容量时拒绝新建，不淘汰已有会话
- Turn 由独立 controller 驱动；同步等待超时只返回 `running`，不取消后台任务
- 权限事件按 Session + request ID 路由；列表返回 `state` / `turn_id` / `turn_status`，由宿主自行决定下一步工具
- 列表不提供 `next_action`；宿主根据 `state` / `turn_status` 选择下一步
- skill 与工具同源：MCP Resource `acp-bridge://skill` 内嵌 `internal/mcp/skill.md`

## 快速开始

### 依赖

- Go 1.26+
- [Task](https://taskfile.dev)（可选，也可用纯 `go` 命令）
- 目标 agent 可执行文件或 `npx` 可拉取的 ACP 包

### 构建与运行

```bash
task build          # 输出 bin/acp-bridge
task run            # stdio 模式直接跑（调试）
# 或
go build -o bin/acp-bridge ./cmd/acp-bridge
./bin/acp-bridge    # 读 stdin/stdout 作为 MCP 传输
```

宿主侧把本二进制配置为 MCP server（stdio）即可；无需额外 skills 目录。

### 检查

```bash
task check          # fmt + vet + test + build
# 等价
gofmt -w $(find . -name '*.go' -not -path './.git/*')
go vet ./...
go test -race ./...
go build ./...
```

## 配置

全部通过 `ACP_BRIDGE_*` 环境变量控制：

| 变量 | 默认 | 说明 |
|---|---|---|
| `ACP_BRIDGE_CODEX_PATH` | `npx -y @agentclientprotocol/codex-acp` | Codex agent 命令（可含参数，内部会拆分） |
| `ACP_BRIDGE_CODEX_ARGS` | 空 | 追加参数（空格分隔） |
| `ACP_BRIDGE_CLAUDE_PATH` | `npx -y @agentclientprotocol/claude-agent-acp` | Claude agent |
| `ACP_BRIDGE_CLAUDE_ARGS` | 空 | |
| `ACP_BRIDGE_GEMINI_PATH` | `npx -y @agentclientprotocol/gemini-agent-acp` | Gemini agent |
| `ACP_BRIDGE_GEMINI_ARGS` | 空 | |
| `ACP_BRIDGE_OPENCODE_PATH` | `npx -y opencode-ai acp` | OpenCode agent |
| `ACP_BRIDGE_OPENCODE_ARGS` | 空 | |
| `ACP_BRIDGE_DEFAULT_TIMEOUT` | `300s` | `acp_chat` / `acp_respond` 同步等待上限 |
| `ACP_BRIDGE_MAX_SESSIONS` | `10` | 全局活跃 Session 上限；`0` 表示不限制 |
| `ACP_BRIDGE_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `ACP_BRIDGE_LOG_FORMAT` | `text` | `text` 或 `json`（写到 stderr） |

## MCP 工具

| 工具 | 主要参数 | 作用 |
|---|---|---|
| `acp_chat` | `prompt`，`session_id?`，`cwd?`，`agent_type?` | 新建或续聊 |
| `acp_respond` | `session_id`，`request_id`，`outcome` | 权限 allow/deny |
| `acp_progress` | `session_id`，`turn_id?` | 随时只读查询：会话状态、本轮/最近一轮完整内容；无 Turn 返回 `idle` |
| `acp_interrupt` | `session_id`，`turn_id` | 中断 Turn，Session 仍存活 |
| `acp_close` | `session_id` | 关闭活跃 Session |
| `acp_sessions` | — | 列出全部活跃 Session |
| `acp_session_info` | `session_id` | 元数据：mode、config_options、commands 等 |
| `acp_set_mode` | `session_id`，`mode` | 设置权限模式 |
| `acp_set_config` | `session_id`，`config_id`，`value` | 设置配置项 |
| `acp_fork_session` | `session_id` | 从活跃 Session 派生 |
| `acp_load_session` | `session_id`，`cwd?` | 加载历史为活跃 |
| `acp_resume_session` | `session_id`，`cwd?` | 恢复已关闭历史 |
| `acp_list_history` | `agent_type?` | 列持久化历史（默认 codex） |
| `acp_delete_session` | `session_id` | 删历史（活跃须先 close） |

默认：`agent_type=codex`，`cwd=.`。业务失败时 MCP `IsError=true`，错误文案见结构化 `error` 字段。

### Session ID

```text
codex:thread_abc
claude:sess-1
codex:thread:child    # agent 原始 ID 可含冒号；只按第一个 ':' 分割
```

load / resume / delete **不再**单独传 `agent_type`，从 qualified ID 推导。

### 典型宿主流程

```text
acp_chat(prompt)
  ├─ completed / error / interrupted → 可读结果；可同 session 再 chat
  ├─ running → 记下 session_id + turn_id → 轮询 acp_progress
  └─ permission_required → acp_respond → 再 progress / chat
需要停止 → acp_interrupt(session_id, turn_id)
查看能力 → acp_session_info(session_id)
结束 → acp_close(session_id)
历史 → acp_list_history → load / resume；删除前若仍活跃须先 close
```

列表导航不依赖 `next_action` 字段：根据 `state` 与 `turn_status` 选择工具即可。

| `state` / `turn_status` | 常见下一步 |
|---|---|
| `idle`（无 turn 或 turn 已终态） | `acp_chat` |
| `prompting` + `running` | `acp_progress`；要停则 `acp_interrupt` |
| `permission_pending` | `acp_progress` 取权限详情 → `acp_respond` |
| `closing` | 等待关闭完成 |

### Skill Resource

- URI：`acp-bridge://skill`
- MIME：`text/markdown`
- 内容与工具版本绑定，随二进制升级；宿主用标准 `resources/list` / `resources/read` 即可

## 生命周期摘要

**Agent 实例**

- 按类型懒启动；Session 清零不退出实例
- 进程或连接退出 → 清理该类型全部活跃 Session → 下次请求重建新 generation
- 旧实例延迟退出事件不会误删新实例

**Session**

- 创建/load/resume/fork 走容量预留；满额直接错误
- 同 qualified ID 的 load/resume/delete/fork/close/set-mode/set-config 串行
- 活跃 Session 不能 delete，须先 close
- 远端创建/fork 成功但本地注册失败会补偿关闭；load/resume 注册失败同样补偿

**Turn**

- 同 Session 同时仅一个 Turn；不同 Session 可并发
- handler 取消（Turn 已注册）等价于中断并保留 `interrupted` 快照
- ACP Cancel 失败不回滚本地 interrupted

**Bridge 关闭**

- `runWith` 在 Server 返回后始终 `Manager.Close`
- 共享调用方 deadline：中断 Turn → 关 Session → 关 Client → 再取消 Manager context

更细的设计见 [DESIGN.md](DESIGN.md) 与 [docs/superpowers/specs/](docs/superpowers/specs/)。

## 项目结构

```text
cmd/acp-bridge/     入口，stdio MCP + 关闭编排
internal/config/    ACP_BRIDGE_* 配置
internal/driver/    codex/claude/gemini/opencode 进程启动
internal/client/    ACP 连接、权限与 session update 路由
internal/instance/  Manager：实例、Session 索引、Turn controller
internal/session/   Session/Turn 领域模型
internal/mcp/       工具注册、handler、内嵌 skill
```

## 开发

```bash
task test           # go test -race ./...
task test:verbose
task vet
task fmt
task clean
```

约定：

- 测试与源码同目录 `*_test.go`
- handler 使用 SDK 泛型结构化输出，业务错误设 `IsError: true`
- 修改 skill 只改 `internal/mcp/skill.md`，不要另建 `skills/` 目录

## License

见仓库内 LICENSE（若有）。
