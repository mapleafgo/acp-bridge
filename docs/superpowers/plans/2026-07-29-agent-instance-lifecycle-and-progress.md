# Agent 实例生命周期与 acp_progress 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将当前 `Server.clients + Server.turns + SessionPool` 重构为每种 agent 一个常驻实例、实例承载多个永久 Session 的运行时，并让 `acp_progress(session_id)` 能查询 Session 当前状态。

**Architecture:** 新增 `internal/instance.Manager` 作为唯一运行时入口，所有权固定为 `Manager → Instance → Client → Process` 和 `Instance → Session → Turn`。MCP handler 只做参数与结构化输出转换；Session 使用 `<agent_type>:<agent_session_id>`，Turn controller 独占 Prompt 完成与权限事件消费。

**Tech Stack:** Go 1.26.5、`github.com/coder/acp-go-sdk` v0.13.5、`github.com/modelcontextprotocol/go-sdk` v1.7.0、标准库 `context`/`os/exec`/`sync`/`testing`。

## Global Constraints

- 每个 `agent_type` 在 bridge 进程中最多一个 AgentInstance，零 Session 时实例继续运行。
- Session 只因 `acp_close`、agent 实例退出或 bridge 退出而结束；禁止 TTL 和 LRU 自动淘汰。
- 默认全局最多 10 个活跃 Session；满额拒绝 new/load/resume/fork，不影响已有 Session；`MaxSessions=0` 表示无限制。
- `acp_sessions()` 不分页并返回全部活跃 Session。
- MCP 公开 Session ID 固定为 `<agent_type>:<agent_session_id>`；不再生成或接受 `s-*`。
- `acp_progress` 的 `session_id` 必填，`turn_id` 可选；无 Turn 时返回正常状态 `idle`。
- `acp_interrupt` 仍要求 `session_id + turn_id`；MCP handler context 取消与显式中断走同一内部方法。
- 权限等待按 `(agent_session_id, request_id)` 路由；`UnstableCreateElicitation` 在当前 SDK 下明确返回不支持。
- MCP 业务错误使用 `sdk.CallToolResult{IsError: true}` 和具体结构化输出。
- 手工文件修改只使用 `apply_patch`；每个行为变更先写失败测试。

---

### Task 1: 建立 qualified Session ID 与并发安全 Session/Turn

**Files:**
- Create: `internal/session/id.go`
- Create: `internal/session/turn.go`
- Create: `internal/session/view.go`
- Modify: `internal/session/session.go`
- Delete: `internal/session/pool_test.go`
- Create tests: `internal/session/id_test.go`
- Create tests: `internal/session/session_test.go`
- Create tests: `internal/session/turn_test.go`

**Interfaces:**
- Produces: `session.ID{AgentType driver.AgentType, AgentSessionID string}`
- Produces: `session.ParseID(string) (session.ID, error)`、`session.ID.String() string`
- Produces: `session.Session` 私有字段、`View() SessionView`、`BeginTurn(*Turn) error`、`CurrentTurn() *Turn`
- Produces: `session.Turn` 的唯一终态与可重复快照

- [ ] **Step 1: 编写 ID 与 Session 状态失败测试**

```go
func TestParseIDPreservesColonInAgentSessionID(t *testing.T) {
	id, err := ParseID("codex:thread:child")
	if err != nil {
		t.Fatal(err)
	}
	if id.AgentType != driver.AgentTypeCodex || id.AgentSessionID != "thread:child" {
		t.Fatalf("unexpected ID: %#v", id)
	}
}

func TestSessionBeginTurnRejectsBusySession(t *testing.T) {
	s := New(ID{AgentType: driver.AgentTypeCodex, AgentSessionID: "one"}, "/tmp")
	first := NewTurn("t-1", func() {})
	if err := s.BeginTurn(first); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginTurn(NewTurn("t-2", func() {})); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("expected ErrSessionBusy, got %v", err)
	}
}
```

