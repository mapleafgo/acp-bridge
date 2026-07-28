# Hermes 会话与轮次契约实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 ACP 对话工具调整为 Hermes 可稳定索引的 session/turn 契约，并让完成或中断的 turn 在进入下一轮前可重复查询。

**Architecture:** bridge Session 继续保存 agent 类型、ACP session ID 和标题；`Server.turns` 为每个 Session 保留一个 `promptTurn`。`promptTurn` 使用互斥锁保护权限请求与终态快照，完成和中断只生成一次结构化快照，下一次 `acp_chat` 才替换旧 turn。

**Tech Stack:** Go 1.26.5、`github.com/coder/acp-go-sdk` v0.13.5、`github.com/modelcontextprotocol/go-sdk` v1.6.1、标准库 `testing`。

## Global Constraints

- `session_id` 与 `title` 是两个字段，所有对话操作只用 `session_id` 定位会话。
- `acp_chat` 不接收 `title` 或 `turn_id`；续会话忽略传入的 `agent_type` 和 `cwd`。
- `acp_progress`、`acp_interrupt` 必须接收 `session_id + turn_id`。
- `acp_respond` 必须接收 `session_id + request_id + outcome`，不接收 `turn_id`。
- title 只取 agent 上报的非空 `SessionInfoUpdate.Title`，没有标题时省略。
- 业务错误通过 MCP `IsError: true` 返回。
- 手工文件修改只使用 `apply_patch`。
- 当前目录不是有效 Git 仓库，因此无法创建 worktree 或提交；每个任务以测试通过作为检查点。

---

### Task 1: 修正续会话路由

**Files:**
- Modify: `internal/mcp/handlers_test.go`
- Modify: `internal/mcp/handlers.go`

**Interfaces:**
- Consumes: `session.Session.AgentType`、`session.Session.ACPSessionID`
- Produces: `handleAcpChat` 的 session-first 路由；新会话才调用 `defaultAgentType(args.AgentType)`

- [x] **Step 1: 编写失败测试**

在 `internal/mcp/handlers_test.go` 增加两个 mock client，记录 factory 收到的 agent 类型以及 Prompt 使用的 ACP session ID：

```go
func TestAcpChatContinuationUsesStoredAgentAndSession(t *testing.T) {
	codex := newMockAcpClient()
	claude := newMockAcpClient()
	srv := newTestServer(t, codex)
	srv.clientFactory = func(_ context.Context, agentType string) (acpClient, error) {
		switch agentType {
		case "codex":
			return codex, nil
		case "claude":
			return claude, nil
		default:
			return nil, fmt.Errorf("unexpected agent type %q", agentType)
		}
	}

	first := acpChatArgs{Prompt: "first", AgentType: "claude", CWD: "/original"}
	_, firstOut, _ := srv.handleAcpChat(context.Background(), nil, first)
	_, secondOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "second", SessionID: firstOut.SessionID, AgentType: "unknown", CWD: "/ignored",
	})

	if secondOut.Status != "completed" {
		t.Fatalf("expected continuation to complete, got %s", secondOut.Status)
	}
}
```

同时增强 `TestAcpChatSessionNotFound`，断言 factory 未被调用。

- [x] **Step 2: 运行测试并确认按预期失败**

Run:

```bash
go test ./internal/mcp -run 'TestAcpChatContinuationUsesStoredAgentAndSession|TestAcpChatSessionNotFound' -count=1
```

Expected: 续会话测试因当前代码先按请求中的 `agent_type` 创建 client 而失败。

- [x] **Step 3: 实现最小路由修复**

将 `handleAcpChat` 重排为：

