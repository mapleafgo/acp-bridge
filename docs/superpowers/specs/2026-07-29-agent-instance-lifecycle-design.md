# Agent 实例与 Session 生命周期设计

## 目标

重构 acp-bridge 的运行时所有权，使一种 agent 类型对应一个常驻实例，并由该实例完整管理所属 ACP Client、子进程、Session 和 Turn。

本设计确立以下规则：

- 每种 `agent_type` 在一个 acp-bridge 进程内最多运行一个 agent 实例；
- 同一实例可以承载多个 Session；
- Session 不因空闲时间或容量压力被自动淘汰；
- Session 只在用户显式关闭、所属 agent 实例退出或 bridge 退出时结束；
- agent 实例没有 Session 时仍保持运行；
- agent 实例退出后，其全部 Session 立即失效，后续请求按需启动新实例；
- bridge 退出时统一关闭全部 Session、Client 和子进程；
- 默认最多保留 10 个活跃 Session，达到上限时拒绝新建，不影响已有 Session；
- `acp_sessions` 不分页，一次返回全部活跃 Session。

## 与现有合同的关系

本设计是 Agent 实例、Session、Turn 和子进程生命周期的主设计。`2026-07-29-hermes-session-turn-contract-design.md` 定义 Hermes 侧调用合同；`2026-07-29-acp-progress-session-status-design.md` 只扩展 `acp_progress` 的功能面，不定义运行时所有权、Session ID 生成方式、容量或淘汰策略。发生交叉时以本设计为准。

本设计保留两份功能合同已经确定的以下行为：

- `title` 只来自 agent 上报的 `SessionInfoUpdate.Title`；
- `turn_id` 标识当前保留的 Turn；
- 每个 Session 只保留一个当前 Turn；
- `completed` 和 `interrupted` 快照保留到下一轮 `acp_chat`；
- `acp_progress` 必须接收 `session_id`，可以额外接收 `turn_id` 做精确校验；
- Session 尚无 Turn 时，`acp_progress(session_id)` 返回统一的 Session 状态 `idle`；
- `acp_interrupt` 必须接收 `session_id + turn_id`；
- `acp_respond` 使用 `session_id + request_id + outcome`；
- `acp_interrupt` 只中断 Turn，不关闭 Session。

本设计替换其中以下旧约定：

- bridge 不再生成独立的 `s-*` Session ID；
- Session 不再由带 LRU 和 TTL 的 `SessionPool` 管理；
- Turn 不再由 MCP Server 的独立 map 管理；
- Client 不再由 MCP Server 按类型单独缓存；
- TTL 和 LRU 不再构成 Turn 终止或 Session 删除条件。

实现完成后，既有 Hermes Session/Turn 合同、`DESIGN.md` 和内嵌 skill 必须同步更新，不能并存相互矛盾的 Session ID 或生命周期描述。

## 当前代码事实基线

本文描述的是重构目标，不是当前实现。方案以本次修订时的源码和以下直接依赖为基线：

- `github.com/coder/acp-go-sdk v0.13.5`；
- `github.com/modelcontextprotocol/go-sdk v1.7.0`；
- `github.com/caarlos0/env/v11 v11.4.1`。

当前运行时由三套并列状态组成：

| 当前代码 | 实际职责 | 与目标的差距 |
|---|---|---|
| `internal/mcp.Server.clients` | 按 `agent_type` 缓存一个 `acpClient` | 已具备“每类型一个 Client”的雏形，但没有实例状态、退出清理和安全重建 |
| `internal/mcp.Server.turns` | 按 bridge Session ID 保存 `promptTurn` | Turn 脱离 Session，由 MCP 层独立管理 |
| `internal/session.Pool` | 保存 Session、维护 LRU、执行 TTL 清理 | 满容量时自动淘汰，和永久保持目标相反 |
| `internal/client.Client` | 保存 ACP connection、handler 和 stdio pipe | 没有保存 `exec.Cmd`，不能等待、回收或强制终止子进程 |
| `internal/client.acpClientHandler` | 汇总通知并用全局 channel 传递权限 | 同一 Client 下多个 Session 会竞争同一个权限 channel |

当前关键调用流如下：