- [ ] **Step 2: 运行聚焦测试并确认失败**

Run:

```bash
go test ./internal/session -run 'TestParseID|TestSession|TestTurn' -count=1
```

Expected: FAIL，`ParseID`、`New`、`NewTurn` 尚不存在。

- [ ] **Step 3: 实现 ID、Session 和 view**

`internal/session/id.go` 定义：

```go
type ID struct {
	AgentType      driver.AgentType
	AgentSessionID string
}

func (id ID) String() string {
	return string(id.AgentType) + ":" + id.AgentSessionID
}

func ParseID(raw string) (ID, error) {
	agentType, agentSessionID, ok := strings.Cut(raw, ":")
	if !ok || agentSessionID == "" {
		return ID{}, ErrInvalidSessionID
	}
	switch driver.AgentType(agentType) {
	case driver.AgentTypeCodex, driver.AgentTypeClaude, driver.AgentTypeGemini, driver.AgentTypeOpenCode:
	default:
		return ID{}, ErrInvalidSessionID
	}
	return ID{AgentType: driver.AgentType(agentType), AgentSessionID: agentSessionID}, nil
}
```

`Session` 使用私有字段和自身 mutex，状态只保留：

```go
const (
	StateIdle              State = "idle"
	StatePrompting         State = "prompting"
	StatePermissionPending State = "permission_pending"
	StateClosing           State = "closing"
)
```

`SessionView` 返回 ID、cwd、title、mode、config、commands、turn count、last used 和当前 `TurnView` 的值拷贝；MCP 层不能取得可变 Session 字段。

Turn 状态常量统一定义为：

```go
const (
	TurnRunning            TurnState = "running"
	TurnPermissionRequired TurnState = "permission_required"
	TurnCompleted          TurnState = "completed"
	TurnInterrupted        TurnState = "interrupted"
	TurnError              TurnState = "error"
)
```

- [ ] **Step 4: 实现 Turn 唯一终态**

`Turn` 保存 `id`、`state`、`cancel`、权限队列、`changed` channel 和不可变 `TurnSnapshot`。提供：

```go
func (t *Turn) Snapshot() TurnView
func (t *Turn) EnqueuePermission(PermissionView) bool
func (t *Turn) ResolvePermission(requestID string) error
func (t *Turn) Complete(TurnSnapshot) bool
func (t *Turn) Interrupt(TurnSnapshot) bool
func (t *Turn) Fail(TurnSnapshot) bool
func (t *Turn) Wait(ctx context.Context, timeout time.Duration) TurnView
```

`Complete`、`Interrupt` 和 `Fail` 只有首次调用返回 true；每次状态变化关闭旧 `changed` 并创建新 channel，使多个 MCP 查询可以等待同一 Turn。

- [ ] **Step 5: 删除 Pool 行为并运行 session 测试**

删除 `Pool`、LRU、TTL、cleanup goroutine 及其旧测试，保留 Session metadata 类型并迁移到新文件。

Run:

```bash
go test -race ./internal/session -count=1
```

Expected: PASS，且源码中 `container/list`、`cleanupLoop`、`evictOldestLocked` 不再存在。

- [ ] **Step 6: 提交**

```bash
git add internal/session
git commit -m "refactor(session): 建立会话与轮次领域模型"
```

### Task 2: 让 Driver 和 Client 完整拥有 agent 子进程

**Files:**
- Create: `internal/driver/process.go`
- Create: `internal/driver/process_test.go`
- Modify: `internal/driver/driver.go`
- Modify: `internal/driver/codex.go`
- Modify: `internal/driver/claude.go`
- Modify: `internal/driver/gemini.go`
- Modify: `internal/driver/opencode.go`
- Modify: `internal/driver/driver_test.go`
- Modify: `internal/client/client.go`
- Modify: `internal/client/client_test.go`