```go
var (
	sess *session.Session
	cl   acpClient
	err  error
)
if args.SessionID != "" {
	sess, err = s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return chatErr(fmt.Sprintf("session not found: %s", args.SessionID))
	}
	if !sess.CanChat() {
		if sess.State == session.StatePermissionPending {
			return chatErr("waiting for permission response, use acp_respond instead")
		}
		return chatErr("session busy (prompt in progress)")
	}
	cl, err = s.getClientBySession(sess)
	if err != nil {
		return chatErr(fmt.Sprintf("agent client unavailable: %v", err))
	}
} else {
	agentType := defaultAgentType(args.AgentType)
	cl, err = s.getOrCreateClient(ctx, agentType)
	if err != nil {
		return chatErr(fmt.Sprintf("failed to start %s agent: %v", agentType, err))
	}
	// 使用 args.CWD 创建并注册新 session。
}
```

- [x] **Step 4: 运行聚焦测试**

Run:

```bash
go test ./internal/mcp -run 'TestAcpChatContinuationUsesStoredAgentAndSession|TestAcpChatSessionNotFound|TestAcpChatMultiTurn' -count=1
```

Expected: PASS。

### Task 2: 建立可重复查询的 turn 状态机

**Files:**
- Modify: `internal/mcp/types.go`
- Modify: `internal/mcp/handlers_test.go`
- Modify: `internal/mcp/handlers.go`
- Modify: `internal/session/pool_test.go`
- Modify: `internal/session/session.go`

**Interfaces:**
- Produces: `acpTurnArgs{SessionID, TurnID string}`
- Produces: `promptTurn` 上受锁保护的 `permReq`、`terminal`、`finalResult`、`finalIsError`
- Produces: `acp_progress(session_id, turn_id)` 的 repeatable snapshot

- [x] **Step 1: 编写 turn 参数与终态保留失败测试**

在 `internal/mcp/handlers_test.go` 增加：

```go
func TestAcpProgressRequiresMatchingTurnID(t *testing.T) {
	srv := newTestServer(t, newMockAcpClient())
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})

	result, out, _ := srv.handleAcpProgress(context.Background(), nil, acpTurnArgs{SessionID: chatOut.SessionID})
	if !result.IsError || out.Error != "turn_id is required" {
		t.Fatalf("expected required error, got %#v", out)
	}
	result, out, _ = srv.handleAcpProgress(context.Background(), nil, acpTurnArgs{
		SessionID: chatOut.SessionID,
		TurnID:    "wrong",
	})
	if !result.IsError || out.Error != "turn mismatch" {
		t.Fatalf("expected mismatch error, got %#v", out)
	}
}

func TestAcpProgressCompletedIsRepeatableUntilNextTurn(t *testing.T) {
	srv := newTestServer(t, newMockAcpClient())
	_, first, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "first"})

	_, again, _ := srv.handleAcpProgress(context.Background(), nil, acpTurnArgs{
		SessionID: first.SessionID,
		TurnID:    first.TurnID,
	})
	_, repeated, _ := srv.handleAcpProgress(context.Background(), nil, acpTurnArgs{
		SessionID: first.SessionID,
		TurnID:    first.TurnID,
	})
	if again.Status != "completed" || repeated.AgentText != again.AgentText {
		t.Fatalf("expected stable completed snapshot: %#v %#v", again, repeated)
	}

	_, second, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "second", SessionID: first.SessionID,
	})
	result, stale, _ := srv.handleAcpProgress(context.Background(), nil, acpTurnArgs{
		SessionID: first.SessionID,
		TurnID:    first.TurnID,
	})
	if !result.IsError || stale.Error != "turn mismatch" || second.TurnID == first.TurnID {
		t.Fatalf("old turn should be invalid after next chat")
	}
}
```

在 `internal/session/pool_test.go` 增加空标题不覆盖已有标题的测试。

- [x] **Step 2: 运行测试并确认失败**

Run:

```bash
go test ./internal/mcp ./internal/session -run 'TestAcpProgressRequiresMatchingTurnID|TestAcpProgressCompletedIsRepeatableUntilNextTurn|TestSetTitleIgnoresEmpty' -count=1
```

Expected: 参数类型不存在或 completed turn 已被删除，测试失败。

- [x] **Step 3: 定义参数和结构化输出字段**

在 `internal/mcp/types.go` 中用下列类型替换 `acpProgressArgs`：