1. `cmd/acp-bridge/main.go` 创建 `session.Pool`，再把它传给 `mcp.NewServer`；
2. `Server.getOrCreateClient` 在 `Server.mu` 内启动并初始化 agent，同时该锁还保护 `turns`；
3. `client.New` 把首次 MCP handler context 传给 `driver.Start`，而 driver 使用 `exec.CommandContext`；
4. `acp_chat` 创建 ACP Session 后再生成一个 `s-*` bridge ID，分别保存 `ID` 与 `ACPSessionID`；
5. Prompt 使用 `context.Background()` 派生的本地 cancel，Turn 存入 `Server.turns`；
6. `Pool.Get` 对包括查询在内的每次读取都更新 `LastUsed` 和 LRU；
7. `Pool.Add` 达到上限时直接淘汰最旧 Session，TTL goroutine 也会删除 Session，但两条路径都不会完成 ACP CloseSession、Turn 索引和 Client 生命周期清理；
8. `main` 在 `Server.Run` 返回错误时直接 `os.Exit(1)`，正常和异常退出路径都没有统一关闭 Pool、Client 和子进程。

现有 `acp_sessions` 已经是不分页的本地活跃 Session 列表，现有 `promptTurn` 也已经保留终态快照。这两部分行为继续保留，但所有权和 ID 改为本设计定义的结构。

## 核心术语

### AgentInstanceManager

bridge 内全部 agent 实例的唯一注册表和生命周期入口。

### AgentInstance

一种 agent 类型在当前 bridge 进程内的唯一运行实例。它是生命周期聚合根，直接拥有 ACP Client 和 Session，并通过 Session 拥有 Turn。ACP Client 是 AgentProcess 的唯一 owner。

### Session

agent 内的独立对话上下文。Session 归属于一个 AgentInstance。

### Turn

一次 `acp_chat` Prompt 执行。Turn 归属于一个 Session，同一 Session 同时最多存在一个当前 Turn。

### AgentProcess

对 agent 子进程及其 stdio、退出通知和进程回收能力的封装。

## 总体架构

```text
MCP Server
    │
    │ 参数转换与结构化输出
    ▼
AgentInstanceManager
    ├── instances[agentType]
    ├── sessionIndex[qualifiedSessionID]
    ├── pendingReservations
    └── manager lifecycle context
            │
            ▼
        AgentInstance
        ├── agentType
        ├── ACP Client
        │   └── AgentProcess
        ├── sessions[agentSessionID]
        ├── instance state
        └── instance lifecycle context
                │
                ▼
            Session
            ├── metadata
            ├── session state
            └── current Turn
```

所有权方向固定为：

```text
Manager → Instance → Client → Process
                   └→ Session → Turn
```

MCP Server 不再持有 `clients`、`turns` 或 `SessionPool`，只调用 Manager 提供的运行时操作。

## 包与组件职责

### `internal/instance`

新增实例管理包，接管当前 `mcp.Server.clients`、`mcp.Server.turns` 和 `session.Pool` 的编排职责：

- `manager.go`：实例注册、全局 Session 索引、容量预留和 bridge 关闭；
- `instance.go`：单个 agent 实例的状态、Client、Session 集合和退出处理；
- `factory.go`：承接当前 `mcp.clientFactory` 测试缝，通过 Driver 和 Client 创建实例。

Manager 的全局索引使用 qualified Session ID；Instance 已知自身 `agent_type`，内部 Session map 只使用 agent 原始 Session ID，避免保存两份等价键。

Manager 对 MCP 层暴露具体方法而不是可变对象：创建类操作为 new/load/resume/fork，Turn 操作为 chat/respond/progress/interrupt，生命周期操作为 close/delete，查询与配置操作为 sessions/history/info/set-mode/set-config。所有方法返回值类型的 view 或业务错误，不返回 `*AgentInstance`、`*Session`、`*Turn` 或 `*client.Client`，避免 handler 重新组合当前实现中的跨表状态。

### `internal/session`

保留为领域对象包，但把当前集中在 `session.go` 的 Pool 与数据对象拆开：

- `id.go`：qualified Session ID 的构造与解析；
- `session.go`：Session 状态、元数据、当前 Turn 和并发安全操作；
- `turn.go`：从 `internal/mcp.promptTurn` 迁入的 Turn 状态、取消函数和终态快照；
- `updates.go`：从 `internal/mcp/collector.go` 迁入的 ACP SessionUpdate 聚合。

删除 `Pool`、`container/list`、`WithCleanupInterval`、`cleanupLoop`、`evictIdle` 和 `evictOldestLocked`。MCP 输出类型仍留在 `internal/mcp`；Session/Turn 只返回不依赖 MCP SDK 的只读 view，避免领域对象依赖 `sdk.CallToolResult`。

