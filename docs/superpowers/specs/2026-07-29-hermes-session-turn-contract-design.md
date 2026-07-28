# Hermes 会话与轮次契约设计

## 目标

让 Hermes 通过稳定、职责单一的字段管理 ACP 会话和长任务：

- `session_id` 只标识 bridge 会话；
- `title` 只承载 agent 上报的人类可读标题；
- `turn_id` 标识当前保留的 turn；
- `request_id` 只标识待回复的权限请求。

## 字段语义

### session_id

`session_id` 是 bridge 生成的稳定会话主键，也是所有对话和会话管理操作使用的会话索引。

### title

`title` 来自 agent 的 `SessionInfoUpdate.Title`：

- agent 未上报标题时不返回该字段；
- bridge 不生成默认标题；
- 标题只作为结果元数据和 session 列表中的人类可读索引；
- 对话工具不接受 `title` 作为输入，不得把标题填入 `session_id`。

### turn_id

`turn_id` 由 bridge 为每次 `acp_chat` 调用生成，用于标识当前正在执行或已终止但尚未进入下一轮的 Prompt。它不是会话索引，也不用于续接下一轮对话。

### request_id

`request_id` 标识当前 turn 中待处理的权限请求，供 `acp_respond` 使用。

## acp_chat

### 新会话

当 `session_id` 为空时：

1. 读取 `agent_type`，为空则使用 `codex`；
2. 读取 `cwd`，为空则使用 `.`；
3. 获取或启动对应类型的 ACP Client；
4. 调用 ACP `session/new`；
5. 保存 agent 类型和 ACP session ID；
6. 生成新的 `turn_id` 并执行 Prompt。

### 续会话

当 `session_id` 非空时：

1. 先从 `SessionPool` 查询 bridge Session；
2. 查询失败时直接返回 `session not found`，不得回退创建新会话；
3. 根据 Session 中保存的 `AgentType` 获取 ACP Client；
4. 使用 Session 中保存的 `ACPSessionID` 执行 Prompt；
5. 完全忽略本次请求中的 `agent_type` 和 `cwd`；
6. 为本轮生成新的 `turn_id`。

`acp_chat` 的输入不包含 `title` 或 `turn_id`。

### 返回

- `completed`、`running` 和 `permission_required` 均返回本轮生成的 `turn_id`；
- session 已有标题时返回 `title`，否则省略；
- `running` 返回的 `turn_id` 是后续 `acp_progress`、`acp_interrupt` 的必填参数。

## acp_progress

输入必须包含：

- `session_id`；
- `turn_id`。

处理顺序：

1. 按 `session_id` 查询 Session；
2. 查询当前保留的 turn；
3. `turn_id` 为空时返回参数错误；
4. 没有保留的 turn 时返回 `turn not found`；
5. `turn_id` 与当前 turn 不一致时返回 `turn mismatch`；
6. 一致时返回 `running`、`permission_required`、`completed` 或 `interrupted`。

返回中包含实际 `session_id`、`turn_id`，有标题时包含 `title`。

`acp_progress` 是幂等状态查询：

- `running` 返回当前累计进度；
- `permission_required` 在调用 `acp_respond` 前可重复查询；
- `completed` 和 `interrupted` 返回缓存的稳定快照；
- 同一个 `session_id + turn_id` 在下一轮开始前可重复查询，结果不会因查询本身被删除。

## acp_interrupt

输入必须包含：

- `session_id`；
- `turn_id`。

只有 `turn_id` 与该 session 当前保留的 turn 一致，且 turn 仍在执行或等待权限时，才发送 ACP Cancel 并取消后台 Prompt。空值、无 turn、不匹配或 turn 已终止均返回工具错误，不得影响其他 turn。

中断后：

- turn 状态变为 `interrupted`；
- Session 回到 `idle`；
- 保留中断前已收集的 `agent_text`、`reasoning`、`tool_calls` 和 `plan`；
- 返回中包含 `status: interrupted`、`session_id`、`turn_id`，有标题时包含 `title`；
- 下一轮 `acp_chat` 开始前，`acp_progress` 可重复查询同一份中断快照。

