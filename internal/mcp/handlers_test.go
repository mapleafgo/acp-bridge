package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

// ---------------------------------------------------------------------------
// mockAcpClient — test double for the acpClient interface
// ---------------------------------------------------------------------------

type mockAcpClient struct {
	mu sync.Mutex

	newSessionFn             func(ctx context.Context, cwd string) (*acp.NewSessionResponse, error)
	promptFn                 func(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResponse, error)
	cancelFn                 func(ctx context.Context, sessionID string) error
	closeSessionFn           func(ctx context.Context, sessionID string) (*acp.CloseSessionResponse, error)
	listSessionsFn           func(ctx context.Context) (*acp.ListSessionsResponse, error)
	loadSessionFn            func(ctx context.Context, sessionID string) (*acp.LoadSessionResponse, error)
	resumeSessionFn          func(ctx context.Context, sessionID string) (*acp.ResumeSessionResponse, error)
	setSessionModeFn         func(ctx context.Context, sessionID, modeID string) (*acp.SetSessionModeResponse, error)
	setSessionConfigOptionFn func(ctx context.Context, sessionID, configID, valueID string) error
	forkSessionFn            func(ctx context.Context, sessionID string) (*acp.UnstableForkSessionResponse, error)
	deleteSessionFn          func(ctx context.Context, sessionID string) (*acp.UnstableDeleteSessionResponse, error)
	respondPermissionFn      func(requestID string, resp acp.RequestPermissionResponse) error
	popUpdatesFn             func(sessionID string) []acp.SessionNotification
	peekUpdatesFn            func(sessionID string) []acp.SessionNotification

	permissionSignal chan acp.RequestPermissionRequest
	updates          map[string][]acp.SessionNotification
	permCh           map[string]chan acp.RequestPermissionResponse
}

func newMockAcpClient() *mockAcpClient {
	return &mockAcpClient{
		permissionSignal: make(chan acp.RequestPermissionRequest, 8),
		updates:          make(map[string][]acp.SessionNotification),
		permCh:           make(map[string]chan acp.RequestPermissionResponse),
	}
}

func (m *mockAcpClient) NewSession(ctx context.Context, cwd string) (*acp.NewSessionResponse, error) {
	if m.newSessionFn != nil {
		return m.newSessionFn(ctx, cwd)
	}
	return &acp.NewSessionResponse{SessionId: acp.SessionId("acp-sid-123")}, nil
}

func (m *mockAcpClient) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResponse, error) {
	if m.promptFn != nil {
		return m.promptFn(ctx, sessionID, blocks)
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (m *mockAcpClient) Cancel(ctx context.Context, sessionID string) error {
	if m.cancelFn != nil {
		return m.cancelFn(ctx, sessionID)
	}
	return nil
}

func (m *mockAcpClient) CloseSession(ctx context.Context, sessionID string) (*acp.CloseSessionResponse, error) {
	if m.closeSessionFn != nil {
		return m.closeSessionFn(ctx, sessionID)
	}
	return &acp.CloseSessionResponse{}, nil
}

func (m *mockAcpClient) ListSessions(ctx context.Context) (*acp.ListSessionsResponse, error) {
	if m.listSessionsFn != nil {
		return m.listSessionsFn(ctx)
	}
	return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
}

func (m *mockAcpClient) LoadSession(ctx context.Context, sessionID string) (*acp.LoadSessionResponse, error) {
	if m.loadSessionFn != nil {
		return m.loadSessionFn(ctx, sessionID)
	}
	return &acp.LoadSessionResponse{}, nil
}

func (m *mockAcpClient) ResumeSession(ctx context.Context, sessionID string) (*acp.ResumeSessionResponse, error) {
	if m.resumeSessionFn != nil {
		return m.resumeSessionFn(ctx, sessionID)
	}
	return &acp.ResumeSessionResponse{}, nil
}

func (m *mockAcpClient) SetSessionMode(ctx context.Context, sessionID, modeID string) (*acp.SetSessionModeResponse, error) {
	if m.setSessionModeFn != nil {
		return m.setSessionModeFn(ctx, sessionID, modeID)
	}
	return &acp.SetSessionModeResponse{}, nil
}

func (m *mockAcpClient) SetSessionConfigOption(ctx context.Context, sessionID, configID, valueID string) error {
	if m.setSessionConfigOptionFn != nil {
		return m.setSessionConfigOptionFn(ctx, sessionID, configID, valueID)
	}
	return nil
}

func (m *mockAcpClient) ForkSession(ctx context.Context, sessionID string) (*acp.UnstableForkSessionResponse, error) {
	if m.forkSessionFn != nil {
		return m.forkSessionFn(ctx, sessionID)
	}
	return &acp.UnstableForkSessionResponse{SessionId: acp.SessionId("forked-sid")}, nil
}

func (m *mockAcpClient) DeleteSession(ctx context.Context, sessionID string) (*acp.UnstableDeleteSessionResponse, error) {
	if m.deleteSessionFn != nil {
		return m.deleteSessionFn(ctx, sessionID)
	}
	return &acp.UnstableDeleteSessionResponse{}, nil
}

func (m *mockAcpClient) RespondPermission(requestID string, resp acp.RequestPermissionResponse) error {
	if m.respondPermissionFn != nil {
		return m.respondPermissionFn(requestID, resp)
	}
	m.mu.Lock()
	ch, ok := m.permCh[requestID]
	if ok {
		delete(m.permCh, requestID)
	}
	m.mu.Unlock()
	if ok {
		ch <- resp
	}
	return nil
}

func (m *mockAcpClient) PopUpdates(sessionID string) []acp.SessionNotification {
	if m.popUpdatesFn != nil {
		return m.popUpdatesFn(sessionID)
	}
	m.mu.Lock()
	updates := m.updates[sessionID]
	delete(m.updates, sessionID)
	m.mu.Unlock()
	return updates
}

func (m *mockAcpClient) PeekUpdates(sessionID string) []acp.SessionNotification {
	if m.peekUpdatesFn != nil {
		return m.peekUpdatesFn(sessionID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.updates[sessionID]
	out := make([]acp.SessionNotification, len(src))
	copy(out, src)
	return out
}

func (m *mockAcpClient) PermissionSignal() <-chan acp.RequestPermissionRequest {
	return m.permissionSignal
}

func (m *mockAcpClient) pushUpdate(sid string, notif acp.SessionNotification) {
	m.mu.Lock()
	m.updates[sid] = append(m.updates[sid], notif)
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Test server helpers
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T, mock *mockAcpClient) *Server {
	t.Helper()
	cfg := config.Load()
	pool := session.NewPool(cfg, session.WithCleanupInterval(10*time.Minute))
	t.Cleanup(pool.Shutdown)

	srv := NewServer(cfg, pool)
	srv.clientFactory = func(_ context.Context, _ string) (acpClient, error) {
		return mock, nil
	}
	return srv
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestAcpChatNewSession(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate("acp-sid-123", acp.SessionNotification{
			SessionId: acp.SessionId("acp-sid-123"),
			Update:    acp.UpdateAgentMessageText("Hello from agent"),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	result, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "test prompt",
	})
	if err != nil {
		t.Fatalf("handleAcpChat error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful chat")
	}
	if out.Status != "completed" {
		t.Fatalf("expected status=completed, got %s", out.Status)
	}
	if out.AgentText != "Hello from agent" {
		t.Fatalf("expected agent_text='Hello from agent', got %s", out.AgentText)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("expected stop_reason=end_turn, got %s", out.StopReason)
	}
	if out.SessionID == "" {
		t.Fatal("expected non-empty session_id")
	}
	if !out.IsNew {
		t.Fatal("expected is_new=true for first turn")
	}
}

func TestAcpChatSessionNotFound(t *testing.T) {
	mock := newMockAcpClient()
	srv := newTestServer(t, mock)
	factoryCalls := 0
	srv.clientFactory = func(_ context.Context, _ string) (acpClient, error) {
		factoryCalls++
		return mock, nil
	}

	result, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "test",
		SessionID: "nonexistent",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for session not found")
	}
	if out.Status != "error" {
		t.Fatalf("expected status=error, got %s", out.Status)
	}
	if factoryCalls != 0 {
		t.Fatalf("missing session must not start an agent client, got %d factory calls", factoryCalls)
	}
}

func TestAcpChatPermissionRequired(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.permissionSignal <- acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "allow", Name: "Allow", Kind: "allow_once"},
				{OptionId: "deny", Name: "Deny", Kind: "reject_once"},
			},
			SessionId: acp.SessionId("acp-sid-123"),
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: acp.ToolCallId("tc-1"),
				Title:      strPtr("Run command"),
			},
		}
		permCh := make(chan acp.RequestPermissionResponse, 1)
		mock.mu.Lock()
		mock.permCh["tc-1"] = permCh
		mock.mu.Unlock()
		select {
		case <-permCh:
		case <-time.After(5 * time.Second):
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	result, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "do something that needs permission",
	})
	if err != nil {
		t.Fatalf("handleAcpChat error: %v", err)
	}
	if result.IsError {
		t.Fatal("IsError should be false for permission_required")
	}
	if out.Status != "permission_required" {
		t.Fatalf("expected status=permission_required, got %s", out.Status)
	}
	if out.RequestID != "tc-1" {
		t.Fatalf("expected request_id=tc-1, got %s", out.RequestID)
	}
	if out.Permission == nil {
		t.Fatal("expected permission object in result")
	}
	if out.Permission.ToolCallID != "tc-1" {
		t.Fatalf("expected tool_call_id=tc-1, got %s", out.Permission.ToolCallID)
	}
	if out.Permission.Title != "Run command" {
		t.Fatalf("expected title='Run command', got %s", out.Permission.Title)
	}
	if len(out.Permission.Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(out.Permission.Options))
	}
}

