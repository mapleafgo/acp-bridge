package client

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
)

// ---------------------------------------------------------------------------
// mock types
// ---------------------------------------------------------------------------

// mockReadCloser wraps a string reader into an io.ReadCloser.
type mockReadCloser struct {
	io.Reader
}

func (m *mockReadCloser) Close() error { return nil }

// mockWriteCloser wraps a strings.Builder into an io.WriteCloser.
type mockWriteCloser struct {
	w *strings.Builder
}

func (m *mockWriteCloser) Write(p []byte) (int, error) { return m.w.Write(p) }
func (m *mockWriteCloser) Close() error                { return nil }

// mockDriver implements driver.AgentDriver for testing without a real
// subprocess.
type mockDriver struct {
	t            *testing.T
	agentHandler func(t *testing.T, stdin io.ReadCloser, stdout io.WriteCloser, stderr io.WriteCloser)
}

func (m *mockDriver) Type() driver.AgentType { return driver.AgentTypeCodex }

func (m *mockDriver) Capabilities() driver.AgentCapabilities {
	return driver.AgentCapabilities{}
}

func (m *mockDriver) Start(ctx context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	clientRead, agentWrite := io.Pipe()
	agentRead, clientWrite := io.Pipe()
	stderrRead, stderrWrite := io.Pipe()

	if m.agentHandler != nil {
		go m.agentHandler(m.t, agentRead, agentWrite, stderrWrite)
	}

	return clientRead, clientWrite, stderrRead, nil
}

// errorDriver always fails to start.
type errorDriver struct{}

func (errorDriver) Type() driver.AgentType                 { return driver.AgentTypeCodex }
func (errorDriver) Capabilities() driver.AgentCapabilities { return driver.AgentCapabilities{} }
func (errorDriver) Start(context.Context) (io.ReadCloser, io.WriteCloser, io.ReadCloser, error) {
	return nil, nil, nil, errors.New("driver start failed")
}

// ---------------------------------------------------------------------------
// Handler tests
// ---------------------------------------------------------------------------

func TestNewHandler(t *testing.T) {
	h := newHandler()
	if h == nil {
		t.Fatal("newHandler returned nil")
	}
	if len(h.permissionCh) != 0 {
		t.Errorf("expected empty permissionCh, got %d", len(h.permissionCh))
	}
}

func TestHandlerRequestPermissionNormal(t *testing.T) {
	h := newHandler()

	requestID := "tc-1"
	req := acp.RequestPermissionRequest{
		SessionId: "sess-1",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(requestID),
		},
	}

	var (
		resp acp.RequestPermissionResponse
		err  error
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err = h.RequestPermission(context.Background(), req)
	}()

	time.Sleep(10 * time.Millisecond)

	expectedResp := acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{
				OptionId: "allow",
			},
		},
	}
	if injectErr := h.Respond(requestID, expectedResp); injectErr != nil {
		t.Fatalf("Respond: %v", injectErr)
	}

	wg.Wait()
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "allow" {
		t.Errorf("expected Selected.OptionId=allow, got %+v", resp.Outcome)
	}
}

func TestHandlerRequestPermissionTimeout(t *testing.T) {
	h := newHandler()

	req := acp.RequestPermissionRequest{
		SessionId: "sess-2",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tc-2",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := h.RequestPermission(ctx, req)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestHandlerRequestPermissionCancel(t *testing.T) {
	h := newHandler()

	req := acp.RequestPermissionRequest{
		SessionId: "sess-3",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tc-3",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	var err error
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err = h.RequestPermission(ctx, req)
	}()

	time.Sleep(5 * time.Millisecond)
	cancel()
	wg.Wait()

	if err == nil {
		t.Fatal("expected cancelled error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestHandlerRequestPermissionCloseUnblocks(t *testing.T) {
	h := newHandler()

	req := acp.RequestPermissionRequest{
		SessionId: "sess-4",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tc-4",
		},
	}

	var err error
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err = h.RequestPermission(context.Background(), req)
	}()

	time.Sleep(5 * time.Millisecond)

	h.close()
	wg.Wait()

	if err == nil {
		t.Fatal("expected closed error, got nil")
	}
}

func TestHandlerRespondUnknownID(t *testing.T) {
	h := newHandler()
	err := h.Respond("nonexistent", acp.RequestPermissionResponse{})
	if err == nil {
		t.Fatal("expected error for unknown request ID")
	}
}

func TestHandlerSessionUpdateAndPop(t *testing.T) {
	h := newHandler()
	sessionID := "sess-stream"

	notif1 := acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update: acp.SessionUpdate{
			Plan: &acp.SessionUpdatePlan{
				Entries: []acp.PlanEntry{{Content: "plan1"}},
			},
		},
	}
	notif2 := acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update: acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{Title: "tool1"},
		},
	}

	if err := h.SessionUpdate(context.Background(), notif1); err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}
	if err := h.SessionUpdate(context.Background(), notif2); err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}

	updates := h.PopUpdates(sessionID)
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
	if updates[0].Update.Plan == nil {
		t.Error("expected first update to be a Plan update")
	}
	if updates[1].Update.ToolCall == nil {
		t.Error("expected second update to be a ToolCall update")
	}

	remaining := h.PopUpdates(sessionID)
	if len(remaining) != 0 {
		t.Errorf("expected empty after drain, got %d", len(remaining))
	}
}

