# 仓库贡献指南

acp-bridge 是一个 Go 服务，将 ACP 兼容的 agent（codex-acp、claude-agent-acp、gemini-agent-acp）桥接到基于 stdio 的 Model Context Protocol（MCP）。MCP 宿主（如 Hermes Agent）启动该二进制后，由它按需拉起 agent 子进程，把 MCP 工具调用翻译为 ACP JSON-RPC 2.0 请求。

## 项目结构与模块组织

- `cmd/acp-bridge/` — 程序入口，加载配置并在 stdio 上启动 MCP 服务器。
- `internal/config/` — 从 `ACP_BRIDGE_*` 环境变量读取全部配置项。
- `internal/client/` — 封装 `acp-go-sdk` 的客户端连接与通知处理。
- `internal/driver/` — 定义 `AgentDriver` 接口及 codex/claude/gemini 三种实现。
- `internal/mcp/` — 基于 SDK 的 MCP 服务器、工具注册与各工具 handler。
- `internal/session/` — 并发安全的 `SessionPool`，含空闲清理与取消传播。
  - `internal/mcp/skill.go` 和 `internal/mcp/skill.md` — skill 与 MCP 工具同源暴露（见下方「Skill 与 MCP 的关系」一节）。
- 测试与源码同目录放置，文件名为 `*_test.go`。架构细节见 `DESIGN.md`。

## 构建、测试与开发命令

```bash
task build          # 编译二进制到 bin/
task run            # 直接在 stdio 上运行（调试用）
task test           # 运行全部测试，开启竞态检测
task test:verbose   # 全部测试，输出详细信息
task vet            # 静态检查
task fmt            # 格式化全部 Go 源码
task check          # 一键检查：fmt + vet + test + build
task clean          # 清理构建产物
```

> 项目使用 [Taskfile](https://taskfile.dev) 管理命令，先[安装 task](https://taskfile.dev/installation/)，再用 `task --list` 查看所有命令。

## 代码风格与命名约定

沿用 Go 标准工具链：`gofmt`/`goimports` 格式化，缩进统一为 tab，不存在空格与 tab 混用问题。保持与现有文件一致的包级文档注释（如 `// Package driver ...`）。导出标识符使用 PascalCase；配置键统一加 `ACP_BRIDGE_*` 前缀、用 snake_case。MCP 工具名沿用 `internal/mcp/mcp.go` 中已有的 `acp_*` 命名模式。

## MCP 实现规范

编写或修改 MCP 工具 handler 时，必须遵循以下 SDK 范式：

1. **结构化输出** — handler 的第三个返回值（泛型 `Out`）必须是具体的输出类型（如 `chatResultJSON`、`toolResult`），不能是 `struct{}`。SDK 会自动推导 `OutputSchema`、填充 `StructuredContent` 并生成 JSON 文本 `Content`。禁止手动 `json.Marshal` 结果再塞进 `TextContent`。

2. **IsError 报告工具错误** — handler 内部的业务错误（session 不存在、agent 启动失败等）必须通过 `&sdk.CallToolResult{IsError: true}` 报告，而不是返回 `IsError` 为 false 的正常结果。MCP 客户端依赖 `IsError` 区分工具执行失败与正常返回。仅当结果是"需要用户输入"（如 `permission_required`）等语义上不算错误的场景时，才保持 `IsError` 为 false。

3. **StdioTransport 直传** — 服务器启动时直接使用 `&sdk.StdioTransport{}`，不要手动包装 `io.Reader`/`io.Writer` 构造 `IOTransport`。`StdioTransport` 内部已处理 `os.Stdin`/`os.Stdout`，额外包装只增加无意义的代码。

工具注册使用 SDK 泛型 `mcp.AddTool`，参数 schema 从 `In` 类型的 `jsonschema` tag 自动推断，无需手写 `InputSchema`。

## Skill 与 MCP 的关系

acp-bridge 把自己的 skill（面向宿主侧模型的「何时使用 acp_* 工具」说明）和 MCP 工具**统一在同一个二进制里**，不再单独维护 `skills/acp-bridge/` 目录。具体做法：

1. skill 正文写在 [internal/mcp/skill.md](internal/mcp/skill.md)。
2. [internal/mcp/skill.go](internal/mcp/skill.go) 用 `//go:embed skill.md` 把正文编译进二进制。
3. 服务器启动时调用 `registerSkill()`，把同一份正文注册成一个 MCP Resource：
   - URI：`acp-bridge://skill`
   - MIME：`text/markdown`
   - Description：从 `skill.md` 的 frontmatter `description` 字段提取，与 skill 触发条件完全一致。

宿主侧的 OrchestratorSkillProvider 走标准 MCP resource 协议（`resources/list` → `resources/read`）就能拿到 skill 全文，无需任何额外目录、软链或 Hermes `skills.external_dirs` 配置。好处是：

- skill 与工具共享同一条 stdio 连接，天然同源、同版本、同权限边界；
- 不会出现「仓库里多了一个 skills 目录，但宿主路径没配置」造成的静默不可见；
- 二进制升级即 skill 升级，不存在旧版 skill 仍在用的漂移问题。

修改 skill 时，**只改 `internal/mcp/skill.md`**，不要新建任何 `skills/` 目录或独立的 `SKILL.md` 文件。skill.md 的 frontmatter 必须包含 `name` 和 `description` 两个字段（由 `skill.go` 的 `extractFrontmatterField` 解析），否则 resource description 会回落到默认提示文案。

## 测试指南

测试使用标准库 `testing`。遵循现有命名习惯：按类型分组的描述性顶层函数（如 `TestNewDriver_Codex`、`TestMaxSessions_ExactLimit`、`TestAcpChatPermissionRequired`），必要时用 `t.Run` 子测试。并发敏感的包（`internal/session`）必须在 `-race` 下通过。优先 mock 子进程与 stdio 行为，不依赖真实 agent 二进制。

## 配置

运行时行为全部由 `ACP_BRIDGE_*` 环境变量控制（agent 路径、额外参数、超时、会话 TTL、最大会话数、日志级别与格式）。默认值定义在 `internal/config/config.go`。调试 prompt 与权限交互流程时可设置 `ACP_BRIDGE_LOG_LEVEL=debug`。

## 提交与 Pull Request 指南

本仓库尚未纳入 git 版本控制（`.git` 目录为空）。初始化后，建议使用简短的 conventional-commit 信息并按受影响包添加 scope，例如 `feat(mcp): add acp_respond handler` 或 `fix(session): clear pending turns on close`。每次行为变更须附带同目录的测试，并在请求 review 前运行 `task check`（等价于 fmt + vet + test + build）。
