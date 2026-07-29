# 生命周期自审问题修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Agent 默认启动、Session 创建事务、删除合同和 bridge 关闭预算问题，并补齐关键 MCP 回归测试与生命周期可观测性。

**Architecture:** 保持 `Manager → AgentInstance → Client → AgentProcess` 所有权不变。所有创建型 ACP 操作统一执行“远端创建、原子注册、失败补偿”，重复原始 Session ID 视为实例级协议违约；关闭阶段共享调用方总截止时间，Client 完成优雅关闭后才取消 Manager context。

**Tech Stack:** Go 1.26.5、`github.com/coder/acp-go-sdk` v0.13.5、`github.com/modelcontextprotocol/go-sdk` v1.7.0、标准库 `context`、`errors`、`log/slog`、`sync`、`testing`。

## Global Constraints

- 不改变每种 agent 一个常驻实例、Session 永久保留和默认最多 10 个活跃 Session 的产品合同。
- handler context 取消触发的 Turn 中断必须使用脱离已取消 handler 的短超时 context。
- bridge 关闭的全部 Session、Turn、Client 和 Process 共享 `runWith` 提供的总截止时间。
- agent 返回重复原始 Session ID 时禁止调用该 ID 的 ACP CloseSession。
- 手工文件修改只使用 `apply_patch`；每项行为变更先写测试并确认 RED。

---

### Task 1: 统一 Driver 默认命令解析

**Files:**
- Modify: `internal/driver/claude.go`
- Modify: `internal/driver/gemini.go`
- Test: `internal/driver/driver_test.go`

**Interfaces:**
- Consumes: `splitCommand(string) (string, []string)`
- Produces: Claude/Gemini 与 Codex/OpenCode 一致的完整命令字符串解析行为

- [x] 写表驱动测试，使用 `sh -c true` 启动 Claude/Gemini Driver。
- [x] 运行 `go test ./internal/driver -run TestCommandStringDriversStart -count=1`，确认因整串 `LookPath` 失败。
- [x] 在两个 Driver 的 `Start` 中拆分 executable 与 initial args。
- [x] 重跑聚焦测试并确认通过。

### Task 2: 修复 Session 创建事务和 delete 合同

**Files:**
- Modify: `internal/instance/manager.go`
- Test: `internal/instance/manager_test.go`
- Modify: `internal/session/session.go`

**Interfaces:**
- Produces: `ErrSessionActive`
- Produces: 创建/fork 注册失败补偿逻辑
- Produces: 重复 Session ID 的实例隔离逻辑

- [x] 增加测试：重复 NewSession ID 不调用 CloseSession，并清理该实例全部本地 Session。
- [x] 增加测试：Fork 注册失败时关闭新建远端 Session。
- [x] 增加测试：Delete 活跃 Session 返回 `ErrSessionActive` 且不调用远端 Delete。
- [x] 运行对应测试并确认 RED。
- [x] 实现可判别的注册失败补偿和实例清理；delete 在远端调用前检查活跃索引。
- [x] 重跑 `go test -race ./internal/instance -count=1`。

### Task 3: 让 shutdown 和 interrupt 服从统一预算

**Files:**
- Modify: `internal/instance/manager.go`
- Modify: `internal/instance/turn.go`
- Test: `internal/instance/manager_test.go`
- Test: `internal/driver/process_test.go`

**Interfaces:**
- Produces: `interrupt(context.Context, sessionRef, *session.Turn, string)`
- Produces: 共享截止时间的并发 Session/Client 关闭阶段

- [x] 增加测试：已取消的 `Manager.Close` 不等待内部两个固定 3 秒窗口。
- [x] 增加测试：Manager context 在 Client.Close 执行后才取消。
- [x] 运行聚焦测试并确认 RED。
- [x] 让 ACP Cancel 和 controller wait 共用一个由 Instance context、调用方预算和短超时组成的 context。
- [x] 并发关闭 Session 与 Client，最后调用 `m.cancel()`。
- [x] 重跑 instance 和 driver 竞态测试。

### Task 4: 恢复 MCP 错误合同并补关键工具测试

**Files:**
- Modify: `internal/mcp/handlers.go`
- Test: `internal/mcp/handlers_test.go`
- Modify: `internal/session/id.go`
- Modify: `internal/session/turn.go`

**Interfaces:**
- Produces: 空 `turn_id` 返回 `turn_id is required`
- Produces: 既定 Session/Turn 错误文本

- [x] 增加 `acp_interrupt` 缺少 turn ID、活跃 Session delete、fork/close/interrupt 结构化错误测试。
- [x] 运行聚焦 MCP 测试并确认 RED。
- [x] 增加 handler 边界校验并对齐既定错误文本。
- [x] 重跑 `go test -race ./internal/mcp ./internal/session -count=1`。

### Task 5: 补生命周期日志、API 注释并全量验证

**Files:**
- Modify: `internal/instance/manager.go`
- Modify: `internal/instance/turn.go`
- Modify: `internal/session/session.go`
- Modify: `internal/session/turn.go`

**Interfaces:**
- Produces: 不包含 prompt 内容的实例、Session、Turn、shutdown 结构化日志

- [x] 为实例启动/退出、Session 创建/关闭、Turn 终态和 Manager shutdown 添加 `agent_type`、`session_id`、`turn_id`、`elapsed` 等字段。
- [x] 为本次涉及的 exported API 补充错误、副作用和并发语义注释。
- [x] 运行 `gofmt`、`go vet ./...`、`go test -race ./... -count=1`、`go build ./...` 和 `git diff --check`。

### Task 6: 收紧同一 Session 的并发生命周期边界

**Files:**
- Modify: `internal/instance/manager.go`
- Modify: `internal/instance/turn.go`
- Modify: `internal/session/session.go`
- Test: `internal/instance/manager_test.go`

**Interfaces:**
- Produces: qualified Session ID 级操作门闩
- Produces: `Session.BeginClose()`
- Produces: `ErrInstanceChanged`
- Produces: controller 独占的 Turn 排空语义

- [x] 增加关闭与 Chat、重复关闭、interrupt 预算先结束的并发回归测试并确认 RED。
- [x] 让 Session 在中断前原子进入 closing，并由 controller 唯一执行 `FinishTurn`。
- [x] 增加 delete/load、load/resume、fork/close 的同 ID 远端调用串行化测试并确认 RED。
- [x] 实现按 qualified ID 操作门闩，并在 Manager shutdown 时等待门闩 owner 与创建预留排空。
- [x] ACP 调用返回后重新校验 Instance generation、Session 引用和 closing 状态。
- [x] 补齐失败日志、进程退出错误、公开错误和 Driver/Process API 注释。
- [x] 重跑全仓格式化、竞态测试、静态检查和构建。