func TestHandlerSessionUpdateClosed(t *testing.T) {
	h := newHandler()
	h.close()

	notif := acp.SessionNotification{
		SessionId: "sess-closed",
	}
	err := h.SessionUpdate(context.Background(), notif)
	if err == nil {
		t.Fatal("expected error on closed handler")
	}
}

func TestHandlerUnsupportedMethods(t *testing.T) {
	h := newHandler()

	// Elicitation / MCP experimental methods also return errNotSupported.
	if _, err := h.UnstableConnectMcp(context.Background(), acp.UnstableConnectMcpRequest{}); err != errNotSupported {
		t.Errorf("UnstableConnectMcp: expected errNotSupported, got %v", err)
	}
	if _, err := h.UnstableDisconnectMcp(context.Background(), acp.UnstableDisconnectMcpRequest{}); err != errNotSupported {
		t.Errorf("UnstableDisconnectMcp: expected errNotSupported, got %v", err)
	}

	if _, err := h.ReadTextFile(context.Background(), acp.ReadTextFileRequest{}); err != errNotSupported {
		t.Errorf("ReadTextFile: expected errNotSupported, got %v", err)
	}
	if _, err := h.WriteTextFile(context.Background(), acp.WriteTextFileRequest{}); err != errNotSupported {
		t.Errorf("WriteTextFile: expected errNotSupported, got %v", err)
	}
	if _, err := h.CreateTerminal(context.Background(), acp.CreateTerminalRequest{}); err != errNotSupported {
		t.Errorf("CreateTerminal: expected errNotSupported, got %v", err)
	}
	if _, err := h.KillTerminal(context.Background(), acp.KillTerminalRequest{}); err != errNotSupported {
		t.Errorf("KillTerminal: expected errNotSupported, got %v", err)
	}
	if _, err := h.TerminalOutput(context.Background(), acp.TerminalOutputRequest{}); err != errNotSupported {
		t.Errorf("TerminalOutput: expected errNotSupported, got %v", err)
	}
	if _, err := h.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{}); err != errNotSupported {
		t.Errorf("ReleaseTerminal: expected errNotSupported, got %v", err)
	}
	if _, err := h.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{}); err != errNotSupported {
		t.Errorf("WaitForTerminalExit: expected errNotSupported, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// PermissionSignal 测试
// ---------------------------------------------------------------------------

func TestHandlerPermissionSignal(t *testing.T) {
	h := newHandler()

	// PermissionSignal 应该返回同一个 channel
	ch1 := h.PermissionSignal()
	ch2 := h.PermissionSignal()
	if ch1 != ch2 {
		t.Error("PermissionSignal should return the same channel on each call")
	}
}

func TestHandlerPermissionSignalReceivesRequest(t *testing.T) {
	h := newHandler()

	req := acp.RequestPermissionRequest{
		SessionId: "sess-signal",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tc-signal",
		},
	}

	go func() {
		// Will block until Respond is called, but the signal should
		// arrive immediately.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.RequestPermission(ctx, req)
	}()

	select {
	case got := <-h.PermissionSignal():
		if string(got.ToolCall.ToolCallId) != "tc-signal" {
			t.Fatalf("expected tool_call_id=tc-signal, got %s", got.ToolCall.ToolCallId)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission signal")
	}

	// Cleanup: respond to unblock the goroutine
	h.Respond("tc-signal", acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeCancelled(),
	})
}

// ---------------------------------------------------------------------------
// Elicitation 测试
// ---------------------------------------------------------------------------

func TestHandlerUnstableCompleteElicitation(t *testing.T) {
	h := newHandler()
	// Should be a no-op and return nil
	if err := h.UnstableCompleteElicitation(context.Background(), acp.UnstableCompleteElicitationNotification{}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestHandlerUnstableCreateElicitationRoutesToPermission(t *testing.T) {
	h := newHandler()

	// Form-based elicitation
	params := acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Please enter your API key",
			Mode:    "form",
		},
	}

	var (
		resp acp.UnstableCreateElicitationResponse
		err  error
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err = h.UnstableCreateElicitation(context.Background(), params)
	}()

	// Verify a permission request was broadcast on permissionSignal.
	select {
	case got := <-h.PermissionSignal():
		if got.ToolCall.Title == nil || *got.ToolCall.Title != "Please enter your API key" {
			t.Fatalf("expected title from elicitation message, got %+v", got.ToolCall.Title)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for elicitation on permission signal")
	}

	// Inject an "accept" response via the permission channel.
	h.Respond("elicitation", acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected("accept"),
	})

	wg.Wait()
	if err != nil {
		t.Fatalf("UnstableCreateElicitation error: %v", err)
	}
	if resp.Accept == nil {
		t.Fatal("expected Accept outcome")
	}
}

func TestHandlerUnstableCreateElicitationDecline(t *testing.T) {
	h := newHandler()

	params := acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Confirm action",
			Mode:    "form",
		},
	}

	var (
		resp acp.UnstableCreateElicitationResponse
		err  error
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err = h.UnstableCreateElicitation(context.Background(), params)
	}()

	// Wait for signal
	<-h.PermissionSignal()

	// Inject a "reject" response (maps to Decline)
	h.Respond("elicitation", acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeCancelled(),
	})

	wg.Wait()
	if err != nil {
		t.Fatalf("UnstableCreateElicitation error: %v", err)
	}
	if resp.Decline == nil {
		t.Fatal("expected Decline outcome")
	}
}