func TestAcpRespondWithOutcomeMapping(t *testing.T) {
	mock := newMockAcpClient()
	promptDone := make(chan struct{})
	var capturedResp acp.RequestPermissionResponse

	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.permissionSignal <- acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "allow-once", Name: "Allow", Kind: "allow_once"},
				{OptionId: "reject-once", Name: "Deny", Kind: "reject_once"},
			},
			SessionId: acp.SessionId("acp-sid-123"),
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: acp.ToolCallId("tc-1"),
			},
		}
		permCh := make(chan acp.RequestPermissionResponse, 1)
		mock.mu.Lock()
		mock.permCh["tc-1"] = permCh
		mock.mu.Unlock()
		resp := <-permCh
		capturedResp = resp
		close(promptDone)
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	mock.respondPermissionFn = func(requestID string, resp acp.RequestPermissionResponse) error {
		mock.mu.Lock()
		ch, ok := mock.permCh[requestID]
		if ok {
			delete(mock.permCh, requestID)
		}
		mock.mu.Unlock()
		if ok {
			ch <- resp
		}
		return nil
	}

	srv := newTestServer(t, mock)

	_, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "test",
	})
	if err != nil {
		t.Fatalf("handleAcpChat error: %v", err)
	}
	if chatOut.Status != "permission_required" {
		t.Fatalf("expected permission_required, got %s", chatOut.Status)
	}
	sid := chatOut.SessionID

	// Respond with outcome=allow — should map to option_id "allow-once".
	respondResult, respondOut, err := srv.handleAcpRespond(context.Background(), nil, acpRespondArgs{
		SessionID: sid,
		RequestID: "tc-1",
		Outcome:   "allow",
	})
	if err != nil {
		t.Fatalf("handleAcpRespond error: %v", err)
	}

	select {
	case <-promptDone:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not complete after acp_respond")
	}

	if respondResult.IsError {
		t.Fatal("unexpected IsError after successful respond")
	}
	if respondOut.Status != "completed" {
		t.Fatalf("expected status=completed after respond, got %s", respondOut.Status)
	}
	if respondOut.TurnID != chatOut.TurnID {
		t.Fatalf("respond must continue turn %q, got %q", chatOut.TurnID, respondOut.TurnID)
	}

	// Verify the outcome was mapped to the correct option_id.
	if capturedResp.Outcome.Selected == nil {
		t.Fatal("expected selected outcome")
	}
	if string(capturedResp.Outcome.Selected.OptionId) != "allow-once" {
		t.Fatalf("expected option_id=allow-once, got %s", capturedResp.Outcome.Selected.OptionId)
	}
}

func TestAcpRespondRejectsMismatchedRequestID(t *testing.T) {
	mock := newMockAcpClient()
	promptCtx := make(chan context.Context, 1)
	mock.promptFn = func(ctx context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.permissionSignal <- acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "allow-once", Name: "Allow", Kind: "allow_once"},
			},
			SessionId: acp.SessionId("acp-sid-123"),
			ToolCall:  acp.ToolCallUpdate{ToolCallId: acp.ToolCallId("tc-current")},
		}
		promptCtx <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
	respondCalls := 0
	mock.respondPermissionFn = func(_ string, _ acp.RequestPermissionResponse) error {
		respondCalls++
		return fmt.Errorf("must not be called")
	}

	srv := newTestServer(t, mock)
	_, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}
	if chatOut.Status != "permission_required" {
		t.Fatalf("expected permission_required, got %s", chatOut.Status)
	}

	result, out, err := srv.handleAcpRespond(context.Background(), nil, acpRespondArgs{
		SessionID: chatOut.SessionID,
		RequestID: "tc-stale",
		Outcome:   "allow",
	})
	if err != nil {
		t.Fatalf("respond error: %v", err)
	}
	if !result.IsError || out.Error != "permission request mismatch" {
		t.Fatalf("expected permission request mismatch, got result=%#v out=%#v", result, out)
	}
	if respondCalls != 0 {
		t.Fatalf("mismatched request must not be forwarded, got %d calls", respondCalls)
	}
	ctx := <-promptCtx
	if turn := srv.peekTurn(session.SessionID(chatOut.SessionID)); turn != nil {
		turn.cancel()
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("prompt context was not cancelled during cleanup")
	}
}