当前 `Pool.Get` 返回可变 `*Session`，handler 随后在 Pool 锁外读取 `State`、`Title`、`ConfigOpts` 等字段。目标 Session 字段改为私有，只能通过并发安全方法转换为 view。当前 `Session.Cancel` 下沉到 Turn；`StatusPaused` 和持久化的 `StatusClosed` 删除，`closing` 纳入 Session state，列表中的 `status: active` 由“仍在 Manager 索引中”派生。

### `internal/client`

继续负责 ACP 协议连接，但需要：

- 在现有 `conn`、`handler`、`stdin`、`stdout` 基础上持有可管理的 AgentProcess；
- 暴露连接或进程退出通知；
- 支持幂等关闭；
- 把现有全局 `permissionSignal` 和仅按 request ID 建键的 `permissionCh` 改为按 Session 路由；
- 关闭时解除全部权限等待。

Client 是 AgentProcess 的唯一 owner。AgentInstance 只持有 Client，并通过 `Client.Done()` 观察 ACP 连接或进程退出。

### `internal/driver`

当前 `AgentDriver.Start` 返回三根 pipe，`startPipes` 在 `cmd.Start()` 后丢失 `*exec.Cmd`。重构后 `Start` 返回 AgentProcess。AgentProcess 至少提供：

- stdin、stdout、stderr；
- `Done()` 退出通知；
- 幂等 `Close(ctx)`；
- 优雅退出超时后的强制终止；
- 单一 Wait goroutine，保证子进程只被等待和回收一次；
- `Close(ctx)` 等待该 goroutine，不得再次调用底层 `cmd.Wait()`。

### `internal/mcp`

保留工具注册、参数类型、结果类型和 domain view 到结构化输出的转换。删除 `Server.pool`、`Server.clients`、`Server.turns`、`getOrCreateClient`、`storeTurn`、`peekTurn` 和 `popTurn`；Server 只持有 Manager，所有 Session、Turn 和 Client 操作统一委托给 Manager。

## Session ID

### 公开格式

不再生成 bridge `s-*` ID。公开 Session ID 使用：

```text
<agent_type>:<agent_session_id>
```

示例：

```text
codex:abc123
claude:abc123
gemini:xyz789
```

ACP 只保证 Session ID 是 agent 返回的唯一标识，没有保证不同 agent 实现之间不会冲突，因此必须带 agent 类型命名空间。

### 解析规则

- 只按第一个 `:` 分割；
- 前半部分必须是受支持的 agent 类型；
- 后半部分是原始 agent Session ID，不能为空；
- 原始 ID 可以继续包含 `:`；
- 对 agent 发起 ACP 请求时只传原始 ID；
- 对 MCP 宿主返回和接收时只使用 qualified ID。

Session 保存：

```go
type Session struct {
    ID             ID
    AgentType      driver.AgentType
    AgentSessionID string
}
```

### 工具一致性

以下工具统一返回或接收 qualified Session ID：

- `acp_chat`
- `acp_progress`
- `acp_interrupt`
- `acp_respond`
- `acp_close`
- `acp_sessions`
- `acp_session_info`
- `acp_set_mode`
- `acp_set_config`
- `acp_fork_session`
- `acp_list_history`
- `acp_load_session`
- `acp_resume_session`
- `acp_delete_session`

`acp_list_history` 根据请求中的 `agent_type` 查询目标实例并把 agent 返回的原始 ID 转为 qualified ID。load、resume 和 delete 从 qualified ID 推导 agent 类型，不再需要重复传递 `agent_type`。

已经注册为活跃 Session 的历史 ID 不能重复 load 或 resume。活跃 Session 不能直接 delete，必须先 `acp_close`。

### 从当前工具参数迁移

| 工具 | 当前代码 | 目标 |
|---|---|---|
| `acp_chat` 新建 | agent 返回原始 ID，bridge 再生成 `s-*` | 直接注册并返回 qualified agent Session ID |
| 活跃 Session 工具 | 接收 `s-*`，再查 `ACPSessionID` | 接收 qualified ID，解析后按 Instance 和原始 ID 路由 |
| `acp_sessions` | 返回字段 `id`，值为 `s-*` | 返回字段 `session_id`，值为 qualified ID |
| `acp_list_history` | 参数只有未实际使用的 `cwd`，固定查询 Codex，返回原始 ID | 删除无效 `cwd`，增加可选 `agent_type`，默认 Codex，返回 qualified ID |
| `acp_load_session` | 原始 ID、`agent_type` 和可选 `cwd` 分开传入 | qualified ID 取代前两项，保留可选 `cwd`，注册同一个 qualified ID |
| `acp_resume_session` | 先查活跃 bridge Session，再为同一 ACP ID 生成新 `s-*` | 接收未活跃的 qualified 历史 ID，恢复后注册原 ID |
| `acp_delete_session` | 原始 ID 与 `agent_type` 分开传入 | 只传 qualified ID；活跃时拒绝 |
| `acp_fork_session` | fork 后生成新的 `s-*` | 使用 agent 返回的新原始 ID 构造 qualified ID |