## acp_respond

输入包含：

- `session_id`；
- `request_id`；
- `outcome`。

`acp_respond` 不接收 `turn_id`。服务端必须确认：

1. Session 处于 `permission_pending`；
2. Session 存在活跃 turn；
3. turn 存在待回复权限请求；
4. `request_id` 与当前待回复请求一致。

权限回复后继续原 turn，返回值仍携带该 turn 的 `turn_id`；有标题时返回 `title`。

## turn 保留与进入下一步

每个 Session 只保留一个当前 turn。turn 完成或中断时不立即删除，而是缓存最终结构化结果。

“进入下一步”定义为以下任一事件：

1. 同一 Session 再次调用 `acp_chat`，新 turn 替换旧 turn；
2. 调用 `acp_close` 关闭 Session；
3. Session 被 TTL 或 LRU 清理。

在进入下一步前，`acp_progress` 可重复查询当前 turn。新一轮开始后，旧 `turn_id` 不再有效；使用旧 ID 查询或中断时返回 `turn mismatch`。

## 其他工具

`acp_close`、`acp_sessions`、`acp_session_info`、配置工具和持久化会话工具不接收 `turn_id`。它们操作整个 session 或 agent 持久化记录，不操作某一个 Prompt turn。

现有 `acp_sessions`、`acp_session_info`、`acp_list_history` 继续返回 agent 已提供的标题。其他返回具体 bridge session 的结构化结果也应在标题非空时返回 `title`。

## 标题更新

收到 `SessionInfoUpdate.Title` 后立即保存到 bridge Session：

- `running` 阶段已经收到标题时，不必等待 `completed` 才可见；
- 后续结果和 `acp_sessions` 使用最新标题；
- 空标题不覆盖已有非空标题。

## 错误语义

- 续会话不存在：`session not found`；
- `acp_progress` 或 `acp_interrupt` 缺少 `turn_id`：`turn_id is required`；
- Session 没有保留的 turn：`turn not found`；
- `turn_id` 不匹配：`turn mismatch`；
- 对已完成或已中断的 turn 再次执行 `acp_interrupt`：`turn is not interruptible`；
- 权限 `request_id` 不匹配：`permission request mismatch`。

上述业务错误均使用 MCP `IsError: true` 和对应结构化错误结果返回。

## 测试

### 会话路由

1. 新会话仍按 `agent_type` 创建 ACP Client；
2. 续接非 Codex 会话且省略 `agent_type`，使用原 Session 的 ACP Client；
3. 续接会话时传入冲突或未知 `agent_type`，仍使用原 Session 的 ACP Client；
4. 续会话传入不同 `cwd` 时忽略新值；
5. 不存在的 `session_id` 不启动 Client、不创建 ACP session。

### 标题

1. agent 未上报标题时结果不返回 `title`；
2. `running` 阶段收到标题后立即保存并返回；
3. `completed`、`permission_required` 和 session 查询返回已有标题；
4. 后续标题更新覆盖旧标题，空标题不覆盖已有标题。

### turn_id

1. `acp_chat` 不接收 `turn_id`，每轮仍生成并返回新的 ID；
2. `acp_progress` 缺少、错误或已失效的 `turn_id` 时返回对应错误；
3. `completed` 和 `interrupted` 结果在下一轮开始前可被重复查询；
4. 新一轮 `acp_chat` 替换旧 turn，旧 `turn_id` 随即失效；
5. `acp_interrupt` 缺少、错误或已失效的 `turn_id` 时不中断 Prompt；
6. 正确的 `session_id + turn_id` 可以轮询或中断对应 turn；
7. `acp_interrupt` 返回并保留 `interrupted` 快照；
8. `acp_respond` 无需 `turn_id`，但必须匹配当前 `request_id`。

## 范围

本次修改 MCP 参数与结构化输出、Session/turn 查询逻辑、工具名、skill 文档和对应测试。`acp_interrupt` 替换 `acp_cancel`，工具总数不变；不改变 ACP 子进程启动方式、SessionPool 容量策略或 agent 持久化协议。