func TestAcpInterruptRequiresMatchingTurnAndRetainsSnapshot(t *testing.T) {
	t.Setenv("ACP_BRIDGE_DEFAULT_TIMEOUT", "20ms")
	mock := newMockAcpClient()
	mock.promptFn = func(ctx context.Context, sessionID string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sessionID, acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update:    acp.UpdateAgentMessageText("partial result"),
		})
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cancelCalls := 0
	mock.cancelFn = func(_ context.Context, sessionID string) error {
		cancelCalls++
		if sessionID != "acp-sid-123" {
			t.Fatalf("unexpected ACP session ID %q", sessionID)
		}
		return nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}
	if chatOut.Status != "running" {
		t.Fatalf("expected running, got %s", chatOut.Status)
	}

	result, missing, err := srv.handleAcpInterrupt(context.Background(), nil, acpTurnArgs{
		SessionID: chatOut.SessionID,
	})
	if err != nil {
		t.Fatalf("missing turn interrupt error: %v", err)
	}
	if !result.IsError || missing.Error != "turn_id is required" {
		t.Fatalf("expected turn_id required, got result=%#v out=%#v", result, missing)
	}
	if cancelCalls != 0 {
		t.Fatalf("missing turn must not be cancelled, got %d calls", cancelCalls)
	}

	result, mismatch, err := srv.handleAcpInterrupt(context.Background(), nil, acpTurnArgs{
		SessionID: chatOut.SessionID,
		TurnID:    "wrong",
	})
	if err != nil {
		t.Fatalf("mismatched interrupt error: %v", err)
	}
	if !result.IsError || mismatch.Error != "turn mismatch" {
		t.Fatalf("expected turn mismatch, got result=%#v out=%#v", result, mismatch)
	}
	if cancelCalls != 0 {
		t.Fatalf("wrong turn must not be cancelled, got %d calls", cancelCalls)
	}

	result, interrupted, err := srv.handleAcpInterrupt(context.Background(), nil, acpTurnArgs{
		SessionID: chatOut.SessionID,
		TurnID:    chatOut.TurnID,
	})
	if err != nil {
		t.Fatalf("interrupt error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected interrupt error: %q", interrupted.Error)
	}
	if interrupted.Status != "interrupted" {
		t.Fatalf("expected interrupted, got %s", interrupted.Status)
	}
	if interrupted.AgentText != "partial result" {
		t.Fatalf("expected partial snapshot, got %q", interrupted.AgentText)
	}
	if cancelCalls != 1 {
		t.Fatalf("expected one ACP Cancel call, got %d", cancelCalls)
	}

	for range 2 {
		progressResult, progress, progressErr := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
			SessionID: chatOut.SessionID,
			TurnID:    chatOut.TurnID,
		})
		if progressErr != nil {
			t.Fatalf("progress error: %v", progressErr)
		}
		if progressResult.IsError || progress.Status != "interrupted" || progress.AgentText != "partial result" {
			t.Fatalf("expected retained interrupted snapshot, got result=%#v out=%#v", progressResult, progress)
		}
	}
}

func TestAcpInterruptRejectsCompletedTurn(t *testing.T) {
	mock := newMockAcpClient()
	cancelCalls := 0
	mock.cancelFn = func(_ context.Context, _ string) error {
		cancelCalls++
		return nil
	}
	srv := newTestServer(t, mock)
	_, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}

	result, out, err := srv.handleAcpInterrupt(context.Background(), nil, acpTurnArgs{
		SessionID: chatOut.SessionID,
		TurnID:    chatOut.TurnID,
	})
	if err != nil {
		t.Fatalf("interrupt error: %v", err)
	}
	if !result.IsError || out.Error != "turn is not interruptible" {
		t.Fatalf("expected non-interruptible error, got result=%#v out=%#v", result, out)
	}
	if cancelCalls != 0 {
		t.Fatalf("completed turn must not call Cancel, got %d calls", cancelCalls)
	}
}

func TestAcpClose(t *testing.T) {
	mock := newMockAcpClient()
	closed := false
	mock.closeSessionFn = func(_ context.Context, _ string) (*acp.CloseSessionResponse, error) {
		closed = true
		return &acp.CloseSessionResponse{}, nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "hi"})
	sid := chatOut.SessionID

	result, out, err := srv.handleAcpClose(context.Background(), nil, acpSessionIDArgs{SessionID: sid})
	if err != nil {
		t.Fatalf("close error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful close")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if !closed {
		t.Fatal("expected CloseSession to be called")
	}

	// Session should be removed from pool — second close should error.
	result2, out2, err2 := srv.handleAcpClose(context.Background(), nil, acpSessionIDArgs{SessionID: sid})
	if err2 != nil {
		t.Fatalf("unexpected error on second close: %v", err2)
	}
	if !result2.IsError {
		t.Fatal("expected IsError when closing already-closed session")
	}
	if out2.Status != "error" {
		t.Fatalf("expected status=error, got %s", out2.Status)
	}
}

func TestAcpSessions(t *testing.T) {
	mock := newMockAcpClient()
	srv := newTestServer(t, mock)

	srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "one"})
	srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "two"})

	result, out, err := srv.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("sessions error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for sessions list")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if len(out.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(out.Sessions))
	}
}

func TestAcpSetMode(t *testing.T) {
	mock := newMockAcpClient()
	modeSet := ""
	title := "Configured session"
	mock.promptFn = func(_ context.Context, sessionID string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sessionID, acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update: acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: &title},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	mock.setSessionModeFn = func(_ context.Context, _, modeID string) (*acp.SetSessionModeResponse, error) {
		modeSet = modeID
		return &acp.SetSessionModeResponse{}, nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "hi"})
	sid := chatOut.SessionID

	result, out, err := srv.handleAcpSetMode(context.Background(), nil, acpSetModeArgs{
		SessionID: sid,
		Mode:      "accept-edits",
	})
	if err != nil {
		t.Fatalf("set mode error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful set mode")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if out.Title != title {
		t.Fatalf("expected session title %q, got %q", title, out.Title)
	}
	if modeSet != "accept-edits" {
		t.Fatalf("expected mode accept-edits, got %s", modeSet)
	}
}

func TestAcpSetConfig(t *testing.T) {
	mock := newMockAcpClient()
	var cfgID, valID string
	mock.setSessionConfigOptionFn = func(_ context.Context, _, configID, valueID string) error {
		cfgID = configID
		valID = valueID
		return nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "hi"})
	sid := chatOut.SessionID

	result, out, err := srv.handleAcpSetConfig(context.Background(), nil, acpSetConfigArgs{
		SessionID: sid,
		ConfigID:  "model",
		Value:     "gpt-4",
	})
	if err != nil {
		t.Fatalf("set config error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful set config")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if cfgID != "model" || valID != "gpt-4" {
		t.Fatalf("expected config=model value=gpt-4, got config=%s value=%s", cfgID, valID)
	}
}

func TestAcpForkSession(t *testing.T) {
	mock := newMockAcpClient()
	srv := newTestServer(t, mock)

	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "hi"})
	sid := chatOut.SessionID

	result, out, err := srv.handleAcpForkSession(context.Background(), nil, acpSessionIDArgs{SessionID: sid})
	if err != nil {
		t.Fatalf("fork error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful fork")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if out.SessionID == "" {
		t.Fatal("expected non-empty forked session_id")
	}
	if out.SessionID == sid {
		t.Fatal("forked session should have a different ID")
	}
}