## AgentInstance 生命周期

状态机：

```text
不存在
  │ 首次请求
  ▼
starting ──成功──▶ running
   │                 │
   │ 失败            ├──进程或连接退出──▶ dead
   ▼                 │
不存在               └──bridge 退出──▶ closing ─▶ closed
```

### 懒启动

- 第一次需要某个 `agent_type` 时创建 AgentInstance；
- 进程 context 派生自 Manager 生命周期，不绑定首次 MCP handler context；
- 同类型并发首次请求共享一次启动结果；
- 启动和 ACP initialize 期间不持有 Manager 全局锁；
- 启动失败不注册 Instance，等待中的请求收到相同失败结果；
- Instance 启动成功后，即使没有 Session 也保持运行。

当前 `getOrCreateClient` 已保证一个 `agent_type` 只缓存一个 Client，但它在 `Server.mu` 内执行 `client.New`，并把调用者 context 传给 `exec.CommandContext`。Manager 必须用 `starting` 占位和完成 channel 复用同一次启动结果，在锁外使用 Manager 生命周期 context 创建 Client；不能继续沿用当前函数实现。

### 退出检测

Manager 监听 `Client.Done()`。Client 合并 ACP 连接和 AgentProcess 的退出通知；任一底层退出都可使 Instance 进入 `dead`，但通知和清理必须幂等。

当前 `Client.Done()` 只返回 `ClientSideConnection.Done()`，当前 `Client.Close()` 也只关闭 pipe。重构后的 `Client.Done()` 必须合并 connection Done 与 AgentProcess Done；`Client.Close(ctx)` 必须关闭 handler、关闭连接和 pipe，并等待 AgentProcess 的唯一 Wait goroutine 完成。

退出回调携带具体 Instance 指针或 generation。Manager 只有在实例表中仍是同一对象时才执行删除，防止旧实例的延迟退出事件误删新实例。

### 崩溃清理

实例异常退出时：

1. 原子标记为 `dead`；
2. 从 Manager 实例表移除；
3. 取消该实例下全部 Turn；
4. 删除该实例下全部 Session；
5. 删除对应全局 Session 索引；
6. 释放对应 Session 配额；
7. 关闭剩余 stdio 并完成进程回收；
8. 后续旧 Session ID 返回 `session not found`；
9. 下一次需要该 agent 类型时创建全新实例。

不自动恢复旧 Session，也不自动调用 load 或 resume。

## Session 生命周期

状态机：

```text
idle
  ├──acp_chat──▶ prompting
  │                ├──权限请求──▶ permission_pending
  │                │                └──acp_respond──▶ prompting
  │                ├──完成或失败──▶ idle
  │                └──中断──▶ idle
  └──acp_close──▶ closing ──成功──▶ 从 Manager 删除
```

Session 不存在 paused 状态。关闭成功的 Session 直接从活跃注册表移除，不保留 closed 对象。

### 创建与容量预留

以下操作都可能注册新的活跃 Session，必须走统一容量预留：

- new；
- load；
- resume；
- fork。

流程：

1. 检查 Manager 是否正在关闭；
2. 在锁内检查 `active sessions + pending reservations`；
3. 获得一个预留名额；
4. 在锁外调用 agent；
5. 构造 qualified ID；
6. 原子注册到 Instance 和全局索引；
7. 把预留名额转为正式 Session；
8. 任一步失败都释放预留；
9. agent 已创建 Session 但本地注册失败时，尽力调用 ACP CloseSession 回滚；
10. agent 返回已经活跃的原始 Session ID 属于协议违约，为避免误关已有 Session，直接把该 Instance 标记为不可用并走实例退出清理。

这样并发创建不会突破上限，也不会因先调用 agent 再检查上限而产生孤儿 Session。

实例启动与 Session 创建事务使用 Manager/Instance 生命周期派生的 context，不直接绑定 MCP handler context。如果新 Session 已创建并注册、但首个 Turn 尚未建立时 handler 被取消，Session 保持 `idle` 并出现在 `acp_sessions`，不自动关闭。