**Interfaces:**
- Produces: `driver.AgentProcess` 接口
- Changes: `AgentDriver.Start(context.Context) (AgentProcess, error)`
- Changes: `client.Client.Close(context.Context) error`
- Produces: `Client.Done()` 合并 ACP connection 与 process 退出

- [ ] **Step 1: 编写 AgentProcess 生命周期失败测试**

```go
func TestAgentProcessCloseWaitsExactlyOnce(t *testing.T) {
	process, err := startProcess(context.Background(), "sh", []string{"-c", "read line"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(ctx); err != nil {
		t.Fatalf("second Close must be idempotent: %v", err)
	}
	select {
	case <-process.Done():
	default:
		t.Fatal("Done must be closed")
	}
}
```

- [ ] **Step 2: 运行 driver 测试并确认失败**

Run:

```bash
go test ./internal/driver -run TestAgentProcess -count=1
```

Expected: FAIL，`startProcess` 和 `AgentProcess` 尚不存在。

- [ ] **Step 3: 实现 AgentProcess**

定义：

```go
type AgentProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}
```

真实 process 在 `cmd.Start()` 后立即启动唯一 Wait goroutine。`Close` 首次调用关闭 stdin，等待 Done；context 到期后调用 `Process.Kill()` 并继续等待 Done；后续调用只等待同一个 Done，不再次调用 `cmd.Wait()`。

- [ ] **Step 4: 修改 Driver 与 Client**

四个 driver 的 `Start` 都返回 AgentProcess。`Client` 保存 process，不再单独保存 stdin/stdout；创建 ACP connection 时使用 process pipe，stderr 继续后台 drain。

`Client.Close(ctx)` 依次关闭 handler、关闭 stdin、等待/终止 process。`Client.Done()` 由一个只关闭一次的 channel 合并 `conn.Done()` 与 `process.Done()`；initialize 失败也必须关闭并回收 process。

- [ ] **Step 5: 运行 client/driver 竞态测试**

Run:

```bash
go test -race ./internal/driver ./internal/client -count=1
```

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/driver internal/client
git commit -m "refactor(client): 接管 agent 子进程生命周期"
```

### Task 3: 按 Session 路由权限并禁用不可路由 elicitation

**Files:**
- Modify: `internal/client/handler.go`
- Modify: `internal/client/client.go`
- Modify: `internal/client/client_test.go`

**Interfaces:**
- Produces: `client.PermissionEvent`
- Produces: `Client.PermissionEvents(agentSessionID string) <-chan PermissionEvent`
- Changes: `Client.RespondPermission(agentSessionID, requestID string, response acp.RequestPermissionResponse) error`
- Produces: `Client.ForgetSession(agentSessionID string)`

- [ ] **Step 1: 编写跨 Session 权限失败测试**

```go
func TestPermissionRoutingSeparatesSessionsWithSameRequestID(t *testing.T) {
	h := newHandler()
	aEvents := h.PermissionEvents("a")
	bEvents := h.PermissionEvents("b")
	request := func(sessionID string) {
		_, _ = h.RequestPermission(context.Background(), acp.RequestPermissionRequest{
			SessionId: acp.SessionId(sessionID),
			ToolCall: acp.ToolCallUpdate{ToolCallId: "same"},
		})
	}
	go request("a")
	go request("b")
	if event := <-aEvents; event.SessionID != "a" || event.RequestID != "same" {
		t.Fatalf("unexpected a event: %#v", event)
	}
	if event := <-bEvents; event.SessionID != "b" || event.RequestID != "same" {
		t.Fatalf("unexpected b event: %#v", event)
	}
}
```

另加测试：同 Session 多个 request 保持顺序；空 ToolCall ID 返回错误；`UnstableCreateElicitation` 返回 `errNotSupported` 且不创建 pending key；ForgetSession 和 handler close 解除等待。

- [ ] **Step 2: 运行失败测试**

Run:

```bash
go test ./internal/client -run 'TestPermissionRouting|TestPermissionQueue|TestHandlerUnstableCreateElicitation' -count=1
```

Expected: FAIL，现有 map 只按 request ID，signal 为全局 channel，elicitation 被伪装成权限。

- [ ] **Step 3: 实现复合键与 per-session queue**

```go
type permissionKey struct {
	sessionID string
	requestID string
}