func TestAcpLoadSession(t *testing.T) {
	mock := newMockAcpClient()
	loaded := false
	mock.loadSessionFn = func(_ context.Context, sid string) (*acp.LoadSessionResponse, error) {
		loaded = true
		if sid != "persisted-sid" {
			t.Errorf("expected persisted-sid, got %s", sid)
		}
		return &acp.LoadSessionResponse{}, nil
	}

	srv := newTestServer(t, mock)
	result, out, err := srv.handleAcpLoadSession(context.Background(), nil, acpLoadSessionArgs{
		SessionID: "persisted-sid",
	})
	if err != nil {
		t.Fatalf("load session error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful load")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if !loaded {
		t.Fatal("expected LoadSession to be called")
	}
}

func TestAcpListHistory(t *testing.T) {
	mock := newMockAcpClient()
	title1 := "My Session"
	mock.listSessionsFn = func(_ context.Context) (*acp.ListSessionsResponse, error) {
		return &acp.ListSessionsResponse{
			Sessions: []acp.SessionInfo{
				{SessionId: acp.SessionId("hist-1"), Cwd: "/tmp", Title: &title1},
			},
		}, nil
	}

	srv := newTestServer(t, mock)
	result, out, err := srv.handleAcpListHistory(context.Background(), nil, acpListHistoryArgs{})
	if err != nil {
		t.Fatalf("list history error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful list history")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	if out.Sessions[0].Title != "My Session" {
		t.Fatalf("expected title 'My Session', got %s", out.Sessions[0].Title)
	}
}

func TestAcpListHistoryError(t *testing.T) {
	mock := newMockAcpClient()
	mock.listSessionsFn = func(_ context.Context) (*acp.ListSessionsResponse, error) {
		return nil, fmt.Errorf("agent unavailable")
	}

	srv := newTestServer(t, mock)
	result, out, err := srv.handleAcpListHistory(context.Background(), nil, acpListHistoryArgs{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError for failed list history")
	}
	if out.Status != "error" {
		t.Fatalf("expected status=error, got %s", out.Status)
	}
}

func TestAcpResumeSession(t *testing.T) {
	mock := newMockAcpClient()
	resumed := false
	mock.resumeSessionFn = func(_ context.Context, sid string) (*acp.ResumeSessionResponse, error) {
		resumed = true
		if sid != "acp-sid-123" {
			t.Errorf("expected acp-sid-123, got %s", sid)
		}
		return &acp.ResumeSessionResponse{}, nil
	}

	srv := newTestServer(t, mock)
	// First create a session via chat.
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "hi"})
	sid := chatOut.SessionID

	// Resume operates on the existing bridge session.
	result, out, err := srv.handleAcpResumeSession(context.Background(), nil, acpSessionIDArgs{SessionID: sid})
	if err != nil {
		t.Fatalf("resume session error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful resume")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if !resumed {
		t.Fatal("expected ResumeSession to be called")
	}
}

func TestAcpDeleteSession(t *testing.T) {
	mock := newMockAcpClient()
	deleted := false
	mock.deleteSessionFn = func(_ context.Context, sid string) (*acp.UnstableDeleteSessionResponse, error) {
		deleted = true
		if sid != "persisted-sid" {
			t.Errorf("expected persisted-sid, got %s", sid)
		}
		return &acp.UnstableDeleteSessionResponse{}, nil
	}

	srv := newTestServer(t, mock)
	result, out, err := srv.handleAcpDeleteSession(context.Background(), nil, acpDeleteSessionArgs{
		SessionID: "persisted-sid",
	})
	if err != nil {
		t.Fatalf("delete session error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected IsError for successful delete")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}
	if !deleted {
		t.Fatal("expected DeleteSession to be called")
	}
}

func TestMapOutcome(t *testing.T) {
	opts := []acp.PermissionOption{
		{OptionId: "allow-once", Kind: "allow_once"},
		{OptionId: "reject-once", Kind: "reject_once"},
	}

	// allow → first option with kind starting "allow"
	out := mapOutcome("allow", opts)
	if out.Selected == nil || string(out.Selected.OptionId) != "allow-once" {
		t.Fatalf("expected allow-once, got %+v", out)
	}

	// deny → first option with kind starting "reject"
	out = mapOutcome("deny", opts)
	if out.Selected == nil || string(out.Selected.OptionId) != "reject-once" {
		t.Fatalf("expected reject-once, got %+v", out)
	}

	// unknown → cancelled
	out = mapOutcome("maybe", opts)
	if out.Cancelled == nil {
		t.Fatal("expected cancelled outcome for unknown input")
	}
}

func TestAcpChatBusyGuard(t *testing.T) {
	mock := newMockAcpClient()
	// Block the prompt goroutine so the session stays in prompting state.
	blockCh := make(chan struct{})
	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		<-blockCh
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)

	// Start first chat — it will block.
	go func() {
		srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "first"})
	}()

	// Wait for the session to enter prompting state.
	time.Sleep(100 * time.Millisecond)

	// Find the session ID from the pool.
	summaries := srv.pool.List()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session, got %d", len(summaries))
	}
	busySid := string(summaries[0].ID)

	// Attempt a second chat on the same session — should be rejected.
	result, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "second",
		SessionID: busySid,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError for busy session")
	}
	if out.Status != "error" {
		t.Fatalf("expected status=error, got %s", out.Status)
	}

	// Unblock and cleanup.
	close(blockCh)
	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// acp_progress 测试
// ---------------------------------------------------------------------------

func TestAcpProgressRequiresMatchingTurnID(t *testing.T) {
	srv := newTestServer(t, newMockAcpClient())
	_, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}

	result, out, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: chatOut.SessionID,
	})
	if err != nil {
		t.Fatalf("progress without turn_id: %v", err)
	}
	if !result.IsError || out.Error != "turn_id is required" {
		t.Fatalf("expected turn_id required error, got result=%#v out=%#v", result, out)
	}

	result, out, err = srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: chatOut.SessionID,
		TurnID:    "wrong",
	})
	if err != nil {
		t.Fatalf("progress with wrong turn_id: %v", err)
	}
	if !result.IsError || out.Error != "turn mismatch" {
		t.Fatalf("expected turn mismatch, got result=%#v out=%#v", result, out)
	}
}

func TestAcpProgressCompletedIsRepeatableUntilNextTurn(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sessionID string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sessionID, acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update:    acp.UpdateAgentMessageText("stable result"),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	srv := newTestServer(t, mock)

	_, first, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "first"})
	if err != nil {
		t.Fatalf("first chat error: %v", err)
	}
	_, again, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: first.SessionID,
		TurnID:    first.TurnID,
	})
	if err != nil {
		t.Fatalf("first progress error: %v", err)
	}
	_, repeated, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: first.SessionID,
		TurnID:    first.TurnID,
	})
	if err != nil {
		t.Fatalf("repeated progress error: %v", err)
	}
	if again.Status != "completed" || repeated.Status != "completed" {
		t.Fatalf("expected completed snapshots, got %#v and %#v", again, repeated)
	}
	if again.AgentText != "stable result" || repeated.AgentText != again.AgentText {
		t.Fatalf("expected stable result, got %#v and %#v", again, repeated)
	}

	_, second, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "second",
		SessionID: first.SessionID,
	})
	if err != nil {
		t.Fatalf("second chat error: %v", err)
	}
	if second.TurnID == first.TurnID {
		t.Fatalf("next chat must create a new turn_id, got %q", second.TurnID)
	}

	result, stale, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: first.SessionID,
		TurnID:    first.TurnID,
	})
	if err != nil {
		t.Fatalf("stale progress error: %v", err)
	}
	if !result.IsError || stale.Error != "turn mismatch" {
		t.Fatalf("expected stale turn mismatch, got result=%#v out=%#v", result, stale)
	}
}

func TestAcpRunningResultPersistsTitle(t *testing.T) {
	t.Setenv("ACP_BRIDGE_DEFAULT_TIMEOUT", "20ms")
	mock := newMockAcpClient()
	unblock := make(chan struct{})
	title := "Hermes task"
	mock.promptFn = func(_ context.Context, sessionID string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sessionID, acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update: acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: &title},
			},
		})
		<-unblock
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	srv := newTestServer(t, mock)

	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}
	if out.Status != "running" {
		t.Fatalf("expected running, got %s", out.Status)
	}
	if out.Title != title {
		t.Fatalf("expected running title %q, got %q", title, out.Title)
	}
	sessions := srv.pool.List()
	if len(sessions) != 1 || sessions[0].Title != title {
		t.Fatalf("expected persisted title, got %#v", sessions)
	}
	close(unblock)
}