以 Turn 成功注册到 `Session.currentTurn` 为取消语义分界：注册前的 handler 取消不删除已创建 Session；注册后的 handler 取消按下文“等待中的请求被宿主取消”执行中断。

### 用户关闭

`acp_close`：

1. 校验 qualified Session ID；
2. 原子把 Session 标记为 `closing`；
3. 取消当前 Turn；
4. 调用 agent 的 ACP CloseSession；
5. 成功后从 Instance 和全局索引删除 Session；
6. 释放一个 Session 配额；
7. 即使这是该实例最后一个 Session，Instance 仍保持运行。

如果 CloseSession 失败且 Instance 仍存活，Session 回到 `idle` 并保留，用户可以重试。其已取消 Turn 保留为 `interrupted` 快照。

如果关闭失败同时伴随实例退出，由实例崩溃流程统一删除。

## Turn 生命周期与中断

Turn 状态包括：

```text
running
permission_required
completed
interrupted
error
```

同一 Session 同时只允许一个当前 Turn。新一轮 `acp_chat` 只有在 Session 为 `idle` 时才能开始，并替换上一轮保留的终态快照。

### 等待超时

`ACP_BRIDGE_DEFAULT_TIMEOUT` 只限制 `acp_chat` handler 同步等待时间：

- 到期后返回 `running`；
- 不取消 Turn；
- Session 保持 `prompting`；
- 后续使用 `acp_progress` 查询。

当前 `runPromptTurn` 已让 Prompt goroutine 脱离 handler 等待超时，但 Prompt 完成、权限事件和状态转换仍由每次进入 `waitForTurn` 或 `acp_progress` 的 MCP handler 竞争处理。目标实现为每个 Turn 启动一个 controller goroutine，它是 Prompt 完成和该 Session 权限事件的唯一消费者，并负责写入 Turn/Session 状态。MCP handler 只等待或读取 Turn 快照，不直接消费 Client channel。

### 状态查询

`acp_progress` 支持两种查询：

- `acp_progress(session_id)` 查询当前或最近一个 Turn；Session 尚无 Turn 时返回 `idle`；
- `acp_progress(session_id, turn_id)` 在查询状态前额外校验 Turn ID，不匹配时返回 `turn mismatch`。

查询本身不改变 Session 或 Turn 状态，也不删除终态快照。`idle` 不返回 `turn_id`，其他正常状态返回实际 `turn_id`。

### 显式中断

`acp_interrupt(session_id, turn_id)`：

1. 校验 qualified Session ID；
2. 校验当前保留的 Turn ID；
3. 只有 running 或 permission_required Turn 可以中断；
4. 在 `Session → Turn` 锁内复制当前进度，提交唯一的 `interrupted` 终态快照，把 Session 切回 `idle`，并解除对应权限等待；
5. 释放锁后立即取消本地 Prompt context；
6. 使用 Instance 生命周期派生的短超时 context 尽力发送 ACP Cancel；
7. 返回已经提交的 `interrupted` 快照；
8. 快照保留到下一轮 `acp_chat`。

ACP Cancel 发送失败不能阻止本地中断。bridge 记录 Warn 日志并仍返回 `interrupted`，避免用户已经取消但后台 Turn 因通知失败继续占用本地状态。

### 等待中的请求被宿主取消

如果 `acp_chat` 仍在同步等待，MCP handler context 被宿主取消，等价于中断当前 Turn。已取消的 handler context 只表示触发来源，不能用于发送 ACP Cancel。

handler 已持有准确的 Session 和 Turn，不通过可变的“当前 Turn”重新定位，避免取消事件误伤后续新 Turn。该入口和 `acp_interrupt` 调用同一个内部中断方法。

如果 `acp_chat` 已经返回 `running`，原请求已结束，后续只能通过 `acp_interrupt(session_id, turn_id)` 中断。

当前 `waitForTurn` 的 `ctx.Done()` 分支会直接 `popTurn`、本地 `cancel()` 并返回 `prompt cancelled`，既没有发送 ACP Cancel，也丢失了可查询快照。该分支必须删除，改为调用 Instance 的统一中断方法。

### 完成与中断竞态

Turn 终态只能写入一次：

- 完成先获得 Turn 锁时写入 `completed` 或 `error`，随后到达的中断返回 `turn is not interruptible`；
- 中断先获得 Turn 锁时写入 `interrupted`，随后到达的 Prompt 完成事件直接丢弃；
- 任何后到事件都不得覆盖已有终态；
- 对已终止 Turn 再次中断返回 `turn is not interruptible`。