func TestHandlerUnstableCreateElicitationOnClosedHandler(t *testing.T) {
	h := newHandler()
	h.close()

	_, err := h.UnstableCreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{})
	if err == nil {
		t.Fatal("expected error on closed handler")
	}
}

// ---------------------------------------------------------------------------
// Client lifecycle tests
// ---------------------------------------------------------------------------

func TestCloseIdempotent(t *testing.T) {
	h := newHandler()
	c := &Client{
		conn:    nil,
		handler: h,
		stdin:   &mockWriteCloser{w: &strings.Builder{}},
		stdout:  &mockReadCloser{Reader: strings.NewReader("")},
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}
}

func TestCloseUnblocksPermissionRequests(t *testing.T) {
	h := newHandler()
	c := &Client{
		handler: h,
	}

	req := acp.RequestPermissionRequest{
		SessionId: "sess-close-test",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tc-close-test",
		},
	}

	var err error
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err = h.RequestPermission(context.Background(), req)
	}()

	time.Sleep(10 * time.Millisecond)

	_ = c.Close()
	wg.Wait()

	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
}

func TestNewReturnsErrorOnStartFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := New(ctx, errorDriver{})
	if err == nil {
		t.Fatal("expected error from New when Start fails")
	}
}

func TestNewReturnsErrorOnNonRespondingAgent(t *testing.T) {
	badDriver := &mockDriver{
		t: t,
		agentHandler: func(t *testing.T, stdin io.ReadCloser, stdout io.WriteCloser, stderr io.WriteCloser) {
			// Drain stdin so the SDK's blocking write to io.Pipe
			// completes, but never respond - waitForResponse times
			// out via context.
			io.Copy(io.Discard, stdin)
			stdout.Close()
			stderr.Close()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := New(ctx, badDriver)
	if err == nil {
		t.Fatal("expected error from New with non-responding agent")
	}
}

// ---------------------------------------------------------------------------
// End-to-end: New -> Initialize -> NewSession -> Prompt with a mock agent
// ---------------------------------------------------------------------------

// mockAgent implements acp.Agent for end-to-end testing.
type mockAgent struct {
	newSessionID string
	promptCh     chan acp.PromptRequest
}

func (m *mockAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (m *mockAgent) NewSession(_ context.Context, _ acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: acp.SessionId(m.newSessionID)}, nil
}

func (m *mockAgent) LoadSession(_ context.Context, _ acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	return acp.LoadSessionResponse{}, nil
}

func (m *mockAgent) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (m *mockAgent) Prompt(_ context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	select {
	case m.promptCh <- p:
	default:
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (m *mockAgent) Cancel(_ context.Context, _ acp.CancelNotification) error { return nil }

func (m *mockAgent) CloseSession(_ context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (m *mockAgent) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
}

func (m *mockAgent) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}

func (m *mockAgent) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func (m *mockAgent) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (m *mockAgent) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

var _ acp.Agent = (*mockAgent)(nil)

func TestEndToEndNewSessionAndPrompt(t *testing.T) {
	const wantSessionID = "e2e-session-1"
	agent := &mockAgent{
		newSessionID: wantSessionID,
		promptCh:     make(chan acp.PromptRequest, 1),
	}

	drv := &mockDriver{
		t: t,
		agentHandler: func(t *testing.T, stdin io.ReadCloser, stdout io.WriteCloser, stderr io.WriteCloser) {
			defer func() {
				stdin.Close()
				stdout.Close()
				stderr.Close()
			}()
			// peerInput = what the agent writes (stdout)
			// peerOutput = what the agent reads (stdin)
			conn := acp.NewAgentSideConnection(agent, stdout, stdin)
			<-conn.Done()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cl, err := New(ctx, drv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Close()

	sessResp, err := cl.NewSession(ctx, "/tmp")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if string(sessResp.SessionId) != wantSessionID {
		t.Errorf("session ID: got %q, want %q", sessResp.SessionId, wantSessionID)
	}

	promptResp, err := cl.Prompt(ctx, wantSessionID, []acp.ContentBlock{acp.TextBlock("hello")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if promptResp.StopReason != acp.StopReasonEndTurn {
		t.Errorf("StopReason: got %q, want %q", promptResp.StopReason, acp.StopReasonEndTurn)
	}

	select {
	case got := <-agent.promptCh:
		if len(got.Prompt) == 0 {
			t.Error("agent received empty prompt blocks")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for agent to receive prompt")
	}
}

// ---------------------------------------------------------------------------
// #27 E2E 权限交互：真实 ACP 协议 round-trip
// agent 通过 AgentSideConnection.RequestPermission 发起权限请求，
// client handler 接收并路由到 PermissionSignal，RespondPermission 解除阻塞。
// ---------------------------------------------------------------------------

// permissionAgent 在 Prompt 期间主动发起 RequestPermission。
type permissionAgent struct {
	mockAgent
	conn *acp.AgentSideConnection
}

func (a *permissionAgent) SetAgentConnection(c *acp.AgentSideConnection) { a.conn = c }

func (a *permissionAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	// 向 client 发起权限请求
	_, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: p.SessionId,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tc-e2e",
			Title:      strPtr("Run tests"),
		},
		Options: []acp.PermissionOption{
			{OptionId: "allow-e2e", Name: "Allow", Kind: "allow_once"},
			{OptionId: "reject-e2e", Name: "Deny", Kind: "reject_once"},
		},
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func strPtr(s string) *string { return &s }

func TestE2EPermissionRoundTrip(t *testing.T) {
	agent := &permissionAgent{
		mockAgent: mockAgent{newSessionID: "perm-session"},
	}

	drv := &mockDriver{
		t: t,
		agentHandler: func(_ *testing.T, stdin io.ReadCloser, stdout io.WriteCloser, stderr io.WriteCloser) {
			defer func() {
				stdin.Close()
				stdout.Close()
				stderr.Close()
			}()
			conn := acp.NewAgentSideConnection(agent, stdout, stdin)
			agent.SetAgentConnection(conn)
			<-conn.Done()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := New(ctx, drv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Close()

	_, err = cl.NewSession(ctx, "/tmp")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// 启动 prompt — 会阻塞在权限请求上
	promptErr := make(chan error, 1)
	go func() {
		_, err := cl.Prompt(ctx, "perm-session", []acp.ContentBlock{acp.TextBlock("hello")})
		promptErr <- err
	}()

	// 等权限请求到达 client handler
	select {
	case req := <-cl.PermissionSignal():
		if string(req.ToolCall.ToolCallId) != "tc-e2e" {
			t.Fatalf("expected tool_call_id=tc-e2e, got %s", req.ToolCall.ToolCallId)
		}
		if len(req.Options) != 2 {
			t.Fatalf("expected 2 options, got %d", len(req.Options))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for permission request")
	}

	// 回复权限请求
	if err := cl.RespondPermission("tc-e2e", acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected("allow-e2e"),
	}); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}

	// prompt 应该完成
	select {
	case err := <-promptErr:
		if err != nil {
			t.Fatalf("Prompt error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for prompt to complete")
	}
}

// ---------------------------------------------------------------------------
// #29 真实 npx 启动（条件跳过）
// ---------------------------------------------------------------------------

func TestE2ERealNpxStartup(t *testing.T) {
	if os.Getenv("ACP_BRIDGE_E2E") == "" {
		t.Skip("skipping E2E test; set ACP_BRIDGE_E2E=1 to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := &config.Config{
		CodexPath:      "npx",
		CodexArgs:      []string{"@agentclientprotocol/codex-acp"},
		DefaultTimeout: 30 * time.Second,
	}
	drv, err := driver.NewDriver(driver.AgentTypeCodex, cfg)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	cl, err := New(ctx, drv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Close()

	sessResp, err := cl.NewSession(ctx, "/tmp")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if string(sessResp.SessionId) == "" {
		t.Fatal("expected non-empty session ID")
	}
}
