# acp-bridge 架构设计

acp-bridge 是一个 stdio MCP 服务器，把 `acp_*` 工具调用转换为 ACP JSON-RPC 请求，并按需启动 codex、claude、gemini 或 opencode agent 子进程。

## 1. 所有权模型

运行时只有一条所有权链：

```text
main
└── instance.Manager
    ├── AgentInstance[codex]
    │   ├── client.Client
    │   │   └── driver.AgentProcess
    │   └── Session[agent session id]
    │       └── Turn
    ├── AgentInstance[claude]
    ├── AgentInstance[gemini]
    └── AgentInstance[opencode]
```

- `internal/mcp.Server` 只负责参数校验、结构化输出映射和工具注册，不保存 Client、Session 或 Turn。
- `instance.Manager` 是实例与 Session 的唯一全局索引。
- 每种 agent 类型同时最多存在一个 `AgentInstance`；首次并发启动通过 ready slot 合并为一次 Client 创建。
- `client.Client` 是 `driver.AgentProcess` 的唯一 owner。进程只有一个 `Wait` goroutine，关闭、超时强杀和退出结果都由该对象统一处理。
- `Session` 和 `Turn` 各自保护可变状态，对外只返回值快照。

## 2. Agent 实例生命周期

Agent 实例按类型惰性启动。首次需要某种 agent 时，Manager 在自身生命周期 context 下创建 Client；后续该类型的 Session 共用同一个 ACP 连接和子进程。

实例不会因 Session 数量降为零而退出。只有以下事件结束实例：

1. agent 子进程或 ACP 连接退出；
2. bridge 进程关闭。

agent 自行退出时，Manager 按实例指针和 generation 校验清理目标，只移除该实例拥有的 Session，不影响其他 agent 类型，也不会让旧退出事件删除后来重建的新实例。

## 3. Session 生命周期

公开 Session ID 直接复用 agent ID，并增加 agent 类型限定：

```text
<agent_type>:<agent_session_id>
```

解析时只切分第一个冒号，因此 `codex:thread:child` 的 agent 原始 ID 是 `thread:child`。

Session 创建后永久保留，不设置 TTL、不做 LRU，也不会在容量满时淘汰。它只在以下事件移除：

1. 用户调用 `acp_close` 或删除活跃 Session；
2. 所属 agent 实例退出；
3. bridge 关闭。

`ACP_BRIDGE_MAX_SESSIONS` 是全局活跃 Session 上限，默认 10：

- `0` 表示不限制；
- 负数为非法配置；
- 达到上限后，new/load/resume/fork 直接返回容量错误；
- 容量判断包含尚未完成的并发 reservation，防止并发越界；
- 拒绝新操作时不会关闭或替换已有 Session。

`acp_sessions()` 不分页，返回当前全部活跃 Session，并按 `last_used` 降序、限定 ID 升序稳定排列。只读查询不刷新 `last_used`。

## 4. Turn 生命周期

同一 Session 同时最多运行一个 Turn：

```text
idle → prompting → permission_pending → prompting → idle
```

Turn 状态为：

- `running`
- `permission_required`
- `completed`
- `interrupted`
- `error`

每个 Turn 有一个 controller goroutine。controller 是 Prompt 完成结果与该 Session 权限事件的唯一消费者；MCP handler 只等待或读取 Turn 快照，不能直接消费 Client channel。

`ACP_BRIDGE_DEFAULT_TIMEOUT` 只限制 MCP handler 的同步等待：

- 超时返回 `running`，后台 Turn 继续执行；
- `acp_progress(session_id)` 查询当前或最近 Turn；
- 可选 `turn_id` 用于精确校验；
- 尚无 Turn 时返回 `idle`；
- completed、interrupted 和 error 快照保留到下一轮 chat。

### 中断

显式 `acp_interrupt(session_id, turn_id)` 与“仍在同步等待时宿主取消 handler context”调用同一个内部中断方法：

1. 原子提交 `interrupted` 终态；
2. 保存中断前通知快照；
3. Session 回到 idle；
4. 取消本地 Prompt context；
5. 使用实例生命周期派生的短超时 context 尽力发送 ACP Cancel。

ACP Cancel 失败只记录 Warn，不回滚本地中断。完成与中断并发时只有第一个终态提交成功。

## 5. 权限路由

权限等待键为 `(agent_session_id, request_id)`，事件 channel 也按 agent Session ID 隔离。这样不同 Session 使用相同 ToolCall ID 时不会串线。

同一 Session 的多个权限请求按到达顺序排队。`acp_progress` 返回队首请求，`acp_respond` 解决后继续暴露下一项。

当前 ACP SDK 的 elicitation 请求没有 Session ID，无法安全关联到 Turn，因此 bridge 明确返回 unsupported，不再伪造空 Session 和固定 request ID。

## 6. MCP 工具合同

- `acp_chat` 首次调用返回限定 Session ID；续聊只需传该 ID。
- `acp_progress` 必须传 `session_id`，`turn_id` 可选。
- `acp_interrupt` 必须同时传 `session_id` 和 `turn_id`。
- `acp_sessions` 无分页参数，返回全部活跃 Session。
- `acp_list_history` 可选 `agent_type`，默认 codex；返回的历史 ID 同样是限定 ID。
- `acp_load_session`、`acp_resume_session` 和 `acp_delete_session` 从限定 ID 推导 agent 类型，不再单独接收 `agent_type`。

所有 handler 使用 MCP SDK 泛型结构化输出。业务错误设置 `CallToolResult.IsError=true`。

## 7. bridge 关闭

`main.run()` 创建 Manager 和 MCP Server。无论 stdio 正常 EOF 还是 Server 返回错误，`runWith` 都在返回前调用 `Manager.Close`：

1. 标记 Manager closing，拒绝新操作；
2. 取消所有运行中 Turn；
3. 关闭每个 Client；
4. Client 关闭 stdin，等待 agent 自行退出；超时后强制终止；
5. 清空实例与 Session 索引。

`main()` 只在 `run()` 完成清理后决定是否 `os.Exit(1)`。

## 8. 配置

| 环境变量 | 默认值 | 说明 |
|---|---:|---|
| `ACP_BRIDGE_DEFAULT_TIMEOUT` | `300s` | chat/respond 同步等待时间 |
| `ACP_BRIDGE_MAX_SESSIONS` | `10` | 全局活跃 Session 上限，0 为无限 |
| `ACP_BRIDGE_CODEX_PATH` | `npx -y @agentclientprotocol/codex-acp` | codex agent 命令 |
| `ACP_BRIDGE_CLAUDE_PATH` | `npx -y @agentclientprotocol/claude-agent-acp` | claude agent 命令 |
| `ACP_BRIDGE_GEMINI_PATH` | `npx -y @agentclientprotocol/gemini-agent-acp` | gemini agent 命令 |
| `ACP_BRIDGE_OPENCODE_PATH` | `npx -y opencode-ai acp` | opencode agent 命令 |

不存在 Session TTL 配置。

## 9. 验证重点

- `go test -race ./...`：Session、Turn、Manager 和权限 channel 的竞态。
- 同类型并发首次请求只能启动一个 Client。
- 第 11 个 Session 被拒绝，前 10 个不变。
- 零 Session 实例保持存活。
- agent 退出只清理自身 Session。
- handler 取消与显式中断均保留 interrupted 快照。
- `acp_sessions()` 返回全部限定 ID。
- bridge 所有退出路径回收子进程。