// TestAcpStatusDuringPrompt 验证 acp_chat 阻塞期间能查到实时进度。
func TestAcpStatusDuringPrompt(t *testing.T) {
	mock := newMockAcpClient()
	step1Ch := make(chan struct{})
	step2Ch := make(chan struct{})
	step3Ch := make(chan struct{})
	chatDoneCh := make(chan chatResultJSON, 1)

	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update:    acp.UpdateAgentMessageText("Thinking about..."),
		})
		close(step1Ch)
		<-step2Ch
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc-running",
					Title:      "Running tests",
				},
			},
		})
		// 等主测试读完 status 再返回
		<-step3Ch
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)

	go func() {
		_, out, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "run tests"})
		chatDoneCh <- out
	}()

	select {
	case <-step1Ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for agent first output")
	}

	summaries := srv.pool.List()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session, got %d", len(summaries))
	}
	sid := string(summaries[0].ID)
	turn := srv.peekTurn(summaries[0].ID)
	if turn == nil {
		t.Fatal("expected current turn")
	}
	turnID := turn.turnID

	progressResult, statusOut, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: sid,
		TurnID:    turnID,
	})
	if err != nil {
		t.Fatalf("acp_progress error: %v", err)
	}
	if progressResult.IsError {
		t.Fatal("unexpected IsError for acp_progress")
	}
	if statusOut.AgentText != "Thinking about..." {
		t.Fatalf("expected agent_text='Thinking about...', got %q", statusOut.AgentText)
	}
	if statusOut.State != "prompting" {
		t.Fatalf("expected state=prompting, got %s", statusOut.State)
	}

	close(step2Ch)
	// 等 tool call update 被 push
	time.Sleep(50 * time.Millisecond)

	_, statusOut2, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: sid,
		TurnID:    turnID,
	})
	if err != nil {
		t.Fatalf("second acp_progress error: %v", err)
	}
	if len(statusOut2.ToolCalls) == 0 {
		t.Fatal("expected tool_calls in status")
	}
	if statusOut2.ToolCalls[0].ID != "tc-running" {
		t.Fatalf("expected tool_call_id=tc-running, got %s", statusOut2.ToolCalls[0].ID)
	}
	if statusOut2.AgentText != "Thinking about..." {
		t.Fatalf("agent_text should persist, got %q", statusOut2.AgentText)
	}

	// 让 agent 返回
	close(step3Ch)
	select {
	case chatOut := <-chatDoneCh:
		if chatOut.Status != "completed" {
			t.Fatalf("expected chat status=completed, got %s", chatOut.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for chat to complete")
	}
}

func TestAcpStatusSessionNotFound(t *testing.T) {
	mock := newMockAcpClient()
	srv := newTestServer(t, mock)

	result, _, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{SessionID: "ghost"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError for nonexistent session")
	}
}

// ---------------------------------------------------------------------------
// 多轮对话测试
// ---------------------------------------------------------------------------

func TestAcpChatMultiTurn(t *testing.T) {
	mock := newMockAcpClient()

	srv := newTestServer(t, mock)

	// 第一轮：创建新 session
	_, out1, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "hello",
	})
	if err != nil {
		t.Fatalf("first chat error: %v", err)
	}
	if out1.Status != "completed" {
		t.Fatalf("expected completed, got %s", out1.Status)
	}
	if !out1.IsNew {
		t.Fatal("first turn should be is_new=true")
	}
	sid := out1.SessionID

	// 第二轮：用同一个 session_id 继续
	result2, out2, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "what did I just say?",
		SessionID: sid,
	})
	if err != nil {
		t.Fatalf("second chat error: %v", err)
	}
	if result2.IsError {
		t.Fatal("second chat should succeed")
	}
	if out2.Status != "completed" {
		t.Fatalf("expected completed, got %s", out2.Status)
	}
	if out2.IsNew {
		t.Fatal("second turn should be is_new=false")
	}
	if out2.SessionID != sid {
		t.Fatalf("session_id should persist: expected %s, got %s", sid, out2.SessionID)
	}
	if out2.TurnCount < 2 {
		t.Fatalf("turn_count should be >= 2 after second turn, got %d", out2.TurnCount)
	}
}

func TestAcpChatContinuationUsesStoredAgentAndSession(t *testing.T) {
	codex := newMockAcpClient()
	claude := newMockAcpClient()
	var promptSessionIDs []string
	claude.promptFn = func(_ context.Context, sessionID string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		promptSessionIDs = append(promptSessionIDs, sessionID)
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, codex)
	var factoryAgentTypes []string
	srv.clientFactory = func(_ context.Context, agentType string) (acpClient, error) {
		factoryAgentTypes = append(factoryAgentTypes, agentType)
		switch agentType {
		case "codex":
			return codex, nil
		case "claude":
			return claude, nil
		default:
			return nil, fmt.Errorf("unexpected agent type %q", agentType)
		}
	}

	_, first, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "first",
		AgentType: "claude",
		CWD:       "/original",
	})
	if err != nil {
		t.Fatalf("first chat error: %v", err)
	}
	if first.Status != "completed" {
		t.Fatalf("expected first chat completed, got %s", first.Status)
	}

	result, second, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "second",
		SessionID: first.SessionID,
		AgentType: "unknown",
		CWD:       "/ignored",
	})
	if err != nil {
		t.Fatalf("continuation error: %v", err)
	}
	if result.IsError {
		t.Fatalf("continuation should use stored agent, got error %q", second.Error)
	}
	if second.Status != "completed" {
		t.Fatalf("expected continuation completed, got %s", second.Status)
	}
	if len(factoryAgentTypes) != 1 || factoryAgentTypes[0] != "claude" {
		t.Fatalf("expected only stored claude client lookup, got %v", factoryAgentTypes)
	}
	if len(promptSessionIDs) != 2 || promptSessionIDs[0] != "acp-sid-123" || promptSessionIDs[1] != "acp-sid-123" {
		t.Fatalf("expected stored ACP session on both prompts, got %v", promptSessionIDs)
	}
	sess, err := srv.pool.Get(session.SessionID(first.SessionID))
	if err != nil {
		t.Fatalf("get bridge session: %v", err)
	}
	if sess.CWD != "/original" {
		t.Fatalf("continuation must retain cwd, got %q", sess.CWD)
	}
}

// ---------------------------------------------------------------------------
// deny 路径测试
// ---------------------------------------------------------------------------

func TestAcpRespondDeny(t *testing.T) {
	mock := newMockAcpClient()
	promptDone := make(chan struct{})
	var capturedResp acp.RequestPermissionResponse

	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.permissionSignal <- acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "allow-once", Name: "Allow", Kind: "allow_once"},
				{OptionId: "reject-once", Name: "Deny", Kind: "reject_once"},
			},
			SessionId: acp.SessionId("acp-sid-123"),
			ToolCall:  acp.ToolCallUpdate{ToolCallId: acp.ToolCallId("tc-deny")},
		}
		permCh := make(chan acp.RequestPermissionResponse, 1)
		mock.mu.Lock()
		mock.permCh["tc-deny"] = permCh
		mock.mu.Unlock()
		capturedResp = <-permCh
		close(promptDone)
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	mock.respondPermissionFn = func(requestID string, resp acp.RequestPermissionResponse) error {
		mock.mu.Lock()
		ch, ok := mock.permCh[requestID]
		if ok {
			delete(mock.permCh, requestID)
		}
		mock.mu.Unlock()
		if ok {
			ch <- resp
		}
		return nil
	}

	srv := newTestServer(t, mock)

	_, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "do something risky"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}
	if chatOut.Status != "permission_required" {
		t.Fatalf("expected permission_required, got %s", chatOut.Status)
	}

	// 用 deny 回应
	_, respondOut, err := srv.handleAcpRespond(context.Background(), nil, acpRespondArgs{
		SessionID: chatOut.SessionID,
		RequestID: "tc-deny",
		Outcome:   "deny",
	})
	if err != nil {
		t.Fatalf("respond error: %v", err)
	}

	select {
	case <-promptDone:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not complete after deny")
	}

	// deny 后 agent 继续执行直到完成
	if respondOut.Status != "completed" {
		t.Fatalf("expected completed after deny, got %s", respondOut.Status)
	}

	// 验证 outcome 被正确映射为 reject-once
	if capturedResp.Outcome.Selected == nil {
		t.Fatal("expected selected outcome")
	}
	if string(capturedResp.Outcome.Selected.OptionId) != "reject-once" {
		t.Fatalf("expected option_id=reject-once, got %s", capturedResp.Outcome.Selected.OptionId)
	}
}

