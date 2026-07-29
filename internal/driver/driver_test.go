package driver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mapleafgo/acp-bridge/internal/config"
)

// --- Compile-time interface checks ---

var _ AgentDriver = (*CodexDriver)(nil)
var _ AgentDriver = (*ClaudeDriver)(nil)
var _ AgentDriver = (*GeminiDriver)(nil)

// --- Factory: NewDriver ---

func TestNewDriver_Codex(t *testing.T) {
	cfg := config.Load()
	d, err := NewDriver(AgentTypeCodex, cfg)
	if err != nil {
		t.Fatalf("NewDriver(codex) unexpected error: %v", err)
	}
	if d.Type() != AgentTypeCodex {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeCodex)
	}
	cd, ok := d.(*CodexDriver)
	if !ok {
		t.Fatal("expected *CodexDriver")
	}
	if cd.path != cfg.CodexPath {
		t.Errorf("path = %q, want %q", cd.path, cfg.CodexPath)
	}
	if len(cd.args) != 0 {
		t.Errorf("args = %v, want empty", cd.args)
	}
}

func TestNewDriver_Claude(t *testing.T) {
	cfg := config.Load()
	d, err := NewDriver(AgentTypeClaude, cfg)
	if err != nil {
		t.Fatalf("NewDriver(claude) unexpected error: %v", err)
	}
	if d.Type() != AgentTypeClaude {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeClaude)
	}
	cd, ok := d.(*ClaudeDriver)
	if !ok {
		t.Fatal("expected *ClaudeDriver")
	}
	if cd.path != cfg.ClaudeAgentPath {
		t.Errorf("path = %q, want %q", cd.path, cfg.ClaudeAgentPath)
	}
}

func TestNewDriver_Gemini(t *testing.T) {
	cfg := config.Load()
	d, err := NewDriver(AgentTypeGemini, cfg)
	if err != nil {
		t.Fatalf("NewDriver(gemini) unexpected error: %v", err)
	}
	if d.Type() != AgentTypeGemini {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeGemini)
	}
	gd, ok := d.(*GeminiDriver)
	if !ok {
		t.Fatal("expected *GeminiDriver")
	}
	if gd.path != cfg.GeminiAgentPath {
		t.Errorf("path = %q, want %q", gd.path, cfg.GeminiAgentPath)
	}
}

func TestNewDriver_OpenCode(t *testing.T) {
	cfg := config.Load()
	d, err := NewDriver(AgentTypeOpenCode, cfg)
	if err != nil {
		t.Fatalf("NewDriver(opencode) unexpected error: %v", err)
	}
	if d.Type() != AgentTypeOpenCode {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeOpenCode)
	}
	od, ok := d.(*OpenCodeDriver)
	if !ok {
		t.Fatal("expected *OpenCodeDriver")
	}
	if od.path != cfg.OpenCodePath {
		t.Errorf("path = %q, want %q", od.path, cfg.OpenCodePath)
	}
}

func TestNewDriver_Unknown(t *testing.T) {
	_, err := NewDriver("unknown", config.Load())
	if err == nil {
		t.Fatal("expected error for unknown agent type")
	}
}

// --- Type() ---

func TestCodexDriver_Type(t *testing.T) {
	d := &CodexDriver{}
	if d.Type() != AgentTypeCodex {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeCodex)
	}
}

func TestClaudeDriver_Type(t *testing.T) {
	d := &ClaudeDriver{}
	if d.Type() != AgentTypeClaude {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeClaude)
	}
}

func TestGeminiDriver_Type(t *testing.T) {
	d := &GeminiDriver{}
	if d.Type() != AgentTypeGemini {
		t.Errorf("Type() = %q, want %q", d.Type(), AgentTypeGemini)
	}
}

// --- Capabilities ---

func TestCodexDriver_Capabilities(t *testing.T) {
	d := &CodexDriver{}
	caps := d.Capabilities()
	if !caps.SupportsLoadSession {
		t.Error("Codex should SupportsLoadSession")
	}
	if !caps.SupportsFork {
		t.Error("Codex should SupportsFork")
	}
	if caps.SupportsAuthenticate {
		t.Error("Codex should NOT SupportsAuthenticate")
	}
}

