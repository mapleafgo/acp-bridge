package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewServer(t *testing.T) {
	cfg := config.Load()
	pool := session.NewPool(cfg)
	defer pool.Shutdown()

	srv := NewServer(cfg, pool)
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if srv.sdkServer == nil {
		t.Fatal("SDK server is nil")
	}
}

func TestToolsViaInMemoryTransport(t *testing.T) {
	cfg := config.Load()
	pool := session.NewPool(cfg)
	defer pool.Shutdown()

	srv := NewServer(cfg, pool)

	mock := newMockAcpClient()
	srv.clientFactory = func(_ context.Context, _ string) (acpClient, error) {
		return mock, nil
	}

	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	ctx := context.Background()
	go func() {
		// MUST start server before client — SDK requirement
		if err := srv.sdkServer.Run(ctx, serverTransport); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()

	client := sdk.NewClient(&sdk.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// --- ListTools ---
	tools, err := sess.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("expected at least one tool")
	}

	toolNames := make(map[string]bool, len(tools.Tools))
	var found bool
	for _, tool := range tools.Tools {
		toolNames[tool.Name] = true
		if tool.Name == "acp_chat" {
			found = true
			if tool.Description == "" {
				t.Error("acp_chat tool has empty description")
			}
		}
	}
	if !found {
		t.Fatal("acp_chat tool not found in tools list")
	}
	if !toolNames["acp_interrupt"] {
		t.Fatal("acp_interrupt tool not found in tools list")
	}
	if toolNames["acp_cancel"] {
		t.Fatal("acp_cancel must not be registered")
	}

	// --- CallTool: acp_chat with valid prompt ---
	result, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name: "acp_chat",
		Arguments: map[string]any{
			"prompt": "Hello world",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(acp_chat) failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned unexpected error: %v", result.GetError())
	}
	if len(result.Content) == 0 {
		t.Fatal("expected non-empty content in result")
	}

	// --- CallTool: missing required arg (prompt) ---
	result, err = sess.CallTool(ctx, &sdk.CallToolParams{
		Name:      "acp_chat",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool(acp_chat) without prompt failed: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error for missing required argument 'prompt'")
	}

	// --- CallTool: unknown tool ---
	_, err = sess.CallTool(ctx, &sdk.CallToolParams{
		Name:      "nonexistent_tool",
		Arguments: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for nonexistent tool")
	}
}

// ---------------------------------------------------------------------------
// Skill 作为 MCP Resource 暴露（替代独立 skills 目录）
// ---------------------------------------------------------------------------

func TestSkillResourceListed(t *testing.T) {
	cfg := config.Load()
	pool := session.NewPool(cfg)
	defer pool.Shutdown()

	srv := NewServer(cfg, pool)

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		if err := srv.sdkServer.Run(ctx, serverTransport); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()

	client := sdk.NewClient(&sdk.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// resources/list 应能发现 acp-bridge://skill
	result, err := sess.ListResources(ctx, &sdk.ListResourcesParams{})
	if err != nil {
		t.Fatalf("ListResources failed: %v", err)
	}
	var found *sdk.Resource
	for i := range result.Resources {
		if result.Resources[i].URI == skillResourceURI {
			found = result.Resources[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("skill resource %q not found in resources/list: %+v", skillResourceURI, result.Resources)
	}
	if found.MIMEType != "text/markdown" {
		t.Errorf("expected mimeType=text/markdown, got %q", found.MIMEType)
	}
	if found.Description == "" {
		t.Error("skill resource description should not be empty (should come from SKILL.md frontmatter)")
	}
	if found.Description != "Use when delegating coding tasks to a subagent (Codex, Claude, Gemini, or OpenCode) via ACP, when acp_chat returns status running, when an agent requests permission approval, or when managing agent sessions (fork, resume, load history)" {
		t.Errorf("skill resource description mismatch: %q", found.Description)
	}
}

func TestSkillResourceReadable(t *testing.T) {
	cfg := config.Load()
	pool := session.NewPool(cfg)
	defer pool.Shutdown()

	srv := NewServer(cfg, pool)

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		if err := srv.sdkServer.Run(ctx, serverTransport); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()

	client := sdk.NewClient(&sdk.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// resources/read 应返回 skill.md 全文
	result, err := sess.ReadResource(ctx, &sdk.ReadResourceParams{
		URI: skillResourceURI,
	})
	if err != nil {
		t.Fatalf("ReadResource failed: %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected non-empty resource contents")
	}
	text := result.Contents[0].Text
	if text == "" {
		t.Fatal("expected non-empty text in resource")
	}
	if text != skillMD {
		t.Errorf("resource text does not match embedded skill.md (len: got=%d want=%d)", len(text), len(skillMD))
	}
	// skill.md 应包含 frontmatter 和正文标记，否则 go:embed 拿到的是空文件
	if !strings.Contains(text, "name: acp-bridge") {
		t.Errorf("resource text missing frontmatter 'name: acp-bridge': %q", text[:min(80, len(text))])
	}
	if !strings.Contains(text, "# acp-bridge") {
		t.Errorf("resource text missing '# acp-bridge' heading")
	}
}

func TestSkillResourceUnknownURI(t *testing.T) {
	cfg := config.Load()
	pool := session.NewPool(cfg)
	defer pool.Shutdown()

	srv := NewServer(cfg, pool)

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		if err := srv.sdkServer.Run(ctx, serverTransport); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()

	client := sdk.NewClient(&sdk.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// 未注册的 URI 应返回 resource-not-found 错误，而不是 panic 或空内容
	_, err = sess.ReadResource(ctx, &sdk.ReadResourceParams{
		URI: "acp-bridge://nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for unknown resource URI")
	}
}

// ---------------------------------------------------------------------------
// #30 环境变量驱动运行时行为
// ---------------------------------------------------------------------------

func TestEnvMaxSessionsAffectsServer(t *testing.T) {
	t.Setenv("ACP_BRIDGE_MAX_SESSIONS", "2")

	cfg := config.Load()
	if cfg.MaxSessions != 2 {
		t.Fatalf("expected MaxSessions=2, got %d", cfg.MaxSessions)
	}

	pool := session.NewPool(cfg)
	defer pool.Shutdown()

	// 添加 3 个 session，第 3 个应触发 LRU 淘汰第 1 个
	for _, id := range []session.SessionID{"env-a", "env-b", "env-c"} {
		if err := pool.Add(&session.Session{ID: id, AgentType: "codex"}); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	if _, err := pool.Get("env-a"); err == nil {
		t.Error("env-a should be evicted (LRU) when MaxSessions=2")
	}
	if _, err := pool.Get("env-b"); err != nil {
		t.Error("env-b should survive")
	}
	if _, err := pool.Get("env-c"); err != nil {
		t.Error("env-c should survive")
	}
}

func TestEnvSessionTTLAffectsCleanup(t *testing.T) {
	t.Setenv("ACP_BRIDGE_SESSION_TTL", "100ms")

	cfg := config.Load()
	pool := session.NewPool(cfg, session.WithCleanupInterval(30*time.Millisecond))
	defer pool.Shutdown()

	pool.Add(&session.Session{ID: "ttl-test", AgentType: "codex"})

	// 等 TTL 过期 + 清理周期触发
	time.Sleep(200 * time.Millisecond)

	if _, err := pool.Get("ttl-test"); err == nil {
		t.Error("session should be cleaned up after TTL=100ms")
	}
}

func TestEnvLogLevelAffectsServer(t *testing.T) {
	t.Setenv("ACP_BRIDGE_LOG_LEVEL", "debug")

	cfg := config.Load()
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected LogLevel=debug, got %s", cfg.LogLevel)
	}

	pool := session.NewPool(cfg)
	defer pool.Shutdown()
	srv := NewServer(cfg, pool)
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

func TestEnvAgentPathAffectsDriver(t *testing.T) {
	t.Setenv("ACP_BRIDGE_CODEX_PATH", "echo")

	cfg := config.Load()
	if cfg.CodexPath != "echo" {
		t.Fatalf("expected CodexPath=echo, got %s", cfg.CodexPath)
	}

	drv, err := driver.NewDriver(driver.AgentTypeCodex, cfg)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if drv.Type() != driver.AgentTypeCodex {
		t.Fatalf("expected codex, got %s", drv.Type())
	}
}