type PermissionEvent struct {
	SessionID string
	RequestID string
	Request   acp.RequestPermissionRequest
}
```

handler 保存 `permissionCh map[permissionKey]chan ...` 和 `permissionEvents map[string]chan PermissionEvent`。`RequestPermission` 在写 map 前拒绝空 Session ID、空 ToolCall ID 和重复复合键；事件发送使用阻塞 select，包含 request context 与 handler close 分支，不使用 `default` 丢弃。

- [ ] **Step 4: 删除 elicitation 兼容层并运行测试**

`UnstableCreateElicitation` 直接：

```go
return acp.UnstableCreateElicitationResponse{}, errNotSupported
```

Run:

```bash
go test -race ./internal/client -count=1
```

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/client
git commit -m "fix(client): 按会话路由权限请求"
```

### Task 4: 实现 AgentInstanceManager 与永久 Session 容量

**Files:**
- Create: `internal/instance/client.go`
- Create: `internal/instance/factory.go`
- Create: `internal/instance/instance.go`
- Create: `internal/instance/manager.go`
- Create: `internal/instance/manager_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: Tasks 1-3 的 `session.Session`、`driver.AgentProcess`、权限 API
- Produces: `instance.Manager`
- Produces: `NewManager(*config.Config, ClientFactory) *Manager`
- Produces: `CreateSession`、`LoadSession`、`ResumeSession`、`ForkSession`、`CloseSession`、`DeleteSession`、`Sessions`、`History`
- Test helpers: `testConfig(max int) *config.Config`、`newFakeFactory() *fakeFactory`、`(*fakeFactory).New(context.Context, driver.AgentType) (ACPClient, error)`、`newTestManager(*testing.T, int) *Manager`

- [ ] **Step 1: 编写单实例和容量失败测试**

```go
func TestConcurrentCreateUsesOneInstance(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := factory.Starts(driver.AgentTypeCodex); got != 1 {
		t.Fatalf("starts=%d, want 1", got)
	}
}

func TestSessionLimitRejectsWithoutEviction(t *testing.T) {
	manager := newTestManager(t, 2)
	first, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	second, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("expected limit error, got %v", err)
	}
	if got := len(manager.Sessions()); got != 2 || first.ID == second.ID {
		t.Fatalf("existing sessions changed: %#v", manager.Sessions())
	}
}
```

另加并发 reservation、`MaxSessions=0`、实例零 Session 常驻、Client.Done 清理全部 Session、旧 generation 不删新实例测试。

- [ ] **Step 2: 运行 instance 测试并确认失败**

Run:

```bash
go test ./internal/instance -count=1
```

Expected: FAIL，package 尚不存在。

- [ ] **Step 3: 实现 Manager 启动槽位**

Manager 保存：

```go
type Manager struct {
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	config       *config.Config
	factory      ClientFactory
	instances    map[driver.AgentType]*instanceSlot
	sessionIndex map[string]sessionRef
	reservations int
	closing      bool
	nextGen      uint64
}
```

`instanceSlot.ready` 让同类型并发首次请求共享一次 `client.New`。创建 Client 使用 Manager lifecycle context 并在锁外执行；等待调用方可因自身 context 返回，但不会取消正在启动的实例。

- [ ] **Step 4: 实现容量 reservation 与 Session 注册**

所有 new/load/resume/fork 先在 Manager 锁内执行：

```go
if max := m.config.MaxSessions; max > 0 && len(m.sessionIndex)+m.reservations >= max {
	return ErrSessionLimit
}
m.reservations++
```

ACP 调用在锁外进行。成功后按 `Manager → Instance` 锁顺序检查 duplicate 并注册 qualified ID；失败或冲突释放 reservation。满额绝不调用 Close、Cancel 或删除已有 Session。

删除 `SessionTTL` 配置；`MaxSessions < 0` 在 `config.Load` 后返回配置错误，`0` 保持无限制。

- [ ] **Step 5: 实现实例退出与 Manager.Close**

Client.Done 触发带 Instance 指针和 generation 的清理。只有 slot 仍指向同一 generation 时才删除；清理该实例的 Turn、Session 和全局索引。

`Manager.Close(ctx)` 幂等：标记 closing、取消 Turn、尽力 CloseSession、并发关闭 Client、等待 process、清空索引。

- [ ] **Step 6: 运行 instance/config 竞态测试**

Run:

```bash
go test -race ./internal/instance ./internal/config -count=1
```

Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add internal/instance internal/config
git commit -m "feat(instance): 管理常驻 agent 与永久会话"
```