### 生命周期销毁

用户关闭 Session、agent 实例退出或 bridge 退出时可以在内部取消 Turn。这些操作销毁了 Turn 所属资源，不是额外的用户中断入口。

## 权限请求路由

同一 AgentInstance 可以并发运行不同 Session 的 Turn，因此权限请求不能继续通过一个所有 Turn 竞争消费的全局 channel 传递。

当前 `acpClientHandler.permissionSignal` 是每个 Client 一个共享 channel，`permissionCh` 只按 `ToolCallId` 建键；`waitForTurn` 和 `acp_progress` 都可能从共享 channel 取走其他 Session 的请求。目标实现必须同时按 agent 原始 Session ID 和 request ID 路由权限事件：

```text
permission key = (agent_session_id, request_id)
PermissionEvents(agentSessionID) <-chan PermissionEvent
RespondPermission(agentSessionID, requestID, response)
```

`PermissionEvent` 携带 agent Session ID、request ID 和原始 ACP request。request ID 使用 `ToolCall.ToolCallId`；空 ID 视为无效 ACP 请求，不再回退成 `session:<id>`。Client 用复合键 map 保存等待者，并为每个 Session 维护有序事件队列；Turn controller 是该 Session 队列的唯一消费者，事件不得因 channel 暂满而静默丢弃。

同一 Turn 同时出现多个权限请求时按到达顺序逐个暴露：`acp_progress` 返回当前队首请求，`acp_respond` 完成后继续返回下一项；不同 request ID 的等待者不得互相覆盖。

这样确保：

- Codex Session A 的权限请求只被 Session A 消费；
- Session B 不会抢走 Session A 的请求；
- 两个 Session 使用相同 request ID 时不会覆盖；
- Session 关闭时对应等待者被解除；
- Instance 关闭时全部等待者被解除。

当前依赖的 `acp-go-sdk v0.13.5` 中，`UnstableCreateElicitationRequest` 只有 `Form` 和 `Url`，没有 Session ID。现有 handler 把它伪装成空 Session、固定 request ID 为 `"elicitation"` 的权限请求，会在并发时覆盖等待者，也无法由 `acp_progress(session_id)` 稳定找回。

本次重构不设计实例级 elicitation 队列。`UnstableCreateElicitation` 直接返回明确的 ACP not-supported 错误，删除空 Session 和固定 `"elicitation"` 的兼容路径。以后只有 SDK 提供可路由的 Session ID，或另行设计实例级 MCP 交互合同后，才重新开放。

## 容量策略

配置：

```text
ACP_BRIDGE_MAX_SESSIONS=10
```

语义：

- 默认值为 10；
- 正数表示全局活跃 Session 硬上限；
- `0` 表示无限制；
- 负数是启动配置错误；
- 达到上限只拒绝新的 new、load、resume 或 fork；
- 不关闭、不淘汰、不改变已有 Session；
- 不再提供 `ACP_BRIDGE_SESSION_TTL`；
- 不再运行空闲清理 goroutine；
- 不再维护 LRU 链表。

当前 `Pool.Add` 在 `len >= MaxSessions` 时调用 `evictOldestLocked` 后继续新增；被淘汰 Session 只执行本地 `Cancel`，不会调用 ACP CloseSession，也不会删除 `Server.turns`。重构时不复用该容量分支：Manager 必须在任何 ACP new/load/resume/fork 之前完成容量预留，满额直接返回错误。

错误结果使用 MCP `IsError: true`，错误文本为：

```text
session limit reached: active=10 limit=10
```

## `acp_sessions`

### 输入

工具不接收参数：

```text
acp_sessions()
```

不提供分页、limit、cursor 或过滤条件。

### 范围

一次返回当前 bridge 进程中全部活跃 Session：

- 用户关闭后立即消失；
- agent 实例退出后，该实例全部 Session 立即消失；
- agent 持久化历史不包含在内；
- `MaxSessions` 只限制创建，不裁剪列表；
- 配置上限大于 10 或配置为无限时，列表仍返回全部 Session。

### 排序

默认按 `last_used_at` 倒序排列，时间相同时按 qualified `session_id` 升序排列。调用列表本身不更新 `last_used_at`。

`last_used_at` 只在创建 Session、开始或终止 Turn、响应权限、设置 mode/config 时更新。`acp_sessions`、`acp_progress` 和 `acp_session_info` 等只读查询不改变活动时间。