// ---------------------------------------------------------------------------
// prompt 进行中 close 测试
// ---------------------------------------------------------------------------

func TestAcpCloseDuringPrompt(t *testing.T) {
	mock := newMockAcpClient()

	// prompt 会一直阻塞直到被 cancel
	promptCancelled := make(chan struct{})
	mock.promptFn = func(ctx context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		<-ctx.Done()
		close(promptCancelled)
		return nil, ctx.Err()
	}
	mock.cancelFn = func(_ context.Context, _ string) error {
		return nil
	}
	mock.closeSessionFn = func(_ context.Context, _ string) (*acp.CloseSessionResponse, error) {
		return &acp.CloseSessionResponse{}, nil
	}

	srv := newTestServer(t, mock)

	// 启动 chat（会阻塞）
	chatDone := make(chan struct{})
	go func() {
		srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "long running task"})
		close(chatDone)
	}()

	// 等 session 进入 prompting 状态
	time.Sleep(100 * time.Millisecond)

	summaries := srv.pool.List()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session, got %d", len(summaries))
	}
	sid := string(summaries[0].ID)

	// 在 prompt 进行中调用 close
	result, out, err := srv.handleAcpClose(context.Background(), nil, acpSessionIDArgs{SessionID: sid})
	if err != nil {
		t.Fatalf("close error: %v", err)
	}
	if result.IsError {
		t.Fatal("close should succeed even during prompt")
	}
	if out.Status != "ok" {
		t.Fatalf("expected status=ok, got %s", out.Status)
	}

	// session 应从池中移除
	if _, err := srv.pool.Get(session.SessionID(sid)); err == nil {
		t.Fatal("session should be removed after close")
	}

	// chat goroutine 应该被解除阻塞
	select {
	case <-chatDone:
	case <-time.After(5 * time.Second):
		t.Fatal("chat goroutine did not unblock after close")
	}
}

// ---------------------------------------------------------------------------
// Turn 生命周期显式化测试
// ---------------------------------------------------------------------------

func TestAcpChatReturnsTurnID(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "test",
	})
	if err != nil {
		t.Fatalf("handleAcpChat error: %v", err)
	}
	if out.TurnID == "" {
		t.Fatal("expected non-empty turn_id")
	}
	if !strings.HasPrefix(out.TurnID, "t-") {
		t.Fatalf("expected turn_id prefix 't-', got %s", out.TurnID)
	}
}

func TestAcpChatPermissionReturnsTurnID(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.permissionSignal <- acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "allow", Name: "Allow", Kind: "allow_once"},
			},
			SessionId: acp.SessionId("acp-sid-123"),
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-1"},
		}
		permCh := make(chan acp.RequestPermissionResponse, 1)
		mock.mu.Lock()
		mock.permCh["tc-1"] = permCh
		mock.mu.Unlock()
		<-permCh
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if chatOut.TurnID == "" {
		t.Fatal("expected turn_id in permission_required result")
	}
}

// ---------------------------------------------------------------------------
// 事件粒度细化测试
// ---------------------------------------------------------------------------

func TestBuildChatResultReasoningText(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update:    acp.UpdateAgentMessageText("Here is the answer"),
		})
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update:    acp.UpdateAgentThoughtText("Thinking step by step"),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if out.AgentText != "Here is the answer" {
		t.Fatalf("expected agent_text, got %q", out.AgentText)
	}
	if out.Reasoning != "Thinking step by step" {
		t.Fatalf("expected reasoning text, got %q", out.Reasoning)
	}
}

func TestBuildChatResultToolCallKindAndStatus(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc-1",
					Title:      "Reading file",
					Kind:       acp.ToolKindRead,
					Status:     acp.ToolCallStatusInProgress,
				},
			},
		})
		// Update status to completed
		completed := acp.ToolCallStatusCompleted
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "tc-1",
					Status:     &completed,
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out.ToolCalls))
	}
	tc := out.ToolCalls[0]
	if tc.Kind != "read" {
		t.Fatalf("expected kind=read, got %s", tc.Kind)
	}
	if tc.Status != "completed" {
		t.Fatalf("expected status=completed, got %s", tc.Status)
	}
}

func TestBuildChatResultUsageUpdate(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				UsageUpdate: &acp.SessionUsageUpdate{
					Used: 1500,
					Size: 128000,
					Cost: &acp.Cost{
						Amount:   0.05,
						Currency: "USD",
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if out.Usage == nil {
		t.Fatal("expected usage data")
	}
	if out.Usage.UsedTokens != 1500 {
		t.Fatalf("expected used_tokens=1500, got %d", out.Usage.UsedTokens)
	}
	if out.Usage.TotalTokens != 128000 {
		t.Fatalf("expected total_tokens=128000, got %d", out.Usage.TotalTokens)
	}
	if out.Usage.Cost != 0.05 {
		t.Fatalf("expected cost=0.05, got %f", out.Usage.Cost)
	}
	if out.Usage.Currency != "USD" {
		t.Fatalf("expected currency=USD, got %s", out.Usage.Currency)
	}
}

func TestAcpStatusReasoningText(t *testing.T) {
	mock := newMockAcpClient()
	step1Ch := make(chan struct{})
	step2Ch := make(chan struct{})

	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update:    acp.UpdateAgentThoughtText("Reasoning about the task"),
		})
		close(step1Ch)
		<-step2Ch
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)

	go func() {
		srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	}()

	select {
	case <-step1Ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for agent output")
	}

	summaries := srv.pool.List()
	sid := string(summaries[0].ID)
	turn := srv.peekTurn(summaries[0].ID)
	if turn == nil {
		t.Fatal("expected current turn")
	}

	_, statusOut, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: sid,
		TurnID:    turn.turnID,
	})
	if err != nil {
		t.Fatalf("acp_progress error: %v", err)
	}
	if statusOut.Reasoning != "Reasoning about the task" {
		t.Fatalf("expected reasoning text in status, got %q", statusOut.Reasoning)
	}
	if statusOut.TurnID == "" {
		t.Fatal("expected turn_id in status during prompt")
	}

	close(step2Ch)
}

// ---------------------------------------------------------------------------
// Session 元数据 CWD 测试
// ---------------------------------------------------------------------------

func TestAcpSessionsIncludesCWD(t *testing.T) {
	mock := newMockAcpClient()
	srv := newTestServer(t, mock)

	srv.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt: "hi",
		CWD:    "/home/user/project",
	})

	_, out, err := srv.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("sessions error: %v", err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	if out.Sessions[0].Cwd != "/home/user/project" {
		t.Fatalf("expected cwd=/home/user/project, got %s", out.Sessions[0].Cwd)
	}
}

// ---------------------------------------------------------------------------
// Plan 捕获测试
// ---------------------------------------------------------------------------