### Task 5: 实现 Turn controller、统一中断和 Session 状态查询

**Files:**
- Create: `internal/instance/turn.go`
- Create: `internal/instance/turn_test.go`
- Create: `internal/session/updates.go`
- Create: `internal/session/updates_test.go`
- Modify: `internal/instance/manager.go`
- Modify: `internal/instance/instance.go`

**Interfaces:**
- Produces: `Manager.Chat`、`Manager.Respond`、`Manager.Progress`、`Manager.Interrupt`
- Produces: `instance.ChatView`
- Consumes: `session.Turn` 状态 API与 `client.PermissionEvents`

- [ ] **Step 1: 编写 controller 与中断失败测试**

覆盖：

```go
func TestProgressWithoutTurnReturnsIdle(t *testing.T)
func TestProgressOptionalTurnIDValidatesWhenPresent(t *testing.T)
func TestHandlerCancellationCommitsInterruptedSnapshot(t *testing.T)
func TestInterruptCancelFailureStillReturnsInterrupted(t *testing.T)
func TestCompletionAndInterruptProduceOneTerminalState(t *testing.T)
func TestPermissionEventsDoNotCrossSessions(t *testing.T)
```

其中 Cancel failure 测试断言 `Interrupt` 返回 `session.TurnInterrupted`、Session 回到 idle、Warn 可注入 logger 捕获，且后续 `Progress` 返回同一快照。

- [ ] **Step 2: 运行失败测试**

Run:

```bash
go test ./internal/instance -run 'TestProgress|TestHandlerCancellation|TestInterrupt|TestCompletion|TestPermission' -count=1
```

Expected: FAIL，Turn 操作尚未实现。

- [ ] **Step 3: 实现每 Turn 单 controller**

`Manager.Chat` 在 Session 锁内注册新 Turn 后启动 controller。controller 是以下事件的唯一消费者：

```go
select {
case result := <-promptResult:
	instance.finishTurn(session, turn, result)
case permission := <-client.PermissionEvents(session.AgentSessionID()):
	instance.requirePermission(session, turn, permission)
case <-instance.ctx.Done():
	return
}
```

MCP 等待方只调用 `turn.Wait(ctx, timeout)` 或 `turn.Snapshot()`，不读取 Client permission channel。Prompt 更新由 `session.UpdateCollector` 转为 domain snapshot。

- [ ] **Step 4: 实现统一中断**

内部 `interrupt(session, turn, reason)`：

1. Peek updates 并构造快照；
2. `turn.Interrupt(snapshot)` 原子抢占终态；
3. Session 回到 idle 并解除权限等待；
4. 调用本地 Prompt cancel；
5. 使用 Instance context 派生短超时 context 发送 ACP Cancel；
6. Cancel 失败只记录 Warn，仍返回 interrupted。

`Manager.Chat`/`Respond` 等待 handler context 取消时持有准确 Turn 指针并调用该方法；已经返回 running 的请求只能通过 `Manager.Interrupt(sessionID, turnID)` 进入。