### 返回

```json
{
  "status": "ok",
  "sessions": [
    {
      "session_id": "codex:abc123",
      "agent_type": "codex",
      "state": "idle",
      "status": "active",
      "title": "重构用户认证模块",
      "cwd": "/home/user/project",
      "turn_count": 5,
      "idle_seconds": 120,
      "current_mode": "default",
      "turn_id": "t-123",
      "turn_status": "interrupted",
      "next_action": "acp_chat"
    }
  ]
}
```

现有列表结果字段 `id` 改为 `session_id`，与所有后续工具输入保持一致。

`title` 只使用 agent 上报的非空标题。bridge 不根据 Prompt 生成标题。

当前 `Pool.List` 已一次返回全部元素，但 map 遍历无稳定顺序，且 `Pool.Get` 会让只读查询刷新 `LastUsed`。目标保留无分页行为，改为从 Manager 快照生成结果并稳定排序；列表和其他只读查询不再隐式 Touch。

`next_action`：

| Session 状态 | Turn 状态 | next_action |
|---|---|---|
| idle | 无、completed、interrupted 或 error | `acp_chat` |
| prompting | running | `acp_progress` |
| permission_pending | permission_required | `acp_progress` |
| closing | 任意 | `none` |

存在当前保留 Turn 时返回 `turn_id` 和 `turn_status`。`acp_sessions` 不返回完整 permission 和 request ID；宿主先调用 `acp_progress(session_id)` 取得权限详情，再调用 `acp_respond`。这样列表只承担发现和导航职责。

## Bridge 关闭

bridge 是全部 agent 实例的唯一生命周期 owner。

当前入口没有调用 `Pool.Shutdown`，Server 也没有遍历 `clients` 调用 `Close`；错误路径中的 `os.Exit(1)` 会跳过 defer。下述关闭顺序必须由 `cmd/acp-bridge` 的可返回 `run()` 和 `Manager.Close(ctx)` 统一承载。

MCP stdio 结束或 Server.Run 返回后：

1. Manager 进入 closing，拒绝新实例和新 Session；
2. 取消全部运行中的 Turn；
3. 在统一截止时间内尽力关闭各实例的 ACP Session；
4. 关闭 ACP Client；
5. 请求 agent 子进程优雅退出；
6. 超时未退出则强制终止；
7. Wait 回收全部子进程；
8. 清空实例表和 Session 索引。

关闭多个实例可以并发执行，但必须受同一个总截止时间约束。

程序入口不能在资源清理前直接 `os.Exit`。应通过可返回退出码的 `run()` 或等价结构，保证 Manager.Close 在退出前执行。

## 并发与锁

### 锁职责

- Manager 锁：实例表、全局 Session 索引、容量预留和 Manager 状态；
- Instance 锁：实例状态和所属 Session 集合；
- Session 锁：Session 状态、元数据和当前 Turn；
- Turn 锁：权限请求、运行结果和唯一终态。

### 规则

- 不在 Manager 或 Instance 锁内启动进程；
- 不在全局锁内执行 ACP 网络或 stdio 请求；
- 不在全局锁内等待子进程退出；
- 固定加锁顺序为 `Manager → Instance → Session → Turn`；
- 持有子对象锁时不得反向调用父对象；
- 回调 Manager 前必须先释放 Instance、Session 或 Turn 锁；
- MCP handler 不直接组合可变 Session 和 Client 指针，状态转换通过 Manager 或 Instance 方法完成；
- ACP 调用返回后必须重新校验 Instance generation 和 Session state；
- 删除索引和释放容量必须原子；
- 关闭与崩溃清理必须幂等；
- 完成、中断、关闭和崩溃并发时只能产生一个最终清理结果；
- Manager、Instance 和 Session 的测试必须在 race detector 下通过。

## 错误语义

| 场景 | 错误 |
|---|---|
| qualified ID 格式错误 | `invalid session_id` |
| Session 不在活跃索引 | `session not found` |
| Session 正在关闭 | `session is closing` |
| Session 已有运行中 Turn | `session busy (prompt in progress)` |
| 达到容量上限 | `session limit reached: active=N limit=N` |
| load 或 resume 已活跃 Session | `session already active` |
| delete 活跃 Session | `session is active; close it before deleting` |
| `acp_interrupt` 缺少 turn_id | `turn_id is required` |
| Turn 不存在 | `turn not found` |
| Turn ID 不匹配 | `turn mismatch` |
| Turn 已终止 | `turn is not interruptible` |
| agent 实例启动失败 | `failed to start <type> agent: ...` |

