# acp_progress 会话状态查询设计

## 目标

将 `acp_progress` 从“必须指定 turn 的进度查询”扩展为 Hermes 可随时调用的会话状态查询。

Hermes 只需持有 `session_id`，即可判断当前对话是否就绪、正在执行、等待权限、已经完成或已经中断；需要防止误查旧 turn 时，仍可额外传入 `turn_id` 做精确校验。

本设计替代 `2026-07-29-hermes-session-turn-contract-design.md` 中“`acp_progress` 必须携带 `turn_id`”的约定；其他会话和 turn 契约保持不变。

## 查询范围

每个 bridge Session 仍只保留当前或最近一个 turn，不新增 turn 历史。

- 新一轮 `acp_chat` 开始后，新 turn 替换上一轮；
- `acp_progress(session_id)` 始终查询当前或最近一个 turn；
- 关闭 Session 或被 TTL/LRU 清理后，查询返回 `session not found`。

## 输入契约

`acp_progress` 输入：

- `session_id`：必填，bridge Session ID；
- `turn_id`：可选，用于校验查询目标是否为当前或最近一个 turn。

### 只传 session_id

```json
{
  "session_id": "s-1"
}
```

处理规则：

1. Session 不存在时返回 `session not found`；
2. Session 没有 turn 时返回 `ready`；
3. Session 有 turn 时返回该 turn 的当前或最终状态。

### 同时传 session_id 和 turn_id

```json
{
  "session_id": "s-1",
  "turn_id": "t-1"
}
```

处理规则：

1. Session 不存在时返回 `session not found`；
2. Session 没有 turn 时返回 `turn not found`；
3. 当前或最近 turn 的 ID 不匹配时返回 `turn mismatch`；
4. ID 匹配时返回该 turn 的当前或最终状态。

空字符串 `turn_id` 按未传处理。

## 状态模型

`acp_progress` 返回以下正常状态：

| status | 含义 | 关键字段 | Hermes 下一步 |
|---|---|---|---|
| `ready` | Session 存在，但尚无当前或最近 turn | `session_id`、`state: idle`、可选 `title` | 可以调用 `acp_chat` |
| `running` | 当前 turn 正在执行 | `session_id`、`turn_id`、累计进度 | 稍后继续查询，必要时中断 |
| `permission_required` | 当前 turn 等待权限回复 | `session_id`、`turn_id`、`request_id`、`permission` | 调用 `acp_respond` |
| `completed` | 最近 turn 已完成 | `session_id`、`turn_id`、最终结构化结果 | 继续下一轮或关闭 |
| `interrupted` | 最近 turn 已中断 | `session_id`、`turn_id`、中断前快照 | 继续下一轮或关闭 |

`ready` 不返回 `turn_id`。其他正常状态均返回实际的 `turn_id`。

Prompt 自身执行失败继续作为 MCP 工具错误返回，不转换为 `ready`。

## ready 返回

Session 存在但 `Server.turns` 中没有对应 turn 时：

```json
{
  "status": "ready",
  "session_id": "s-1",
  "state": "idle"
}
```

如果 agent 已上报非空标题，则额外返回：

```json
{
  "title": "Refactor authentication"
}
```

bridge 不生成默认标题。

## 与其他工具的关系

- `acp_chat` 输入保持不变，不接收 `turn_id`；
- `acp_interrupt` 仍必须接收 `session_id + turn_id`，避免中断错误 turn；
- `acp_respond` 仍使用 `session_id + request_id + outcome`；
- `acp_sessions` 继续用于列出所有活跃 Session；
- `acp_session_info` 继续用于查看配置、能力和 Session 元数据。

本次修改不增加新 MCP 工具。

## Hermes 调用模式

Hermes 的默认状态查询只需传入 `session_id`：

```text
acp_progress(session_id: "s-1")
```

当 Hermes 保存了某次 `acp_chat` 返回的 `turn_id`，并希望确认查询目标没有被新一轮替换时，可以使用：

```text
acp_progress(session_id: "s-1", turn_id: "t-1")
```

查询本身不改变 Session 或 turn 状态，也不删除最终快照。

## 错误语义

- Session 不存在：`session not found`；
- 显式传入 `turn_id`，但 Session 没有 turn：`turn not found`；
- 显式传入的 `turn_id` 与当前或最近 turn 不一致：`turn mismatch`；
- agent client 不可用：`agent client unavailable`；
- Prompt 执行失败：保留原有工具错误。

上述错误均使用 MCP `IsError: true`。`ready` 是正常状态，使用 `IsError: false`。

## 测试

### Session 级查询

1. 只传 `session_id`，无 turn 时返回 `ready`；
2. `ready` 返回 `state: idle`，不返回 `turn_id`；
3. 有真实标题时 `ready` 返回 `title`，无标题时省略；
4. 只传 `session_id` 可查询 `running`；
5. 只传 `session_id` 可查询 `permission_required`；
6. 只传 `session_id` 可重复查询 `completed`；
7. 只传 `session_id` 可重复查询 `interrupted`。

### 精确 turn 查询

1. 正确 `turn_id` 返回当前状态；
2. 错误 `turn_id` 返回 `turn mismatch`；
3. Session 没有 turn，但显式传入 `turn_id` 时返回 `turn not found`；
4. 新一轮开始后，旧 `turn_id` 返回 `turn mismatch`。

### MCP 契约

1. `acp_progress` 的 `session_id` 在输入 schema 中必填；
2. `turn_id` 在输入 schema 中可选；
3. `acp_interrupt` 的 `turn_id` 仍保持必填；
4. 内嵌 skill 的默认示例使用 session 级查询，同时说明精确查询方式。

## 范围

本次修改：

- 调整 `acp_progress` 参数 schema；
- 增加 `ready` 正常结果；
- 调整 handler 的 turn 校验分支；
- 更新内嵌 skill、`DESIGN.md` 和对应测试。

本次不修改：

- turn 历史保留策略；
- `acp_interrupt`、`acp_respond` 的参数契约；
- SessionPool 的 TTL、LRU 和容量策略；
- ACP 子进程或持久化会话协议。
