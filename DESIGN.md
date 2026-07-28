# acp-bridge Go 重构设计文档

> 用 Go 重写 Hermes Agent 的 acp-bridge 插件，将 ACP（Agent Client Protocol）桥接为 MCP（Model Context Protocol）服务器，支持 codex-acp / claude-agent-acp / gemini-agent-acp 三种后端 agent。

---

## 目录

1. [架构总览](#1-架构总览)
2. [组件详述](#2-组件详述)
3. [数据流](#3-数据流)
4. [权限交互](#4-权限交互)
5. [AgentDriver 接口](#5-agentdriver-接口)
6. [错误处理](#6-错误处理)
7. [配置与部署](#7-配置与部署)
8. [测试策略](#8-测试策略)
9. [与 Python 版的关键差异](#9-与-python-版的关键差异)

---

## 1. 架构总览

### 1.1 定位

acp-bridge 是 Hermes Agent 与 ACP 兼容 agent 之间的桥梁。它作为一个独立的 MCP 服务器进程运行，通过 stdio 与 Hermes 通信，将 Hermes 的 MCP 工具调用翻译成 ACP JSON-RPC 2.0 请求，再通过另一个 stdio 管道与 agent 子进程交互。

### 1.2 系统架构图

```
┌────────────────────────────────────────────────────────────┐
│                     Hermes Agent                           │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────────────┐  │
│  │  Tools    │  │  Tool Runner │  │  mcp_servers         │  │
│  │  (TUI)    │──│  (Python)    │──│  acp-bridge          │  │
│  └──────────┘  └──────────────┘  │  /path/to/acp-bridge │  │
│                                  └─────────┬────────────┘  │
└────────────────────────────────────────────┼────────────────┘
                                             │ MCP stdio (JSON-RPC)
                                             ▼
┌────────────────────────────────────────────────────────────┐
│                    acp-bridge (Go)                          │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │                  pkg/mcp/                            │   │
│  │   ┌──────────────┐  ┌──────────────────────────┐    │   │
│  │   │  MCP Server   │  │   Tool Registration      │    │   │
│  │   │  (stdio I/O)  │  │   acp_chat, acp_respond, │    │   │
│  │   │               │  │   acp_interrupt, ...     │    │   │
│  │   └──────┬───────┘  └──────────────────────────┘    │   │
│  └──────────┼──────────────────────────────────────────┘   │
│             │                                                │
│  ┌──────────▼──────────────────────────────────────────┐   │
│  │                  pkg/client/                         │   │
│  │   ┌──────────────────────────────────────────────┐  │   │
│  │   │  ACP Client (acp.ClientSideConnection)        │  │   │
│  │   │  初始化 → 会话创建 → Prompt → 通知消费        │  │   │
│  │   └──────────────────────────────────────────────┘  │   │
│  └─────────────────────────────────────────────────────┘   │
│             │                                                │
│  ┌──────────▼──────────────────────────────────────────┐   │
│  │                  pkg/driver/                         │   │
│  │   ┌──────────────┐ ┌──────────────┐ ┌────────────┐ │   │
│  │   │ CodexDriver  │ │ ClaudeDriver │ │GeminiDriver│ │   │
│  │   │              │ │              │ │            │ │   │
│  │   │acp.NewAgent- │ │acp.NewAgent- │ │acp.NewAgent│ │   │
│  │   │SideConnection│ │SideConnection│ │SideConnect.│ │   │
│  │   └──────┬───────┘ └──────┬───────┘ └─────┬──────┘ │   │
│  └──────────┼────────────────┼────────────────┼────────┘   │
│             │                │                │            │
│  ┌──────────▼────────────────▼────────────────▼────────┐   │
│  │                  pkg/session/                        │   │
│  │   ┌──────────────────────────────────────────────┐  │   │
│  │   │   SessionPool                                 │  │   │
│  │   │   映射 sessionId → Session 状态               │  │   │
│  │   │   并发安全 (sync.RWMutex)                     │  │   │
│  │   │   定期清理过期会话                             │  │   │
│  │   └──────────────────────────────────────────────┘  │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │                  pkg/config/                         │   │
│  │   ┌──────────────────────────────────────────────┐  │   │
│  │   │  环境变量加载, Agent 路径解析, 默认值       │  │   │
│  │   └──────────────────────────────────────────────┘  │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘
             │  ACP stdio (JSON-RPC 2.0)
             ▼
┌────────────────────────────────────────────────────────────┐
│                   Agent 子进程                               │
│  ┌──────────────────┐  ┌──────────────────┐  ┌──────────┐  │
│  │  codex-acp       │  │  claude-agent-acp│  │gemini-   │  │
│  │  (npx ...)       │  │  (pip ...)       │  │agent-acp │  │
│  └──────────────────┘  └──────────────────┘  └──────────┘  │
└────────────────────────────────────────────────────────────┘
```

### 1.3 通信协议栈

```
┌─────────────────────────────────────────────┐
│  Layer 4: MCP JSON-RPC 2.0 (Hermes ←→ 桥)  │
│   工具调用: acp_chat, acp_respond, ...       │
├─────────────────────────────────────────────┤
│  Layer 3: 桥内部翻译层                       │
│   MCP ToolCall → ACP Request                │
│   ACP Notification → MCP ToolResult         │
├─────────────────────────────────────────────┤
│  Layer 2: ACP JSON-RPC 2.0 (桥 ←→ Agent)   │
│   方法: initialize, session/new,            │
│         session/prompt, session/close       │
├─────────────────────────────────────────────┤
│  Layer 1: stdio 管道                         │
│   桥的 stdin/stdout ↔ Agent 的 stdout/stdin │
└─────────────────────────────────────────────┘
```

### 1.4 关键设计决策

| 维度 | 决策 | 理由 |
|------|------|------|
| 集成方式 | 独立 MCP 服务器进程 | 与 Hermes 进程解耦，可独立升级，不影响主进程稳定性 |
| 协议 | MCP stdio transport | 零网络配置，Hermes 原生支持，安全性高 |
| SDK | `github.com/coder/acp-go-sdk` | 官方 Go SDK，自动处理 JSON-RPC 2.0 消息帧、请求关联、通知队列 |
| Agent 生命周期 | 按需启动子进程 | 每个 agent 类型一个子进程，lazy 初始化，使用后保持存活直到超时 |
| 权限模型 | MCP 请求—响应 + 同步等待 | 替代 Python 版本的 signal_queue，纯 Go channel 实现 |

---

## 2. 组件详述

### 2.1 `cmd/acp-bridge/` — 入口

**职责**: 解析启动参数，加载配置，启动 MCP 服务器。

```go
func main() {
    cfg := config.Load()
    pool := session.NewPool()
    mcpServer := mcp.NewServer(cfg, pool)
    if err := mcpServer.Run(os.Stdin, os.Stdout); err != nil {
        log.Fatal(err)
    }
}
```

**关键行为**:
- 从环境变量加载配置（见 §7）
- 初始化 SessionPool（空池，lazy 创建 agent 子进程）
- 启动 MCP Server，在 stdio 上监听 JSON-RPC 消息
- 所有 agent 子进程在 `SessionPool` 内部按需创建

### 2.2 `pkg/config/` — 配置

**职责**: 集中管理所有配置项。

```go
type Config struct {
    // Agent 路径配置
    CodexPath       string   // codex-acp 可执行路径 (默认: "npx @agentclientprotocol/codex-acp")
    ClaudeAgentPath string   // claude-agent-acp 路径
    GeminiAgentPath string   // gemini-agent-acp 路径

    // 行为配置
    DefaultTimeout  Duration // prompt 默认超时 (默认: 300s)
    SessionTTL      Duration // 会话空闲存活时间 (默认: 1800s = 30分钟)
    MaxSessions     int      // 最大并发会话数 (默认: 10)

    // 调试
    LogLevel        string   // debug/info/warn/error (默认: info)
    LogFormat       string   // text/json
}
```

**实现**:
- 使用 `os.Getenv` 读取，前缀 `ACP_BRIDGE_`
- 所有字段有合理的默认值
- 在 `Load()` 时验证路径是否可执行（如果指定了显式路径）
- 支持 `ACP_BRIDGE_<AGENT>_ARGS` 传递额外启动参数

### 2.3 `pkg/client/` — ACP 客户端

**职责**: 封装 `acp-go-sdk` 的 `ClientSideConnection`，提供 acp-bridge 内部使用的高级接口。

```go
// Client 是对 acp.ClientSideConnection 的高层封装
type Client struct {
    conn    *acp.ClientSideConnection
    agent   acp.Agent // AgentSideConnection 的 agent 接口实现
    mu      sync.Mutex
    session *acp.SessionState
}

// New 启动 agent 子进程并建立 ACP 连接
func New(ctx context.Context, cmd *exec.Cmd) (*Client, error)

// Initialize 握手并协商协议版本
func (c *Client) Initialize(ctx context.Context) (*acp.InitializeResponse, error)

// NewSession 创建新会话
func (c *Client) NewSession(ctx context.Context, cwd string, mcpServers []string) (string, error)

// Prompt 发送 prompt 并流式接收响应
// 返回一个用于消费通知的 channel
func (c *Client) Prompt(ctx context.Context, sessionID string, prompt []acp.ContentBlock) (<-chan acp.SessionNotification, error)

// Close 关闭连接
func (c *Client) Close() error
```

**底层使用**:

```go
// 示例：启动 codex-acp 子进程
cmd := exec.CommandContext(ctx, "npx", "@agentclientprotocol/codex-acp")
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()

// 创建 ClientSideConnection
clientImpl := &myACPClientImpl{ /* 处理 agent 发起的请求 */ }
conn := acp.NewClientSideConnection(clientImpl, stdout, stdin)
```

**agent 发起的请求处理（Client 接口实现）**:

```go
type acpClientHandler struct {
    // 保存对 MCP server 的引用，以便转发权限请求
    mcpServer  *mcp.Server
    // 权限请求的 channel，用于同步等待用户响应
    permissionCh chan permissionRequest
}

// 实现 acp.Client 接口（关键方法）:
func (h *acpClientHandler) RequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
    // 通过 MCP 向 Hermes 发送权限请求
    // 同步等待用户确认后返回
}

func (h *acpClientHandler) ReadTextFile(ctx context.Context, req acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
    // 通过 MCP 让 Hermes 提供文件内容
}

func (h *acpClientHandler) WriteTextFile(ctx context.Context, req acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
    // 通过 MCP 让 Hermes 写入文件
}

func (h *acpClientHandler) SessionNotification(ctx context.Context, req acp.SessionNotification) error {
    // 流式通知：转发到 Prompt 的 channel
}
```

### 2.4 `pkg/driver/` — Agent 驱动

**职责**: 定义 `AgentDriver` 接口，统一三种 agent 的启动和管理逻辑。

详见 [§5 AgentDriver 接口](#5-agentdriver-接口)。

### 2.5 `pkg/mcp/` — MCP 服务器

**职责**: 实现 MCP 协议服务器，注册 acp-bridge 提供的所有工具。

**工具清单**:

| MCP 工具名 | 对应 ACP 操作 | 描述 |
|-----------|--------------|------|
| `acp_chat` | `session/new` + `session/prompt` | 发送 prompt 到 agent |
| `acp_respond` | `session/request_permission` 的响应 | 回复 agent 的权限请求 |
| `acp_interrupt` | `session/cancel` (notification) | 按 session_id + turn_id 中断正在进行的 prompt |
| `acp_close` | `session/close` | 关闭会话 |
| `acp_sessions` | `session/list` (unstable) | 列出活跃会话 |
| `acp_set_mode` | `session/set_mode` | 设置会话权限模式 |
| `acp_set_config` | `session/set_config_option` | 设置会话配置选项 |
| `acp_fork_session` | `session/fork` (unstable) | 分支会话 |
| `acp_load_session` | `session/load` | 加载持久化会话 |
| `acp_list_history` | — | 列出历史会话（通过 agent 自有能力） |
| `acp_resume_session` | `session/resume` (unstable) | 恢复暂停的会话 |
| `acp_delete_session` | — | 删除持久化会话 |

**核心类型**:

```go
// Server 是 MCP 服务器
type Server struct {
    config *config.Config
    pool   *session.Pool
    logger *slog.Logger
}

// 工具注册
func (s *Server) RegisterTools(ctx context.Context) {
    // 每个工具对应一个 handlers.CallTool 回调
    // 使用 MCP 协议的 Tool 结构体定义参数 schema
}

// Run 监听 stdio，分派请求到对应 handler
func (s *Server) Run(stdin io.Reader, stdout io.Writer) error
```

**MCP 请求分发**:

```go
// 伪代码：MCP JSON-RPC 处理循环
for {
    msg, err := readMessage(stdin)
    switch msg.Method {
    case "tools/list":
        respond(stdout, listTools())
    case "tools/call":
        result := handleToolCall(msg.Params)
        respond(stdout, result)
    // 其他 MCP 方法...
    }
}
```

### 2.6 `pkg/session/` — 会话池

**职责**: 管理所有活跃 ACP 会话的生命周期。

```go
type Session struct {
    ID        string
    AgentType string       // "codex", "claude", "gemini"
    CWD       string
    Client    *client.Client
    CreatedAt time.Time
    LastUsed  time.Time
    Status    SessionStatus // active, paused, closed
    Config    map[string]string
    Mode      string
    // Prompt 进行中的取消函数
    Cancel    context.CancelFunc
}

type Pool struct {
    mu       sync.RWMutex
    sessions map[string]*Session
    config   *config.Config
    // agent 子进程复用池（按 agent 类型聚合）
    agentPool map[string]*client.Client
}
```

**关键行为**:

1. **获取会话** — `GetOrCreate(ctx, agentType, cwd)`: 如果有空闲的 agent 子进程则复用，否则启动新的
2. **会话复用** — 同一 agent 类型共享一个子进程，通过 `session/new` 创建独立上下文
3. **空闲清理** — 后台 goroutine 定期（每 60s）扫描 `LastUsed > SessionTTL` 的会话并关闭
4. **并发控制** — 写操作加写锁，读操作加读锁
5. **取消传播** — `Cancel()` 通过 context 取消正在进行的 Prompt

---

## 3. 数据流

### 3.1 `acp_chat` 全生命周期

以下是一个完整的 `acp_chat` 调用从创建到完成的时序图：

```
Hermes                     acp-bridge                         Agent 子进程
  │                           │                                  │
  │  1. tools/call            │                                  │
  │  {name: "acp_chat",       │                                  │
  │   args: {prompt, cwd}}    │                                  │
  │ ─────────────────────────►│                                  │
  │                           │                                  │
  │                           │  2. 检查 session pool            │
  │                           │     没有活跃会话 → 创建新会话    │
  │                           │                                  │
  │                           │  3. 若 agent 子进程未启动        │
  │                           │     exec agent 子进程            │
  │                           │ ────────────────────────────────►│
  │                           │                                  │
  │                           │  4. ACP initialize               │
  │                           │  {method: "initialize",          │
  │                           │   params: {protocolVersion: 1}}  │
  │                           │ ────────────────────────────────►│
  │                           │◄─────────────────────────────────│
  │                           │  {result: {protocolVersion: 1,   │
  │                           │   agentInfo: {...}}}             │
  │                           │                                  │
  │                           │  5. ACP session/new              │
  │                           │  {method: "session/new",         │
  │                           │   params: {cwd, mcpServers: []}} │
  │                           │ ────────────────────────────────►│
  │                           │◄─────────────────────────────────│
  │                           │  {result: {sessionId: "sid_1"}}  │
  │                           │                                  │
  │                           │  6. ACP session/prompt           │
  │                           │  {method: "session/prompt",      │
  │                           │   params: {sessionId: "sid_1",   │
  │                           │   prompt: [{type:"text",          │
  │                           │             text:"..."}]}}       │
  │                           │ ────────────────────────────────►│
  │                           │                                  │
  │     ┌─── 流式通知 ───┐    │                                  │
  │     │                │    │  7a. session/update              │
  │     │   MCP progress │    │  (agent_message_chunk)           │
  │     │◄───────────────│────│◄─────────────────────────────────│
  │     │◄───────────────│────│◄─────────────────────────────────│
  │     │                │    │                                  │
  │     │   (agent 需要  │    │  7b. session/request_permission  │
  │     │    用户确认)   │    │  {method: "session/request_      │
  │     │                │    │   permission",                   │
  │     │   tools/call   │    │   params: {promptId, toolCall}}  │
  │     │◄───────────────│────│◄─────────────────────────────────│
  │     │                │    │                                  │
  │     │  8. 用户通过    │    │                                  │
  │     │  acp_respond   │    │                                  │
  │     │  返回结果       │    │                                  │
  │     │ ───────────────►│    │                                  │
  │     │                │    │  9. ACP RequestPermission 响应   │
  │     │                │    │  {id: <req_id>, result: {        │
  │     │                │    │   outcome: "selected"}}          │
  │     │                │    │ ────────────────────────────────►│
  │     │                │    │                                  │
  │     │                │    │  10. 继续流式 response            │
  │     │◄───────────────│────│◄─────────────────────────────────│
  │     │◄───────────────│────│◄─────────────────────────────────│
  │     │                │    │                                  │
  │     │                │    │  11. Prompt 完成 (stopReason)     │
  │     │◄───────────────│────│◄─────────────────────────────────│
  │     │                │    │  {result: {stopReason: "endTurn"}}│
  │     │                │    │                                  │
  │  12. 返回最终结果     │    │                                  │
  │◄─────────────────────────│                                  │
  │                           │                                  │
```

### 3.2 详细步骤分析

**Step 1-2: 入口与会话查找**
- Hermes 通过 MCP stdio 调用 `tools/call {name: "acp_chat", arguments: {prompt, session_id?, cwd?, agent_type?}}`。
- 如果提供了 `session_id`，查找已有会话；否则从 `SessionPool.GetOrCreate()` 创建新会话。

**Step 3-4: Agent 子进程初始化**
- 如果是该 agent 类型的首次调用，启动子进程（`exec.Command`）。
- 执行 ACP `initialize` 握手，协商协议版本和 capabilities。
- 注意：agent 子进程启动后保持存活，后续同一类型的会话复用该进程。

**Step 5: 会话创建**
- 发送 `session/new`，传递 `cwd` 和当前 Hermes 的 MCP 服务器列表。
- Agent 返回 `sessionId`，在池中创建 `Session` 对象。

**Step 6: Prompt**
- 发送 `session/prompt`，prompt 格式为 `[{type: "text", text: "用户消息"}]`。
- 注意：字段名是 `prompt` 而不是 `messages`（ACP 协议规范）。

**Step 7a: 流式通知**
- Agent 通过 `session/update` 通知发送消息块、thought 块、工具调用等。
- acp-bridge 将这些通知转发为 MCP `progress` 通知或累积到最终结果中。
- `session/update` 的 `params.update` 结构体包含 `sessionUpdate` 字段（如 `agent_message_chunk`, `agent_thought_chunk`, `tool_call` 等）。

**Step 7b-9: 权限交互**
- 当 agent 需要用户确认时（如执行 shell 命令），发送 `session/request_permission`。
- acp-bridge 暂停当前的 prompt 消费，通过 MCP 向 Hermes 返回一个特殊状态。
- 用户通过 `acp_respond` 工具调用回复。
- 详见 [§4 权限交互](#4-权限交互)。

**Step 10-12: 完成与返回**
- Agent 发送 `stopReason` 表示 prompt 完成。
- acp-bridge 将所有累积的消息和最终结果打包，通过 MCP `tools/call` 响应返回。
- 会话保持在池中，供后续 `acp_chat` 调用复用。

### 3.3 注意点

- **第一次 prompt 冷启动延迟**：Agent（特别是 codex-acp）首次 prompt 需要 5-60 秒初始化 app-server。后续 prompt 仅需 1-5 秒。
- **流式 vs 块式**：acp-bridge 在内部收集所有通知后再一次性返回给 Hermes（类似 Python 版本的块式返回），以避免 MCP stdio 上的流式复杂性。
- **超时**：每个 prompt 受 `ACP_BRIDGE_DEFAULT_TIMEOUT` 控制；单轮等待超时返回 `running`，由调用方携带 `session_id + turn_id` 继续查询或中断。

---

## 4. 权限交互

### 4.1 问题描述

Python 版本使用 `signal_queue`（一个跨进程信号队列）来实现权限请求的同步等待。在 Go 版中，我们需要一个纯 Go 的等价方案。

ACP 协议中，当 agent 需要用户批准时：
1. Agent 发送 `session/request_permission` 请求（Agent → Client，这是一个**请求**，不是通知，需要响应）
2. Client 必须返回 `RequestPermissionResponse` 来继续或拒绝

### 4.2 Go 实现方案：Channel 同步

核心思路：使用 Go channel 实现同步等待，MCP 工具调用作为异步交互通道。

```go
// acpClientHandler 实现 acp.Client 接口
type acpClientHandler struct {
    // 按照 promptId 索引的权限请求 channel
    permissions map[string]chan permissionResult
    mu          sync.Mutex
    mcpServer   *mcp.Server
}

type permissionResult struct {
    Outcome acp.RequestPermissionOutcome
    Err     error
}
```

**流程**:

```
Agent                        acp-bridge                      Hermes
  │                              │                              │
  │  1. request_permission       │                              │
  │  {promptId, toolCall}        │                              │
  │ ───────────────────────────► │                              │
  │                              │                              │
  │                              │  2. 创建 channel             │
  │                              │     ch := make(chan result)  │
  │                              │     permissions[id] = ch ←───┘
  │                              │                              │
  │                              │  3. MCP tools/call 返回      │
  │                              │     特殊标记，表示需要权限   │
  │                              │     {status: "pending",      │
  │                              │      requestId: "perm_1",    │
  │                              │      toolCall: {...}}        │
  │                              │ ────────────────────────────►│
  │                              │                              │
  │                              │  4. 用户调用 acp_respond     │
  │                              │     {requestId, outcome}     │
  │◄─────────────────────────────│──────────────────────────────│
  │                              │                              │
  │                              │  5. ch <- result             │
  │                              │     ←─── 解除阻塞            │
  │                              │                              │
  │  6. 响应 request_permission  │                              │
  │  {outcome: "selected"}       │                              │
  │◄───────────────────────────  │                              │
  │                              │                              │
  │  7. 继续生成 prompt 输出     │                              │
```

### 4.3 超时与取消

```go
// 带超时的权限等待
ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
defer cancel()

select {
case result := <-ch:
    return result.Outcome, result.Err
case <-ctx.Done():
    // 超时或用户取消
    delete(h.permissions, id)
    return acp.NewRequestPermissionOutcomeCancelled(), ctx.Err()
}
```

### 4.4 多个并发权限请求

ACP 协议允许 agent 在等待一个权限请求的同时发起其他请求。实现需要处理：

```go
// 每个权限请求独立一个 channel
// promptId 作为唯一键，防止冲突
// 清理：prompt 完成后清理所有关联的 pending 权限
func (h *acpClientHandler) CleanupSession(sessionID string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    for id, ch := range h.permissions {
        if strings.HasPrefix(id, sessionID+":") {
            close(ch)
            delete(h.permissions, id)
        }
    }
}
```

### 4.5 与 Python signal_queue 的对比

| 维度 | Python 版 (signal_queue) | Go 版 (channel) |
|------|--------------------------|------------------|
| 跨进程通信 | Unix signal + Queue | 同进程内 channel |
| 阻塞机制 | `signal_queue.get()` 阻塞 | `<-ch` 阻塞 |
| 超时 | `signal_queue.get(timeout=...)` | `select { case <-ch: ... case <-ctx.Done(): ... }` |
| 取消 | signal 中断 | `context.WithCancel` |
| 并发安全 | 需自行加锁 | channel 原生安全 |
| 资源泄露风险 | 进程退出时自动清理 | goroutine 泄漏需小心关闭 channel |

---

## 5. AgentDriver 接口

### 5.1 接口定义

```go
package driver

import (
    "context"
    "io"

    "github.com/coder/acp-go-sdk"
)

// AgentType 标识 agent 类型
type AgentType string

const (
    AgentTypeCodex   AgentType = "codex"
    AgentTypeClaude  AgentType = "claude"
    AgentTypeGemini  AgentType = "gemini"
)

// AgentDriver 统一三种 agent 的启动入口
type AgentDriver interface {
    // Type 返回 agent 类型标识
    Type() AgentType

    // Start 启动 agent 子进程
    // 返回 agent 的 stdout/stderr 读取器和 stdin 写入器
    Start(ctx context.Context) (stdout io.ReadCloser, stdin io.WriteCloser, stderr io.ReadCloser, err error)

    // Capabilities 返回该 agent 支持的 ACP capabilities
    Capabilities() AgentCapabilities
}

// AgentCapabilities 描述 agent 能力
type AgentCapabilities struct {
    SupportsLoadSession   bool
    SupportsFork          bool
    SupportsResume        bool
    SupportsListSessions  bool
    SupportsSetModel      bool
    SupportsAuthenticate  bool
    // 其他 ACP capabilities
}
```

### 5.2 三种实现

#### CodexDriver

```go
type CodexDriver struct {
    path string   // "npx @agentclientprotocol/codex-acp"
    args []string // 额外参数
}

func (d *CodexDriver) Start(ctx context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
    cmd := exec.CommandContext(ctx, "npx", append([]string{
        "@agentclientprotocol/codex-acp",
    }, d.args...)...)
    // 设置 stdio pipes
    stdout, _ := cmd.StdoutPipe()
    stdin, _ := cmd.StdinPipe()
    stderr, _ := cmd.StderrPipe()
    if err := cmd.Start(); err != nil {
        return nil, nil, nil, fmt.Errorf("start codex-acp: %w", err)
    }
    return stdout, stdin, stderr, nil
}
```

#### ClaudeDriver

```go
type ClaudeDriver struct {
    path string   // "claude-agent-acp" 可执行路径
    args []string
}

// 启动方式：直接执行 claude-agent-acp 二进制
// 或者通过 pip 安装后的入口点
```

#### GeminiDriver

```go
type GeminiDriver struct {
    path string
    args []string
}

// 启动方式：执行 gemini-agent-acp 二进制
```

### 5.3 工厂函数

```go
// NewDriver 根据类型创建对应的 AgentDriver
func NewDriver(agentType AgentType, cfg *config.Config) (AgentDriver, error) {
    switch agentType {
    case AgentTypeCodex:
        return &CodexDriver{
            path: cfg.CodexPath,
            args: cfg.CodexArgs,
        }, nil
    case AgentTypeClaude:
        return &ClaudeDriver{
            path: cfg.ClaudeAgentPath,
            args: cfg.ClaudeAgentArgs,
        }, nil
    case AgentTypeGemini:
        return &GeminiDriver{
            path: cfg.GeminiAgentPath,
            args: cfg.GeminiAgentArgs,
        }, nil
    default:
        return nil, fmt.Errorf("unknown agent type: %s", agentType)
    }
}
```

### 5.4 统一错误处理

```go
// StartError 包含启动失败的详细信息
type StartError struct {
    AgentType AgentType
    Path      string
    ExitCode  int
    Stderr    string
    Err       error
}

func (e *StartError) Error() string {
    return fmt.Sprintf("failed to start %s agent (path=%s, exit=%d): %s\nstderr: %s",
        e.AgentType, e.Path, e.ExitCode, e.Err, e.Stderr)
}
```

---

## 6. 错误处理

### 6.1 错误分类

| 错误类别 | 示例 | 处理策略 |
|---------|------|---------|
| 配置错误 | 环境变量无效、路径不存在 | 启动时立即失败，输出清晰的错误信息 |
| Agent 启动失败 | 二进制不存在、权限不足 | 返回 MCP 工具调用错误，包含 agent 的 stderr |
| ACP 协议错误 | 版本不匹配、方法不支持 | 断开连接，清理会话，返回错误给 Hermes |
| 会话错误 | sessionId 无效、会话已关闭 | 返回 "session not found" 错误；销毁旧会话 |
| Prompt 错误 | agent 内部错误、超时 | 取消 prompt，关闭会话，返回错误信息 |
| 权限超时 | 用户 5 分钟内未响应 | 自动拒绝（返回 cancelled），清理权限 channel |
| 内部错误 | channel 泄漏、goroutine panic | panic 恢复机制，记录堆栈，保持服务器存活 |

### 6.2 错误传播链

```
Agent 进程 (ACP error)
  → acp.Client (SDK 层 JSON-RPC error)
    → pkg/client (协议错误包装)
      → pkg/mcp (MCP tool error response)
        → Hermes (用户可见的错误消息)
```

### 6.3 ACP JSON-RPC 错误码

```go
const (
    // 标准 JSON-RPC 错误码
    ErrParse          = -32700
    ErrInvalidRequest = -32600
    ErrMethodNotFound = -32601
    ErrInvalidParams  = -32602
    ErrInternal       = -32603

    // ACP 自定义错误码 (范围 -32000 到 -32099)
    ErrSessionNotFound = -32000
    ErrPromptCancelled = -32001
    ErrPermissionDenied = -32002
    ErrAgentBusy       = -32003
    ErrNotSupported    = -32004
)
```

### 6.4 MCP 错误响应格式

```json
{
    "jsonrpc": "2.0",
    "id": 1,
    "error": {
        "code": -32000,
        "message": "session not found",
        "data": {
            "session_id": "sid_invalid",
            "agent_type": "codex",
            "hint": "use acp_sessions to list active sessions"
        }
    }
}
```

### 6.5 Panic 恢复

每个 MCP 请求 handler 应包裹 panic recovery：

```go
func (s *Server) handleToolCall(req Request) (result interface{}, err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("internal error: %v", r)
            s.logger.Error("panic recovered", "panic", r, "stack", debug.Stack())
        }
    }()
    // ... handler logic
}
```

---

## 7. 配置与部署

### 7.1 环境变量

| 变量名 | 默认值 | 描述 |
|--------|--------|------|
| `ACP_BRIDGE_CODEX_PATH` | `npx @agentclientprotocol/codex-acp` | codex-acp 启动命令 |
| `ACP_BRIDGE_CLAUDE_AGENT_PATH` | `claude-agent-acp` | claude-agent-acp 路径 |
| `ACP_BRIDGE_GEMINI_AGENT_PATH` | `gemini-agent-acp` | gemini-agent-acp 路径 |
| `ACP_BRIDGE_CODEX_ARGS` | `""` | codex-acp 额外启动参数 |
| `ACP_BRIDGE_DEFAULT_TIMEOUT` | `300s` | prompt 默认超时 |
| `ACP_BRIDGE_SESSION_TTL` | `1800s` | 会话空闲超时 |
| `ACP_BRIDGE_MAX_SESSIONS` | `10` | 最大并发会话数 |
| `ACP_BRIDGE_LOG_LEVEL` | `info` | 日志级别 |
| `ACP_BRIDGE_LOG_FORMAT` | `text` | 日志格式 (text/json) |

### 7.2 Hermes `mcp_servers` 配置示例

```yaml
# ~/.hermes/config.yaml 或 profile 配置
mcp_servers:
  acp-bridge:
    transport: stdio
    command: /usr/local/bin/acp-bridge
    args: []
    env:
      ACP_BRIDGE_LOG_LEVEL: debug
      ACP_BRIDGE_DEFAULT_TIMEOUT: 600s
      ACP_BRIDGE_SESSION_TTL: 900s
```

### 7.3 构建与部署

```makefile
# Makefile (项目根目录)
BINARY_NAME = acp-bridge
GO = go

.PHONY: build install clean

build:
	$(GO) build -o $(BINARY_NAME) ./cmd/acp-bridge

install: build
	cp $(BINARY_NAME) /usr/local/bin/

clean:
	rm -f $(BINARY_NAME)

test:
	$(GO) test ./... -v -race

lint:
	golangci-lint run ./...
```

**Go 版本要求**: Go 1.22+（使用 `slog` 包和泛型）

**依赖**:
```
github.com/coder/acp-go-sdk@v0.13.5
```

### 7.4 启动流程

```
启动 → 加载配置 → 初始化日志 → 启动 MCP Server →
等待 stdio 输入 → 按需启动 agent 子进程 → 处理请求 → 退出
```

### 7.5 Skill 与 MCP 同源（不再单独分发 skills 目录）

acp-bridge 的 skill（告诉宿主模型「何时使用 acp_* 工具」）通过 MCP Resource 直接暴露，与 MCP 工具共用同一条 stdio 连接。宿主侧的 OrchestratorSkillProvider 经标准 `resources/list` → `resources/read` 即可发现并加载，不需要 Hermes 的 `skills.external_dirs`、软链或仓库内 `skills/` 目录。

| 来源 | 文件 | 作用 |
|------|------|------|
| skill 正文 | `internal/mcp/skill.md` | 面向宿主模型的 SKILL.md 原文（含 frontmatter） |
| 注册逻辑 | `internal/mcp/skill.go` | `//go:embed` 内联 + 注册为 MCP Resource |

Resource 元信息：

- URI：`acp-bridge://skill`
- MIME：`text/markdown`
- Description：由 `skill.go` 从 frontmatter `description` 字段解析，与 skill 触发条件一致。

这样设计的好处：skill 与工具天然同源、同版本、同权限边界；二进制升级即 skill 升级，避免仓库里多出一份 skills 目录却因宿主未配置而静默失效。

---

## 8. 测试策略

### 8.1 单元测试

| 包 | 测试重点 | 方法 |
|----|---------|------|
| `pkg/config` | 环境变量解析、默认值、边界值 | `t.Run` 子测试 + `os.Setenv` 模拟 |
| `pkg/session` | 并发安全、过期清理、最大会话限制 | `testing/synctest` + `time` mock |
| `pkg/driver` | AgentDriver 接口实现、参数构建 | mock `exec.Command` |
| `pkg/client` | 错误包装、连接状态管理 | mock stdio pipe |
| `pkg/mcp` | 工具注册、请求路由 | JSON-RPC 消息模拟 |

### 8.2 集成测试

**a) 模拟 ACP Agent 测试**

使用一个模拟 ACP agent 程序，通过 stdio 与 acp-bridge 交互，验证完整的数据流：

```go
// testhelper/mock_agent.go
// 实现 acp.Agent 接口的最小 mock
// 响应 initialize, session/new, session/prompt
// 支持 session/request_permission 握手

func TestACPChatFullCycle(t *testing.T) {
    // 1. 启动 mock agent (子进程)
    // 2. 启动 acp-bridge (另一个子进程，连接到 mock agent)
    // 3. 通过 MCP stdio 发送 acp_chat 请求
    // 4. 验证响应包含预期的消息
    // 5. 验证 session 状态正确
}
```

**b) 权限交互测试**

```go
func TestPermissionFlow(t *testing.T) {
    // 1. 启动 mock agent，配置为触发 request_permission
    // 2. 验证 MCP 返回 "pending" 状态
    // 3. 通过 acp_respond 回复
    // 4. 验证 agent 收到响应并继续
    // 5. 测试超时场景：不回复 → 自动拒绝
}
```

**c) 并发测试**

```go
func TestConcurrentSessions(t *testing.T) {
    // 1. 启动多个并发的 acp_chat 调用
    // 2. 验证 SessionPool 正确管理并发
    // 3. 验证不超过 MaxSessions
}
```

### 8.3 端到端测试（需要真实 agent）

```bash
# 使用真实 codex-acp 的端到端测试
make e2e-test

# 仅启动桥接服务器（手动测试）
ACP_BRIDGE_LOG_LEVEL=debug acp-bridge
```

### 8.4 测试目录结构

```
acp-bridge/
├── pkg/
│   ├── config/
│   │   └── config_test.go
│   ├── client/
│   │   └── client_test.go
│   ├── driver/
│   │   └── driver_test.go
│   ├── session/
│   │   └── session_test.go
│   └── mcp/
│       └── mcp_test.go
├── testhelper/
│   ├── mock_agent.go
│   └── mock_agent_test.go
├── integration/
│   ├── acp_chat_test.go
│   ├── permission_test.go
│   └── concurrency_test.go
└── e2e/
    └── e2e_test.go
```

### 8.5 测试原则

1. **单元测试不依赖外部进程** — `exec.Command` 通过接口注入 mock
2. **集成测试使用 pipe** — 不需要真实的 TCP 连接
3. **端到端测试标记为 `[e2e]`** — 默认 `go test` 跳过
4. **每个 MCP 工具至少有一个集成测试**
5. **Race condition 测试** — 所有测试添加 `-race` flag

---

## 9. 与 Python 版的关键差异

### 9.1 架构差异

| 维度 | Python 版 | Go 版 |
|------|-----------|-------|
| 运行方式 | Hermes 插件（同进程加载） | 独立 MCP 服务器进程 |
| 通信协议 | Hermes 插件内部 API | MCP stdio JSON-RPC |
| 语言 | Python | Go |
| 依赖管理 | pip/pyproject.toml | go.mod |
| 部署 | 随 Hermes 安装 | 独立编译的二进制 |
| 进程模型 | 与 Hermes 同进程 | 独立进程，按需启动 agent 子进程 |
| 权限交互 | `signal_queue` 跨进程信号 | Go channel 同步 + MCP 工具 |

### 9.2 接口差异

**Python 版接口**（旧）：
```python
# Hermes 插件内部调用
result = await bridge.acp_chat(prompt, session_id=None)
```

**Go 版接口**（新）：
```json
// MCP 工具调用 (通过 stdio)
{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
        "name": "acp_chat",
        "arguments": {
            "prompt": "用户消息",
            "session_id": null,
            "cwd": "/tmp",
            "agent_type": "codex"
        }
    }
}
```

### 9.3 并发模型

| Python 版 | Go 版 |
|-----------|-------|
| asyncio 事件循环 | goroutine + channel |
| 单线程异步 | 多线程并发（GOMAXPROCS） |
| `async/await` 隐式切换 | 显式 `select` + channel |
| GIL 限制 CPU 密集型操作 | 无 GIL，CPU 密集型不受限 |

### 9.4 错误处理差异

| Python 版 | Go 版 |
|-----------|-------|
| 异常传播 | 错误返回值（`error`） |
| `try/except/finally` | `if err != nil` + `defer` |
| 无 panic recovery（Python 异常自动传播） | 需要显式 `recover()` |
| 链式异常（`raise ... from`） | `fmt.Errorf("...: %w", err)` 包装 |
| logging 模块 | `log/slog` 结构化日志 |

### 9.5 会话管理差异

| Python 版 | Go 版 |
|-----------|-------|
| 全局 dict + Lock | `SessionPool` struct + `sync.RWMutex` |
| 无自动清理 | 后台 goroutine 定期清理过期会话 |
| session 直接关联 ACP 子进程 | 子进程与 Session 分离，同一子进程可创建多个 ACP 会话 |
| 无 agent 类型隔离 | 按 agent 类型聚合子进程池 |

### 9.6 优势总结

| 优势 | 说明 |
|------|------|
| **部署简单** | 单二进制，无 Python 运行时依赖，无 pip 安装 |
| **性能更好** | Go 编译型语言，无 GIL，goroutine 轻量 |
| **类型安全** | Go 静态类型，编译时发现错误 |
| **官方 SDK** | `acp-go-sdk` 是 ACP 协议的官方 Go 实现 |
| **进程隔离** | 独立进程，崩溃不影响 Hermes 主进程 |
| **资源管理** | 自动清理过期会话，goroutine 泄漏风险低 |
| **并发模型清晰** | channel 原生支持同步等待，无需 signal_queue 等 hack |
| **标准化协议** | 通过标准的 MCP 协议与 Hermes 通信 |

---

## 附录

### A. 关键 Go 类型映射

```
MCP JSON-RPC 消息        Go 类型
───────────────────      ───────
Request                  mcp.Request
Response                 mcp.Response
Notification             mcp.Notification
Tool                     mcp.Tool
CallToolResult           mcp.CallToolResult

ACP JSON-RPC 消息        Go 类型 (acp-go-sdk)
───────────────────      ──────────────
InitializeRequest        acp.InitializeRequest
InitializeResponse       acp.InitializeResponse
NewSessionRequest        acp.NewSessionRequest
NewSessionResponse       acp.NewSessionResponse
PromptRequest            acp.PromptRequest
SessionNotification      acp.SessionNotification
RequestPermissionRequest acp.RequestPermissionRequest
RequestPermissionResponse acp.RequestPermissionResponse
ReadTextFileRequest      acp.ReadTextFileRequest
WriteTextFileRequest     acp.WriteTextFileRequest
ContentBlock             acp.ContentBlock
```

### B. Session 状态机

```
        +-----------+
        |   Created  |  ← agent 子进程启动, ACP initialize 完成
        +-----+-----+
              |
              v
        +-----------+
        |  Session  |  ← session/new 返回 sessionId
        |   Active  |
        +-----+-----+
              |
        +-----+-----+          +-----------+
        |  Prompt    | ──────►  |  Pending  |
        |  Running   | 权限请求  | Permission|
        +-----+-----+          +-----+-----+
              |                       |
              | (prompt 完成)          | (用户响应)
              v                       v
        +-----------+          +-----------+
        |  Active   |◄─────────|  Running  |
        | (idle)    | 继续      |           |
        +-----------+          +-----+-----+
              |                      |
              | (session/close)      | (超时/取消)
              v                      v
        +-----------+          +-----------+
        |  Closed   |          | Cancelled |
        +-----------+          +-----------+
              |
              v
        +-----------+
        | Destroyed |  ← 从 SessionPool 移除
        +-----------+
```

### C. go.mod 依赖声明

```
module github.com/mapleafgo/acp-bridge

go 1.22

require github.com/coder/acp-go-sdk v0.13.5
```

---

> **文档维护者**: acp-bridge Go 重构项目组
> **最后更新**: 2026-07-08
> **文档状态**: ✅ 已定稿，等待实现阶段