```go
type acpTurnArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session ID"`
	TurnID    string `json:"turn_id" jsonschema:"The turn ID returned by acp_chat"`
}
```

并给 `chatResultJSON` 增加：

```go
Title string `json:"title,omitempty"`
```

- [x] **Step 4: 缓存终态快照**

将 `promptTurn` 改为由自身互斥锁保护：

```go
type promptTurn struct {
	mu           sync.Mutex
	done         chan struct{}
	result       *acp.PromptResponse
	err          error
	turnID       string
	permReq      *acp.RequestPermissionRequest
	cancel       context.CancelFunc
	terminal     bool
	finalResult  chatResultJSON
	finalIsError bool
}
```

`finalizeTurn` 不再调用 `popTurn`。它在 `turn.mu` 下先返回已有终态；首次完成时 Pop updates、应用 session 元数据、构造并缓存 `completed` 或 `error` 结果。`handleAcpProgress` 按以下顺序验证：

```go
if args.TurnID == "" {
	return chatErr("turn_id is required")
}
turn := s.peekTurn(sess.ID)
if turn == nil {
	return chatErr("turn not found")
}
if turn.turnID != args.TurnID {
	return chatErr("turn mismatch")
}
```

然后优先返回缓存终态，再检查 `done`、权限请求和 running 状态。

- [x] **Step 5: 立即持久化标题并返回 title**

提取 `applyCollectorMetadata(sess.ID, collector)`，在 running、permission、completed、interrupted 快照生成时调用。`Pool.SetTitle` 对空字符串直接返回：

```go
func (p *Pool) SetTitle(id SessionID, title string) {
	if title == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.Title = title
	}
}
```

所有具体 bridge session 的 `chatResultJSON` 从保存后的 session 标题填充 `Title`。

- [x] **Step 6: 运行聚焦测试**

Run:

```bash
go test ./internal/mcp ./internal/session -run 'TestAcpProgress|TestAcpChatTimeoutThenProgressCompletes|TestSessionTitle|TestSetTitle' -count=1
```

Expected: PASS。

### Task 3: 将 cancel 替换为带 turn 校验的 interrupt

**Files:**
- Modify: `internal/mcp/handlers_test.go`
- Modify: `internal/mcp/handlers.go`
- Modify: `internal/mcp/mcp.go`

**Interfaces:**
- Consumes: `acpTurnArgs`
- Produces: `handleAcpInterrupt(..., acpTurnArgs) (..., chatResultJSON, error)`
- Produces: 终态 `status: interrupted`

- [x] **Step 1: 编写失败测试**

增加以下行为测试：

```go
func TestAcpInterruptRequiresMatchingTurnAndRetainsSnapshot(t *testing.T) {
	// Prompt 写入 partial update 后阻塞，acp_chat 超时返回 running。
	// 错误 turn_id 必须返回 turn mismatch 且不能调用 Cancel。
	// 正确 turn_id 中断后返回 interrupted 和 partial agent_text。
	// 使用相同 session_id + turn_id 连续调用两次 acp_progress，
	// 两次都必须返回相同 interrupted 快照。
}