上述来自 MCP 工具的业务错误均通过 MCP `IsError: true` 和具体结构化输出返回。

`UnstableCreateElicitation` 是 agent 发起的 ACP 请求，不经过 MCP 工具 handler。它直接向 agent 返回 ACP not-supported 错误，不构造 `sdk.CallToolResult`。

## 测试要求

### 实例生命周期

1. 同类型并发首次请求只创建一个 AgentInstance；
2. 不同类型各自创建一个实例；
3. 最后一个 Session 关闭后实例继续运行；
4. agent 退出后清理该实例全部 Session 和 Turn；
5. 一个实例退出不影响其他类型实例；
6. agent 退出后的下一次请求创建新实例；
7. 旧实例延迟退出事件不能删除新实例；
8. bridge 退出时全部 Client 和子进程被关闭并回收；
9. ACP connection 退出和子进程退出都会触发同一套幂等清理；
10. AgentProcess 只有一个 Wait goroutine，Close 不会二次调用 `cmd.Wait()`。

### Session 与容量

1. 默认允许 10 个活跃 Session；
2. 第 11 个 Session 被拒绝且前 10 个保持有效；
3. 容量检查发生在 ACP session/new 之前；
4. 并发创建通过预留机制不突破上限；
5. `MaxSessions=0` 时允许超过 10 个；
6. 配置为 20 时创建 12 个，`acp_sessions` 返回全部 12 个；
7. 不存在 TTL、LRU 和后台清理 goroutine；
8. Session 只因 close、实例退出或 bridge 退出被删除。

### ID 与路由

1. agent 原始 ID 正确转换为 qualified ID；
2. 原始 ID 包含 `:` 时仍能正确解析；
3. Codex 和 Claude 返回相同原始 ID 时不冲突；
4. 所有活跃 Session 工具按 qualified ID 路由到正确实例；
5. 历史列表返回 qualified ID；
6. load、resume、delete 从 qualified ID 推导 agent 类型；
7. 不再生成或接受 bridge `s-*` ID；
8. Session 创建完成但首个 Turn 尚未建立时取消 handler，Session 保持 idle 并可从列表找回。

### Turn 与中断

1. 每个 Session 同时只允许一个 Turn；
2. 不同 Session 可以并发 Prompt；
3. 等待超时返回 running 且不取消 Turn；
4. `acp_progress(session_id)` 在无 Turn 时返回 `idle`；
5. `acp_progress(session_id)` 可查询当前或最近 Turn；
6. `acp_progress(session_id, turn_id)` 可以精确校验 Turn；
7. 正确的 `session_id + turn_id` 可以中断；
8. 错误 turn_id 不影响当前 Turn；
9. 等待中的 MCP handler context 取消等价于中断；
10. handler context 取消保留 interrupted 快照；
11. 完成与中断竞态只产生一个终态；
12. interrupted 快照保留到下一轮；
13. 新一轮替换旧 Turn；
14. 权限 signal 和 response 都按 Session 与 request ID 路由；
15. 两个 Session 使用相同 request ID 时不冲突；
16. 权限事件在 MCP handler 没有等待时仍保留，不因 channel 暂满而丢失；
17. 同一 Session 的多个权限请求按到达顺序逐个响应且不覆盖；
18. Turn controller 是 Prompt 完成与权限事件的唯一消费者；
19. `UnstableCreateElicitation` 返回明确的不支持错误，且不创建空 Session 或 `"elicitation"` pending key。

### Session 列表

1. 无分页参数；
2. 返回全部活跃 Session；
3. 返回字段名 `session_id`；
4. 返回 agent 类型、状态、标题、cwd、Turn 和 next_action；
5. 标题为空时省略；
6. 按最后活动时间稳定排序；
7. 调用列表不更新 Session 活动时间；
8. close 和实例退出后对应 Session 不再出现。

### 验证命令

```bash
go test ./...
go test -race ./...
go vet ./...
task build
```

## 范围外

本次不实现：

- agent 崩溃后的 Session 自动恢复；
- bridge 重启后的活跃 Session 恢复；
- agent 子进程跨 bridge 进程存活；
- Session 自动 TTL；
- Session 自动 LRU 淘汰；
- `acp_sessions` 分页或过滤；
- bridge 自动生成 Session 标题；
- 每个 Session 独立启动一个 agent 子进程。