func TestBuildChatResultPlanSteps(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.UpdatePlan(
				acp.PlanEntry{
					Content:  "Read the config file",
					Priority: acp.PlanEntryPriorityHigh,
					Status:   acp.PlanEntryStatusCompleted,
				},
				acp.PlanEntry{
					Content:  "Modify the handler",
					Priority: acp.PlanEntryPriorityMedium,
					Status:   acp.PlanEntryStatusInProgress,
				},
				acp.PlanEntry{
					Content:  "Run tests",
					Priority: acp.PlanEntryPriorityLow,
					Status:   acp.PlanEntryStatusPending,
				},
			),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.Plan) != 3 {
		t.Fatalf("expected 3 plan steps, got %d", len(out.Plan))
	}
	if out.Plan[0].Status != "completed" {
		t.Fatalf("expected step 0 status=completed, got %s", out.Plan[0].Status)
	}
	if out.Plan[1].Status != "in_progress" {
		t.Fatalf("expected step 1 status=in_progress, got %s", out.Plan[1].Status)
	}
	if out.Plan[2].Priority != "low" {
		t.Fatalf("expected step 2 priority=low, got %s", out.Plan[2].Priority)
	}
}

func TestAcpStatusPlanSteps(t *testing.T) {
	mock := newMockAcpClient()
	step1Ch := make(chan struct{})
	step2Ch := make(chan struct{})

	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.UpdatePlan(acp.PlanEntry{
				Content: "Working on it",
				Status:  acp.PlanEntryStatusInProgress,
			}),
		})
		close(step1Ch)
		<-step2Ch
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	go func() {
		srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	}()

	select {
	case <-step1Ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	sid := string(srv.pool.List()[0].ID)
	turn := srv.peekTurn(session.SessionID(sid))
	if turn == nil {
		t.Fatal("expected current turn")
	}
	_, statusOut, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: sid,
		TurnID:    turn.turnID,
	})
	if err != nil {
		t.Fatalf("status error: %v", err)
	}
	if len(statusOut.Plan) != 1 {
		t.Fatalf("expected 1 plan step in status, got %d", len(statusOut.Plan))
	}
	if statusOut.Plan[0].Content != "Working on it" {
		t.Fatalf("unexpected plan content: %s", statusOut.Plan[0].Content)
	}
	close(step2Ch)
}

// ---------------------------------------------------------------------------
// 文件变更（diff）捕获测试
// ---------------------------------------------------------------------------

func TestBuildChatResultFileChanges(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		// 新文件创建
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc-edit",
					Title:      "Editing files",
					Kind:       acp.ToolKindEdit,
					Status:     acp.ToolCallStatusCompleted,
					Content: []acp.ToolCallContent{
						acp.ToolDiffContent("src/new.go", "package main"),
						acp.ToolDiffContent("src/existing.go", "modified", "original"),
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "edit files"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.FileChanges) != 2 {
		t.Fatalf("expected 2 file changes, got %d", len(out.FileChanges))
	}
	// 第一项是新文件
	found := false
	for _, fc := range out.FileChanges {
		if fc.Path == "src/new.go" {
			if fc.Kind != "created" {
				t.Fatalf("expected kind=created for new file, got %s", fc.Kind)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected src/new.go in file changes")
	}
	// 第二项是修改
	found = false
	for _, fc := range out.FileChanges {
		if fc.Path == "src/existing.go" {
			if fc.Kind != "modified" {
				t.Fatalf("expected kind=modified for existing file, got %s", fc.Kind)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected src/existing.go in file changes")
	}
}

func TestFileChangeDeduplication(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		// 同一文件两次 diff（tool_call + tool_call_update）
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc-dup",
					Title:      "Editing",
					Content: []acp.ToolCallContent{
						acp.ToolDiffContent("main.go", "v2", "v1"),
					},
				},
			},
		})
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "tc-dup",
					Content: []acp.ToolCallContent{
						acp.ToolDiffContent("main.go", "v3", "v2"),
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "edit"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.FileChanges) != 1 {
		t.Fatalf("expected 1 deduplicated file change, got %d", len(out.FileChanges))
	}
}

// ---------------------------------------------------------------------------
// Session title 跟踪测试
// ---------------------------------------------------------------------------

func TestSessionTitleFromUpdate(t *testing.T) {
	mock := newMockAcpClient()
	titleStr := "Refactoring module"
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
					Title: &titleStr,
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "refactor"})

	_, out, err := srv.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("sessions error: %v", err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	if out.Sessions[0].Title != "Refactoring module" {
		t.Fatalf("expected title 'Refactoring module', got %q", out.Sessions[0].Title)
	}
}

func TestSessionTitleEmptyUpdateDoesNotOverwriteSameTurn(t *testing.T) {
	mock := newMockAcpClient()
	title := "Stable title"
	emptyTitle := ""
	mock.promptFn = func(_ context.Context, sessionID string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sessionID, acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update: acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: &title},
			},
		})
		mock.pushUpdate(sessionID, acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update: acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: &emptyTitle},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}
	if out.Title != title {
		t.Fatalf("empty title update must not overwrite %q, got %q", title, out.Title)
	}
	sessions := srv.pool.List()
	if len(sessions) != 1 || sessions[0].Title != title {
		t.Fatalf("expected retained session title, got %#v", sessions)
	}
}

// ---------------------------------------------------------------------------
// RawInput / RawOutput 捕获测试
// ---------------------------------------------------------------------------

func TestBuildChatResultRawInputOutput(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc-raw",
					Title:      "Running tests",
					Kind:       acp.ToolKindExecute,
					Status:     acp.ToolCallStatusInProgress,
					RawInput:   map[string]any{"command": "go test ./..."},
				},
			},
		})
		// Update with raw output when completed
		completed := acp.ToolCallStatusCompleted
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "tc-raw",
					Status:     &completed,
					RawOutput:  "PASS\nok  example.com/pkg  0.5s",
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "run tests"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out.ToolCalls))
	}
	tc := out.ToolCalls[0]
	// RawInput should be captured from the initial ToolCall
	if tc.RawInput == nil {
		t.Fatal("expected raw_input to be captured")
	}
	// RawOutput should be captured from the ToolCallUpdate
	if tc.RawOutput == nil {
		t.Fatal("expected raw_output to be captured")
	}
	if tc.RawOutput != "PASS\nok  example.com/pkg  0.5s" {
		t.Fatalf("unexpected raw_output: %v", tc.RawOutput)
	}
}

// ---------------------------------------------------------------------------
// CurrentMode 持久化测试
// ---------------------------------------------------------------------------

func TestCurrentModePersistence(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				CurrentModeUpdate: &acp.SessionCurrentModeUpdate{
					CurrentModeId: "accept-edits",
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})

	_, out, err := srv.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("sessions error: %v", err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	if out.Sessions[0].CurrentMode != "accept-edits" {
		t.Fatalf("expected current_mode=accept-edits, got %q", out.Sessions[0].CurrentMode)
	}
}

// ---------------------------------------------------------------------------
// PlanUpdate / PlanRemoved 测试
// ---------------------------------------------------------------------------