- [ ] **Step 5: 实现 Progress**

```go
func (m *Manager) Progress(sessionID string, turnID string) (ChatView, error)
```

Session 不存在返回 `ErrSessionNotFound`；无 Turn 且 turnID 为空返回 `StatusIdle`；无 Turn但显式 turnID 返回 `ErrTurnNotFound`；显式 ID 不匹配返回 `ErrTurnMismatch`；查询不修改 `LastUsed`。

- [ ] **Step 6: 运行竞态测试并提交**

Run:

```bash
go test -race ./internal/instance ./internal/session -count=1
```

Expected: PASS。

```bash
git add internal/instance internal/session
git commit -m "feat(instance): 统一轮次执行与中断状态"
```

### Task 6: 将全部 MCP handler 迁移到 Manager

**Files:**
- Modify: `internal/mcp/mcp.go`
- Modify: `internal/mcp/types.go`
- Rewrite: `internal/mcp/handlers.go`
- Rewrite: `internal/mcp/handlers_test.go`
- Delete: `internal/mcp/collector.go`
- Modify: `internal/mcp/mcp_test.go`

**Interfaces:**
- Consumes: `instance.Manager` 的全部工具级方法和 value views
- Changes: `NewServer(cfg, manager)` 或简化为 `NewServer(manager)`
- Produces: 所有 `acp_*` 工具的最终 MCP schema 与结构化输出

- [ ] **Step 1: 编写 MCP 合同失败测试**

覆盖：

```go
func TestAcpChatReturnsQualifiedAgentSessionID(t *testing.T)
func TestAcpProgressWithoutTurnIDReturnsIdle(t *testing.T)
func TestAcpProgressWithWrongTurnIDReturnsMismatch(t *testing.T)
func TestAcpSessionsReturnsSessionIDAndAllItems(t *testing.T)
func TestAcpListHistoryDefaultsToCodexAndQualifiesIDs(t *testing.T)
func TestAcpResumeUsesInactiveQualifiedID(t *testing.T)
func TestAcpLoadAndDeleteDeriveAgentTypeFromQualifiedID(t *testing.T)
```

Schema 测试断言 `acpProgressArgs.TurnID` 带 `omitempty`，`acp_sessions` 无分页字段，`sessionListItem` JSON 字段为 `session_id`。

- [ ] **Step 2: 运行 MCP 测试并确认失败**

Run:

```bash
go test ./internal/mcp -count=1
```

Expected: FAIL，Server 仍依赖 Pool/clients/turns，参数仍为旧合同。

- [ ] **Step 3: 简化 Server 并迁移 handler**

`Server` 只保存：

```go
type Server struct {
	sdkServer *sdk.Server
	manager   *instance.Manager
}
```

每个 handler 校验输入、调用 Manager、把 domain view 映射为现有具体输出类型。删除 MCP 层 `acpClient`、`clientFactory`、`promptTurn` 和全部 turn/client map helper。

- [ ] **Step 4: 修改工具参数**

- `acpProgressArgs`：`SessionID` 必填、`TurnID` 可选；
- `acpListHistoryArgs`：删除 `cwd`，增加可选 `agent_type`；
- `acpLoadSessionArgs`：qualified `session_id` + 可选 `cwd`，删除 `agent_type`；
- `acpDeleteSessionArgs`：只保留 qualified `session_id`；
- `sessionListItem.ID` 改为 `SessionID string json:"session_id"`。

- [ ] **Step 5: 运行 MCP 竞态测试**

Run:

```bash
go test -race ./internal/mcp -count=1
```

Expected: PASS，`rg 'Server\\.turns|Server\\.clients|SessionPool|newSessionID' internal/mcp` 无结果。

- [ ] **Step 6: 提交**

```bash
git add internal/mcp
git commit -m "refactor(mcp): 统一通过实例管理器路由工具"
```

### Task 7: 接入 bridge 关闭流程并同步最终文档