func TestAcpInterruptRejectsCompletedTurn(t *testing.T) {
	// 同步完成 acp_chat 后用其 turn_id 调 acp_interrupt。
	// 断言 IsError=true 且 error == "turn is not interruptible"。
}
```

- [x] **Step 2: 运行测试并确认失败**

Run:

```bash
go test ./internal/mcp -run 'TestAcpInterrupt' -count=1
```

Expected: `handleAcpInterrupt` 尚不存在或旧 cancel 不校验 turn。

- [x] **Step 3: 实现 interrupt**

将 `handleAcpCancel` 替换为 `handleAcpInterrupt`：

```go
func (s *Server) handleAcpInterrupt(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpTurnArgs,
) (*sdk.CallToolResult, chatResultJSON, error) {
	// 查询 session，校验 turn_id 必填、turn 存在且匹配。
	// 在 turn.mu 下拒绝 terminal turn。
	// 调用 ACP Cancel，取消 Prompt context。
	// PopUpdates 构造 partial snapshot，状态设为 interrupted。
	// 缓存快照并将 Session 设回 idle。
}
```

中断快照包含 `session_id`、`turn_id`、`title`、`agent_text`、`reasoning`、`tool_calls`、`plan`、`file_changes`、`usage`。完成或中断后的再次中断返回 `turn is not interruptible`。

- [x] **Step 4: 校验 acp_respond 的 request_id**

在调用 `RespondPermission` 前增加：

```go
expectedRequestID := string(turn.permReq.ToolCall.ToolCallId)
if args.RequestID != expectedRequestID {
	return chatErr("permission request mismatch")
}
```

增加测试确认错误 `request_id` 不会调用 `RespondPermission`，正确回复仍返回同一 `turn_id`。

- [x] **Step 5: 运行聚焦测试**

Run:

```bash
go test ./internal/mcp -run 'TestAcpInterrupt|TestAcpRespond|TestAcpProgress' -count=1
```

Expected: PASS。

### Task 4: 更新 MCP 工具契约与内嵌 skill

**Files:**
- Modify: `internal/mcp/mcp.go`
- Modify: `internal/mcp/mcp_test.go`
- Modify: `internal/mcp/skill.md`
- Modify: `internal/mcp/handlers_test.go`

**Interfaces:**
- Produces: MCP tool `acp_interrupt`
- Removes: MCP tool `acp_cancel`
- Documents: `acp_progress(session_id, turn_id)` 与 `acp_interrupt(session_id, turn_id)`

- [x] **Step 1: 编写工具注册失败测试**

在 `internal/mcp/mcp_test.go` 的工具列表断言中要求：

```go
if !toolNames["acp_interrupt"] {
	t.Fatal("expected acp_interrupt tool")
}
if toolNames["acp_cancel"] {
	t.Fatal("acp_cancel should not be registered")
}
```

- [x] **Step 2: 运行测试并确认失败**

Run:

```bash
go test ./internal/mcp -run 'Test.*Tools|Test.*Schema' -count=1
```

Expected: 当前仍注册 `acp_cancel`。

- [x] **Step 3: 更新注册和 skill**

`internal/mcp/mcp.go` 注册：

```go
sdk.AddTool(s.sdkServer, &sdk.Tool{
	Name:        "acp_interrupt",
	Description: "Interrupt the current ACP turn. Requires the session_id and turn_id returned by acp_chat.",
}, s.handleAcpInterrupt)
```

在 `internal/mcp/skill.md` 中：

- Quick Reference 改为 `acp_progress | session_id, turn_id`；
- `acp_cancel` 全部改为 `acp_interrupt`；
- running 示例始终传入 `turn_id`；
- 说明 completed/interrupted 在下一次 chat 前可重复查询；
- 说明续会话只需要 `session_id`，传入的 agent_type/cwd 会被忽略；
- 标题仅为可选输出元数据。

- [x] **Step 4: 格式化并运行完整验证**

Run:

```bash
gofmt -w internal/mcp/handlers.go internal/mcp/handlers_test.go internal/mcp/mcp.go internal/mcp/mcp_test.go internal/mcp/types.go internal/session/session.go internal/session/pool_test.go
go vet ./...
go test ./...
go test -race ./internal/mcp ./internal/session
go build ./cmd/acp-bridge
```

Expected: 所有命令退出码为 0；若全仓 `-race` 仍触发既有 `internal/client.TestE2EPermissionRoundTrip` 测试夹具竞态，单独记录为实施前已确认的基线问题，不混入本功能改动。

- [x] **Step 5: 契约扫描**

Run:

```bash
rg -n 'acp_cancel|acp_progress\\(session_id: "[^"]+"\\)' internal/mcp README.md DESIGN.md
```

Expected: 不再暴露 `acp_cancel`；所有 `acp_progress` 使用示例都包含 `turn_id`。