func TestBuildChatResultPlanUpdate(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		// 初始 plan
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.UpdatePlan(
				acp.PlanEntry{Content: "Step A", Status: acp.PlanEntryStatusPending},
				acp.PlanEntry{Content: "Step B", Status: acp.PlanEntryStatusPending},
			),
		})
		// 增量更新：Step A 完成
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				PlanUpdate: &acp.SessionPlanUpdate{
					Plan: acp.PlanUpdateContent{
						Items: &acp.PlanUpdateContentItems{
							Id: "plan-1",
							Entries: []acp.PlanEntry{
								{Content: "Step A", Status: acp.PlanEntryStatusCompleted},
								{Content: "Step B", Status: acp.PlanEntryStatusInProgress},
							},
						},
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.Plan) != 2 {
		t.Fatalf("expected 2 plan steps after update, got %d", len(out.Plan))
	}
	if out.Plan[0].Status != "completed" {
		t.Fatalf("expected step 0 status=completed after PlanUpdate, got %s", out.Plan[0].Status)
	}
	if out.Plan[1].Status != "in_progress" {
		t.Fatalf("expected step 1 status=in_progress after PlanUpdate, got %s", out.Plan[1].Status)
	}
}

func TestBuildChatResultPlanRemoved(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.UpdatePlan(acp.PlanEntry{
				Content: "Temporary plan",
				Status:  acp.PlanEntryStatusInProgress,
			}),
		})
		// Plan 被移除
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				PlanRemoved: &acp.SessionUpdatePlanRemoved{
					Id: "plan-1",
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.Plan) != 0 {
		t.Fatalf("expected 0 plan steps after PlanRemoved, got %d", len(out.Plan))
	}
}

func TestBuildChatResultPlanUpdateMarkdown(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				PlanUpdate: &acp.SessionPlanUpdate{
					Plan: acp.PlanUpdateContent{
						Markdown: &acp.PlanUpdateContentMarkdown{
							Id:      "plan-md",
							Content: "# Plan\n1. Do X\n2. Do Y",
						},
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, out, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(out.Plan) != 1 {
		t.Fatalf("expected 1 markdown plan step, got %d", len(out.Plan))
	}
	if !strings.Contains(out.Plan[0].Content, "# Plan") {
		t.Fatalf("expected markdown content, got %q", out.Plan[0].Content)
	}
}

// ---------------------------------------------------------------------------
// UserMessageChunk 捕获测试
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ConfigOptionUpdate 通过 acp_session_info 查看（持久化到 session）
// ---------------------------------------------------------------------------

func TestAcpSessionInfoConfigOptions(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
					ConfigOptions: []acp.SessionConfigOption{
						{Select: &acp.SessionConfigOptionSelect{
							Id:           "model",
							Name:         "Model",
							CurrentValue: "gpt-4",
						}},
						{Boolean: &acp.SessionConfigOptionBoolean{
							Id:           "stream_thoughts",
							Name:         "Stream Thoughts",
							CurrentValue: true,
						}},
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})

	_, info, err := srv.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{SessionID: chatOut.SessionID})
	if err != nil {
		t.Fatalf("session_info error: %v", err)
	}
	if len(info.ConfigOptions) != 2 {
		t.Fatalf("expected 2 config options, got %d", len(info.ConfigOptions))
	}
	if info.ConfigOptions[0].Type != "select" {
		t.Fatalf("expected type=select, got %s", info.ConfigOptions[0].Type)
	}
	if info.ConfigOptions[0].Value != "gpt-4" {
		t.Fatalf("expected value=gpt-4, got %s", info.ConfigOptions[0].Value)
	}
	if info.ConfigOptions[1].Type != "boolean" {
		t.Fatalf("expected type=boolean, got %s", info.ConfigOptions[1].Type)
	}
	if info.ConfigOptions[1].Value != "true" {
		t.Fatalf("expected value=true, got %s", info.ConfigOptions[1].Value)
	}
}

// ---------------------------------------------------------------------------
// AvailableCommandsUpdate 通过 acp_session_info 查看（持久化到 session）
// ---------------------------------------------------------------------------

func TestAcpSessionInfoAvailableCommands(t *testing.T) {
	mock := newMockAcpClient()
	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
					AvailableCommands: []acp.AvailableCommand{
						{Name: "plan", Description: "Create an execution plan"},
						{
							Name:        "research",
							Description: "Research the codebase",
							Input: &acp.AvailableCommandInput{
								Unstructured: &acp.UnstructuredCommandInput{
									Hint: "query to search for",
								},
							},
						},
					},
				},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)
	_, chatOut, _ := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})

	_, info, err := srv.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{SessionID: chatOut.SessionID})
	if err != nil {
		t.Fatalf("session_info error: %v", err)
	}
	if len(info.AvailableCommands) != 2 {
		t.Fatalf("expected 2 available commands, got %d", len(info.AvailableCommands))
	}
	if info.AvailableCommands[0].Name != "plan" {
		t.Fatalf("expected name=plan, got %s", info.AvailableCommands[0].Name)
	}
	if info.AvailableCommands[0].Description != "Create an execution plan" {
		t.Fatalf("unexpected description: %s", info.AvailableCommands[0].Description)
	}
	if info.AvailableCommands[1].InputHint != "query to search for" {
		t.Fatalf("expected input_hint, got %q", info.AvailableCommands[1].InputHint)
	}
}

// ---------------------------------------------------------------------------
// 超时异步流程测试
// ---------------------------------------------------------------------------

func TestAcpChatTimeoutThenProgressCompletes(t *testing.T) {
	t.Setenv("ACP_BRIDGE_DEFAULT_TIMEOUT", "100ms")

	mock := newMockAcpClient()
	unblockCh := make(chan struct{})

	mock.promptFn = func(_ context.Context, sid string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		mock.pushUpdate(sid, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update:    acp.UpdateAgentMessageText("Partial output"),
		})
		<-unblockCh
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	srv := newTestServer(t, mock)

	// 用极短超时启动 chat，应立即返回 running
	chatResult, chatOut, err := srv.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "test"})
	if err != nil {
		t.Fatalf("chat error: %v", err)
	}
	if chatOut.Status != "running" {
		t.Fatalf("expected status=running (timeout), got %s", chatOut.Status)
	}
	if chatOut.SessionID == "" {
		t.Fatal("expected session_id even on timeout")
	}
	if chatOut.TurnID == "" {
		t.Fatal("expected turn_id even on timeout")
	}
	if chatOut.AgentText != "Partial output" {
		t.Fatalf("expected partial agent_text, got %q", chatOut.AgentText)
	}
	_ = chatResult
	sid := chatOut.SessionID

	// session 应仍在 prompting 状态
	sess, _ := srv.pool.Get(session.SessionID(sid))
	if sess.State != session.StatePrompting {
		t.Fatalf("expected state=prompting, got %s", sess.State)
	}

	// acp_progress 检查进度
	_, progOut, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: sid,
		TurnID:    chatOut.TurnID,
	})
	if err != nil {
		t.Fatalf("progress error: %v", err)
	}
	if progOut.Status != "running" {
		t.Fatalf("expected status=running, got %s", progOut.Status)
	}
	if progOut.AgentText != "Partial output" {
		t.Fatalf("expected partial text in progress, got %q", progOut.AgentText)
	}

	// 解除阻塞，让 prompt 完成
	close(unblockCh)
	time.Sleep(100 * time.Millisecond)

	// acp_progress 应检测到完成
	_, doneOut, err := srv.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: sid,
		TurnID:    chatOut.TurnID,
	})
	if err != nil {
		t.Fatalf("final progress error: %v", err)
	}
	if doneOut.Status != "completed" {
		t.Fatalf("expected status=completed, got %s", doneOut.Status)
	}
	if doneOut.AgentText != "Partial output" {
		t.Fatalf("expected full agent_text, got %q", doneOut.AgentText)
	}

	// session 应回到 idle
	sess2, _ := srv.pool.Get(session.SessionID(sid))
	if sess2.State != session.StateIdle {
		t.Fatalf("expected state=idle after completion, got %s", sess2.State)
	}
}