**Files:**
- Modify: `cmd/acp-bridge/main.go`
- Create: `cmd/acp-bridge/main_test.go`
- Modify: `internal/mcp/skill.md`
- Modify: `DESIGN.md`
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: `instance.NewManager`、`Manager.Close`
- Produces: 可返回退出码的 `run()` 和完整 shutdown
- Produces: `runWith(lifecycleManager, runnableServer) error`
- Test helpers: `fakeManager` 实现 `Close(context.Context) error` 并记录 `closeCalls`；`fakeServer` 实现 `Run() error`

- [ ] **Step 1: 编写入口关闭失败测试**

把依赖构造注入 `runWith`，测试 Server.Run 返回错误时仍调用 Manager.Close，且 main 只在 `run()` 返回后调用 `os.Exit`：

```go
func TestRunClosesManagerWhenServerStops(t *testing.T) {
	manager := &fakeManager{}
	err := runWith(manager, fakeServer{err: errors.New("stdio closed")})
	if err == nil || manager.closeCalls != 1 {
		t.Fatalf("err=%v closeCalls=%d", err, manager.closeCalls)
	}
}
```

- [ ] **Step 2: 实现入口所有权**

`run()` 加载配置、创建 Manager 和 Server，并 `defer manager.Close(shutdownCtx)`；`main()` 只负责日志初始化和根据 `run()` 错误决定退出码。正常 EOF 与错误路径都执行关闭。

- [ ] **Step 3: 删除旧配置与同步文档**

- 删除 `ACP_BRIDGE_SESSION_TTL` 及测试；
- 更新内嵌 skill：qualified Session ID、`acp_sessions()`、`acp_progress(session_id)`、显式 interrupt；
- 更新 `DESIGN.md`：Manager/Instance/Client/Process 所有权、永久 Session、容量拒绝、关闭流程；
- 更新 `AGENTS.md` 中 `internal/session` 和配置描述，删除空闲清理与会话 TTL。

- [ ] **Step 4: 运行全量验证**

Run:

```bash
task check
go test -race ./...
rg 'ACP_BRIDGE_SESSION_TTL|cleanupLoop|evictOldestLocked|newSessionID|Server\\.turns|Server\\.clients' --glob '*.go'
```

Expected: `task check` 和 race PASS；最后一条 `rg` 无结果。

- [ ] **Step 5: 提交**

```bash
git add cmd internal DESIGN.md AGENTS.md
git commit -m "feat: 完成实例与会话生命周期重构"
```

### Task 8: 最终合同回归与差异审查

**Files:**
- Verify: `docs/superpowers/specs/2026-07-29-agent-instance-lifecycle-design.md`
- Verify: `docs/superpowers/specs/2026-07-29-acp-progress-session-status-design.md`
- Verify: all modified Go files

**Interfaces:**
- Verifies: 两份设计的全部外部合同与内部所有权

- [ ] **Step 1: 运行合同矩阵**

```bash
go test -race ./... -count=1
go vet ./...
task build
```

Expected: 全部退出码为 0。

- [ ] **Step 2: 检查仓库差异**

```bash
git status --short
git diff --check
git log --oneline -8
```

确认没有临时文件、旧 bridge Session ID 生成器、TTL/LRU 清理代码或未提交改动。

- [ ] **Step 3: 对照设计逐项确认**

- 每种 agent 同时最多一个 Client/Process；
- 第 11 个 Session 被拒绝且前 10 个仍存在；
- Session 零空闲淘汰；
- agent 退出清空自身 Session，其他实例不受影响；
- handler 取消保留 interrupted 快照；
- `acp_progress(session_id)` 返回 idle/running/permission/completed/interrupted；
- `acp_sessions()` 返回全部活跃 qualified ID；
- bridge 退出回收全部子进程。

- [ ] **Step 4: 如有最终修订则提交**

```bash
git add -A
git commit -m "test: 补充实例生命周期合同回归"
```

若无差异则不创建空提交。
