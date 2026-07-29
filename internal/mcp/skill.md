---
name: acp-bridge
description: Use when calling acp_* tools to work with Codex/Claude/Gemini/OpenCode, when checking any active session with acp_progress, when status is running or permission_required, when choosing the next acp_* call, or when managing sessions and history.
---

# acp-bridge

## Overview

用 `acp_*` 工具把任务交给子 agent。**只根据工具返回的 `session_id` / `turn_id` / `status`（以及列表里的 `state` / `turn_status`）决定下一步**。`acp_progress` 可在任意时刻查询任意活跃会话，不限于「刚 chat 完在轮询」。

## When to Use

- 发 prompt、续聊、中断、关闭会话
- 想查看某个活跃会话当前在做什么、已有哪些输出或是否在等权限 → `acp_progress`
- 收到 `running` 或 `permission_required`
- 从返回状态选择下一个 `acp_*`
- 列活跃会话、查 session 元数据、fork / load / resume / delete 历史

## When NOT to Use

- 不需要子 agent 的本地读写或搜索

## ID 规则

- 所有会话 ID 形如 `<agent_type>:<agent_session_id>`（例如 `codex:thread-1`）
- 新建：省略 `session_id`，从返回值里取
- 续聊 / progress / interrupt / close / info / set_* / fork：只传该 ID
- load / resume / delete：只传 qualified ID（不要另传 `agent_type`）
- 默认：`agent_type=codex`，`cwd=.`（仅新建时有意义）

## 返回 status → 下一步

| 返回 | 你要做的 |
|------|----------|
| `completed` / `error` / `interrupted` | 读结果；可同 `session_id` 再 `acp_chat`，或 `acp_close` |
| `running` | **马上**保存 `session_id`+`turn_id`，建立跟踪，之后用 `acp_progress` 轮询 |
| `permission_required` | `acp_respond(session_id, request_id, allow\|deny)` |
| `idle` | 尚无 turn 或可开新一轮 → 需要时 `acp_chat` |

列表 `acp_sessions` 辅助判断：

| `state` / `turn_status` | 下一步 |
|-------------------------|--------|
| `idle` | `acp_chat` |
| `prompting` + `running` | `acp_progress`；要停则 `acp_interrupt(session_id, turn_id)` |
| `permission_pending` | `acp_progress` 取 `request_id` → `acp_respond` |
| `closing` | 不要再 chat |

**没有 `next_action` 字段。** 同一 `session_id` 上不要并发两个 prompt（会 `session busy`）。

## 工具怎么用

| 工具 | 何时用 | 关键参数 |
|------|--------|----------|
| `acp_chat` | 新任务或续聊 | `prompt`；续聊加 `session_id`；新建可加 `cwd`/`agent_type` |
| `acp_progress` | **随时**看会话：状态、本轮/最近一轮内容与进度 | `session_id`（必填）；`turn_id?` 精确校验某一轮 |
| `acp_respond` | 审批工具调用 | `session_id`，`request_id`，`outcome=allow\|deny` |
| `acp_interrupt` | 停当前这一轮 | `session_id` + **`turn_id`（必填）**；会话仍在，可再 chat |
| `acp_close` | 结束并释放会话 | `session_id` |
| `acp_sessions` | 看有哪些活跃会话 | 无参数；可能带 `turn_id`/`turn_status` |
| `acp_session_info` | 看 mode / 配置项 / 命令 | `session_id` |
| `acp_set_mode` | 改权限模式 | `session_id`，`mode` |
| `acp_set_config` | 改配置（如 model） | `session_id`，`config_id`，`value` |
| `acp_fork_session` | 从现有会话分出新会话 | 活跃 `session_id` → 新 ID |
| `acp_list_history` | 列历史 | 可选 `agent_type`（默认 codex） |
| `acp_load_session` | 把历史加载成活跃 | qualified `session_id`，可选 `cwd` |
| `acp_resume_session` | 恢复已关闭历史 | 同上 |
| `acp_delete_session` | 删历史 | 仅非活跃；活跃须先 `acp_close` |

业务失败时看结构化 `error`（`IsError=true`）。

## 推荐调用序列

```text
acp_chat(prompt, cwd?)
  → running → 记下双 ID → 循环 acp_progress
  → permission_required → acp_respond → 再等
  → completed | error | interrupted → 用结果
可选：acp_session_info → set_mode / set_config
可选：同 session 再 acp_chat
收工：acp_close(session_id)
```

- **`acp_progress` 是通用只读查询**，不只服务于 running 轮询。任意对话中只要有活跃 `session_id`，都可调用：
  - 会话现在是 idle / running / 等权限 / 已结束？
  - 当前或最近一轮的 `agent_text` / `reasoning` / `tool_calls` / `plan` / `usage` 等
  - 是否出现 `permission_required`（含 `request_id`）
  - 只传 `session_id`：看「当前或最近一轮」；加上 `turn_id`：确认仍是那一轮（不匹配则 `turn mismatch`，不改会话状态）
- `running` 后去干别的之前，必须先留下可复查的 `session_id`/`turn_id`（todo 等）
- 轮询不必极高频；复杂任务按分钟级间隔即可
- 要停一轮用 `acp_interrupt`，**不要**用 `acp_close` 当中断
- 终态结果会留到该 session **下一次成功 chat** 之前，可用 `acp_progress` 再读

## 历史与清理

- 活跃会话不会自己消失；不用了就 `acp_close`
- 活跃数有上限（默认 10）；满了先 close 闲置会话，再 new/load/resume/fork
- 已在活跃列表的 ID：直接 chat，不要 load/resume
- 删除：`acp_close`（若活跃）→ `acp_delete_session`

## 常见错误

| 错误 / 现象 | 做法 |
|-------------|------|
| `session not found` | 查 `acp_sessions` 或 `acp_list_history` |
| `session busy` | `acp_progress` 等到终态，或 `acp_interrupt` |
| `session is active; close it before deleting` | 先 close |
| `session already active` | 别 load；直接用该 ID chat |
| `session limit reached...` | close 不用的会话 |
| `turn_id is required` / `turn mismatch` | 用最近 chat/progress/列表里的 turn_id |
| `turn is not interruptible` | 已是终态，直接读结果 |
| permission 未决时又 chat | 先 `acp_respond` |

## Red Flags

- 拿到 `running` 没存 `turn_id` 就切换上下文
- 对忙碌 session 连发 chat
- 对活跃 ID delete / 对活跃 ID load
- 把一次等待超时当成任务失败或已取消
