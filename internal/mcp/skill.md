---
name: acp-bridge
description: Use when delegating coding tasks to a subagent (Codex, Claude, Gemini, or OpenCode) via ACP, when acp_chat returns status running, when an agent requests permission approval, or when managing agent sessions (fork, resume, load history)
---

# acp-bridge

## Overview

acp-bridge 暴露 14 个 MCP 工具（`acp_*` 前缀），用于将编码任务委托给 ACP 兼容的子 agent 后端（codex / claude / gemini / opencode）。所有返回值都是结构化 JSON。

## When to Use

- 需要委托子 agent 执行编码任务
- `acp_chat` 返回 `status: running` 需要轮询结果
- agent 返回 `permission_required` 需要审批
- 需要查看 agent 支持的配置项、slash 命令、权限模式

## When NOT to Use

- 自己能直接完成的简单编码任务
- 纯文件读写或搜索

## Quick Reference

| 工具 | 参数 | 用途 |
|------|------|------|
| `acp_chat` | prompt, session_id?, cwd?, agent_type? | 发送 prompt，等待结果 |
| `acp_respond` | session_id, request_id, outcome | 回复权限请求（allow/deny） |
| `acp_progress` | session_id, turn_id | 检查当前 turn，返回可重复查询的状态快照 |
| `acp_interrupt` | session_id, turn_id | 中断当前 turn（session 保持存活） |
| `acp_close` | session_id | 关闭并释放 session |
| `acp_sessions` | — | 列出活跃 session |
| `acp_session_info` | session_id | 查看 config_options、available_commands、mode 等 |
| `acp_set_mode` | session_id, mode | 设置权限模式 |
| `acp_set_config` | session_id, config_id, value | 设置配置项（model、reasoning_effort 等） |
| `acp_fork_session` | session_id | 分支会话 |
| `acp_load_session` | session_id, agent_type?, cwd? | 加载持久化会话 |
| `acp_resume_session` | session_id | 恢复已关闭会话 |
| `acp_list_history` | cwd? | 列出历史会话 |
| `acp_delete_session` | session_id, agent_type? | 删除持久化会话 |

默认值：`agent_type` 默认 `codex`，`cwd` 默认 `.`。

## acp_chat 返回值

三种 `status`：

**`completed`** — agent 完成。关键字段：`agent_text`、`reasoning`、`tool_calls`（含 raw_input/raw_output/kind/status/locations）、`plan`（含 content/status/priority）、`file_changes`（path/kind=created|modified）、`usage`（used_tokens/total_tokens/cost）、`stop_reason`、`turn_count`。

**`permission_required`** — agent 需要审批。返回 `request_id` 和 `permission`（含 tool_call_id、title、kind、options）。调用 `acp_respond` 回复。

**`running`** — 超时未完成，agent 仍在后台执行。返回已有进度快照（agent_text/reasoning/tool_calls/plan）+ session_id + turn_id。

`acp_progress` 返回结构与 `acp_chat` 完全一致。`completed` 和 `interrupted` 会保留到同一 session 的下一次 `acp_chat`，期间可以使用相同的 `session_id + turn_id` 重复查询。

## Core Pattern: running 必须建 todo

收到 `running` 时 **必须立即创建 todo**，否则会遗忘正在执行的 agent 任务：

```
1. acp_chat → status: running, session_id: "s-1", turn_id: "t-1"
2. 创建 todo: [s-1/t-1] <任务描述> — 调 acp_progress 取结果
3. acp_progress(session_id: "s-1", turn_id: "t-1")
   → completed:           读取结果，勾掉 todo
   → interrupted:         读取中断前快照，勾掉 todo
   → running:             继续等待，稍后再查
   → permission_required: 调 acp_respond
```

todo 必须同时包含 `session_id` 和 `turn_id`。轮询间隔不需要太短——复杂任务通常需要数分钟。

## Core Pattern: 权限审批

权限请求可能出现在两个地方——`acp_chat` 和 `acp_progress` 的返回值中：

```
acp_chat / acp_progress → permission_required, request_id: "tc-1"
acp_respond(session_id, request_id: "tc-1", outcome: "allow"|"deny")
→ completed | permission_required | running
```

`acp_respond` 后同样等待超时，可能再次返回 `running`——此时同样需要建 todo 轮询。

## Core Pattern: 多轮对话

首次调用省略 `session_id` 创建新 session，后续调用传入 `session_id` 保持上下文：

```
1. acp_chat(prompt: "分析测试覆盖率", cwd: "/project")
   → completed, session_id: "s-1"
2. acp_chat(prompt: "为未覆盖分支添加测试", session_id: "s-1")
   → completed
3. acp_chat(prompt: "再跑一次确认", session_id: "s-1")
   → completed
4. acp_close(session_id: "s-1")
```

续会话只按 `session_id` 找回原会话；此时传入的 `agent_type` 和 `cwd` 会被忽略。`title` 是 agent 上报的可选输出元数据，不是 session 索引，也不作为任何对话工具的输入。

同一 session 上不能并发发 prompt——如果 session 正在执行（状态为 prompting），再次调用 `acp_chat` 会返回 `session busy` 错误。需要先 `acp_interrupt(session_id, turn_id)`，或等 `acp_progress(session_id, turn_id)` 返回 completed。

## 会话探索

首次 chat 后建议调 `acp_session_info` 了解 agent 能力：
- `config_options` — 可配置项及当前值（model、reasoning_effort）
- `available_commands` — 支持的 slash 命令（/plan、/research 等）
- `current_mode` — 当前权限模式

## 资源清理

- 任务完成后调 `acp_close` 释放 session
- 空闲 session 会自动超时清理（默认 30 分钟（1800s）），但显式关闭更可靠
- `acp_interrupt` 只中断匹配 `turn_id` 的当前 turn，session 保持存活可继续使用

## Common Mistakes

| 错误 | 原因 | 解决 |
|------|------|------|
| session not found | ID 错误或已超时 | `acp_sessions` 查活跃列表 |
| session busy | 已有 turn 在执行 | 先 `acp_interrupt` 或等 `acp_progress` |
| turn mismatch | turn_id 不是当前 turn | 使用最近一次 `acp_chat` 返回的 turn_id |
| waiting for permission | 处于权限等待 | 调 `acp_respond` 而非 `acp_chat` |
| running 后遗忘 | 没建 todo | 收到 running 必须立即建 todo |
| 并发 prompt | 同 session 已有 turn | 一个 session 同时只能有一个活跃 turn |