func TestClaudeDriver_Capabilities(t *testing.T) {
	d := &ClaudeDriver{}
	caps := d.Capabilities()
	if !caps.SupportsLoadSession {
		t.Error("Claude should SupportsLoadSession")
	}
	if !caps.SupportsFork {
		t.Error("Claude should SupportsFork")
	}
	if caps.SupportsAuthenticate {
		t.Error("Claude should NOT SupportsAuthenticate")
	}
}

func TestGeminiDriver_Capabilities(t *testing.T) {
	d := &GeminiDriver{}
	caps := d.Capabilities()
	if !caps.SupportsLoadSession {
		t.Error("Gemini should SupportsLoadSession")
	}
	if caps.SupportsAuthenticate {
		t.Error("Gemini should NOT SupportsAuthenticate")
	}
}

// --- Command construction via custom args ---

func TestCodexDriver_CommandConstruction(t *testing.T) {
	cfg := &config.Config{
		CodexPath: "npx",
		CodexArgs: []string{"--verbose", "--timeout", "60"},
	}
	d, err := NewDriver(AgentTypeCodex, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cd := d.(*CodexDriver)
	if cd.path != "npx" {
		t.Errorf("path = %q, want %q", cd.path, "npx")
	}
	if len(cd.args) != 3 {
		t.Fatalf("expected 3 args, got %v", cd.args)
	}
	if cd.args[0] != "--verbose" {
		t.Errorf("args[0] = %q, want %q", cd.args[0], "--verbose")
	}
}

func TestClaudeDriver_CommandConstruction(t *testing.T) {
	cfg := &config.Config{
		ClaudeAgentPath: "/usr/local/bin/claude-agent-acp",
		ClaudeAgentArgs: []string{"--model", "claude-opus-4"},
	}
	d, err := NewDriver(AgentTypeClaude, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cd := d.(*ClaudeDriver)
	if cd.path != "/usr/local/bin/claude-agent-acp" {
		t.Errorf("path = %q, want %q", cd.path, "/usr/local/bin/claude-agent-acp")
	}
	if len(cd.args) != 2 {
		t.Fatalf("expected 2 args, got %v", cd.args)
	}
	if cd.args[1] != "claude-opus-4" {
		t.Errorf("args[1] = %q, want %q", cd.args[1], "claude-opus-4")
	}
}

func TestGeminiDriver_CommandConstruction(t *testing.T) {
	cfg := &config.Config{
		GeminiAgentPath: "gemini-agent-acp",
		GeminiAgentArgs: []string{"--project", "my-project"},
	}
	d, err := NewDriver(AgentTypeGemini, cfg)
	if err != nil {
		t.Fatal(err)
	}
	gd := d.(*GeminiDriver)
	if gd.path != "gemini-agent-acp" {
		t.Errorf("path = %q, want %q", gd.path, "gemini-agent-acp")
	}
	if len(gd.args) != 2 {
		t.Fatalf("expected 2 args, got %v", gd.args)
	}
	if gd.args[0] != "--project" {
		t.Errorf("args[0] = %q, want %q", gd.args[0], "--project")
	}
}

// --- StartError ---

func TestStartError_ImplementsError(t *testing.T) {
	var err error = &StartError{
		AgentType: AgentTypeCodex,
		Path:      "/bin/nonexistent",
		ExitCode:  1,
		Stderr:    "command not found",
		Err:       context.DeadlineExceeded,
	}
	msg := err.Error()
	if msg == "" {
		t.Fatal("StartError.Error() returned empty string")
	}
}

// --- Start returns error for nonexistent binary ---

func TestStart_BinaryNotFound(t *testing.T) {
	d := &CodexDriver{
		path: "/nonexistent/binary-that-does-not-exist-xyz789",
	}
	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
}

func TestDriverStartErrorIncludesAgentType(t *testing.T) {
	tests := map[AgentType]AgentDriver{
		AgentTypeCodex:  &CodexDriver{path: "/nonexistent/codex-binary-xyz789"},
		AgentTypeClaude: &ClaudeDriver{path: "/nonexistent/claude-binary-xyz789"},
		AgentTypeGemini: &GeminiDriver{path: "/nonexistent/gemini-binary-xyz789"},
	}
	for agentType, drv := range tests {
		t.Run(string(agentType), func(t *testing.T) {
			_, err := drv.Start(context.Background())
			var startErr *StartError
			if !errors.As(err, &startErr) {
				t.Fatalf("expected StartError, got %T: %v", err, err)
			}
			if startErr.AgentType != agentType {
				t.Fatalf("AgentType = %q, want %q", startErr.AgentType, agentType)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildCmd 单元测试
// ---------------------------------------------------------------------------

func TestBuildCmd_BinaryNotFound(t *testing.T) {
	_, err := buildCmd(context.Background(), "/nonexistent/binary-xyz789", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
	// Should be a *StartError
	se, ok := err.(*StartError)
	if !ok {
		t.Fatalf("expected *StartError, got %T: %v", err, err)
	}
	if se.Path != "/nonexistent/binary-xyz789" {
		t.Errorf("expected Path to be set, got %q", se.Path)
	}
}

func TestBuildCmd_NpxSkipsCheck(t *testing.T) {
	// npx should skip the binary check — buildCmd must succeed even if
	// npx isn't in PATH in this test environment.
	_, err := buildCmd(context.Background(), "npx", []string{"@agentclientprotocol/codex-acp"})
	if err != nil {
		t.Fatalf("expected no error for npx, got: %v", err)
	}
}

func TestBuildCmd_ExistingBinary(t *testing.T) {
	// /bin/echo exists on all Linux systems
	cmd, err := buildCmd(context.Background(), "/bin/echo", []string{"hello"})
	if err != nil {
		t.Fatalf("expected no error for /bin/echo: %v", err)
	}
	if cmd == nil {
		t.Fatal("expected non-nil cmd")
	}
}

func TestBuildCmd_PassesInitialAgentMode(t *testing.T) {
	t.Setenv("INITIAL_AGENT_MODE", "agent-full-access")

	cmd, err := buildCmd(context.Background(), "/bin/echo", nil)
	if err != nil {
		t.Fatalf("buildCmd error: %v", err)
	}

	// Verify INITIAL_AGENT_MODE is in cmd.Env
	found := false
	for _, kv := range cmd.Env {
		if kv == "INITIAL_AGENT_MODE=agent-full-access" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected INITIAL_AGENT_MODE in cmd.Env")
	}
}

func TestBuildCmd_NoInitialAgentMode(t *testing.T) {
	// Ensure the env var is not set
	_ = os.Unsetenv("INITIAL_AGENT_MODE")

	cmd, err := buildCmd(context.Background(), "/bin/echo", nil)
	if err != nil {
		t.Fatalf("buildCmd error: %v", err)
	}

	// cmd.Env should be nil (no modification when INITIAL_AGENT_MODE unset)
	if cmd.Env != nil {
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "INITIAL_AGENT_MODE") {
				t.Errorf("INITIAL_AGENT_MODE should not be in cmd.Env: %s", kv)
			}
		}
	}
}

// --- Start methods for claude and gemini drivers ---

func TestCommandStringDriversStart(t *testing.T) {
	tests := map[string]AgentDriver{
		"claude": &ClaudeDriver{path: "sh -c true"},
		"gemini": &GeminiDriver{path: "sh -c true"},
	}
	for name, drv := range tests {
		t.Run(name, func(t *testing.T) {
			process, err := drv.Start(context.Background())
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			<-process.Done()
		})
	}
}

func TestClaudeDriver_StartBinaryNotFound(t *testing.T) {
	d := &ClaudeDriver{
		path: "/nonexistent/claude-binary-xyz789",
	}
	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent claude binary")
	}
}

func TestGeminiDriver_StartBinaryNotFound(t *testing.T) {
	d := &GeminiDriver{
		path: "/nonexistent/gemini-binary-xyz789",
	}
	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent gemini binary")
	}
}

func TestCodexDriver_StartNpxNoBinaryCheck(t *testing.T) {
	// Codex driver with npx path should pass buildCmd (npx skips check)
	// but the actual subprocess start may fail if npx isn't installed.
	// We only verify buildCmd doesn't reject npx.
	d := &CodexDriver{path: "npx", args: []string{"@agentclientprotocol/codex-acp"}}
	// We can't fully test Start without a real npx, but buildCmd should not error.
	_, err := buildCmd(context.Background(), "npx", d.args)
	if err != nil {
		t.Fatalf("expected no error for npx buildCmd: %v", err)
	}
}
