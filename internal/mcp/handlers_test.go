package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/instance"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

type testACPClient struct {
	mu sync.Mutex

	next              int
	firstSessionID    string
	done              chan struct{}
	closeOnce         sync.Once
	loadCalls         []string
	resumeCalls       []string
	deleteCalls       []string
	closeSessionCalls []string
	promptFn          func(context.Context, string, []acp.ContentBlock) (*acp.PromptResponse, error)
	updates           map[string][]acp.SessionNotification
	permissionEvents  map[string]chan client.PermissionEvent
}

func newTestACPClient() *testACPClient {
	return &testACPClient{
		firstSessionID:   "thread:child",
		done:             make(chan struct{}),
		updates:          make(map[string][]acp.SessionNotification),
		permissionEvents: make(map[string]chan client.PermissionEvent),
	}
}

func (c *testACPClient) NewSession(context.Context, string) (*acp.NewSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("session-%d", c.next)
	if c.next == 1 {
		id = c.firstSessionID
	}
	return &acp.NewSessionResponse{
		SessionId: acp.SessionId(id),
		Modes: &acp.SessionModeState{
			CurrentModeId:  "default",
			AvailableModes: []acp.SessionMode{},
		},
		ConfigOptions: []acp.SessionConfigOption{{
			Select: &acp.SessionConfigOptionSelect{
				Id:           "model",
				Name:         "Model",
				Type:         "select",
				CurrentValue: "gpt-5",
			},
		}},
	}, nil
}

func (c *testACPClient) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResponse, error) {
	if c.promptFn != nil {
		return c.promptFn(ctx, sessionID, blocks)
	}
	c.mu.Lock()
	title := "演示会话"
	c.updates[sessionID] = append(c.updates[sessionID],
		acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update: acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: &title},
			},
		},
		acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update: acp.SessionUpdate{
				AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
					AvailableCommands: []acp.AvailableCommand{
						{Name: "/plan", Description: "制定计划"},
					},
				},
			},
		},
		acp.SessionNotification{
			SessionId: acp.SessionId(sessionID),
			Update:    acp.UpdateAgentMessageText("完成"),
		},
	)
	c.mu.Unlock()
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (c *testACPClient) Cancel(context.Context, string) error { return nil }
func (c *testACPClient) CloseSession(_ context.Context, sessionID string) (*acp.CloseSessionResponse, error) {
	c.mu.Lock()
	c.closeSessionCalls = append(c.closeSessionCalls, sessionID)
	c.mu.Unlock()
	return &acp.CloseSessionResponse{}, nil
}
func (c *testACPClient) ListSessions(context.Context) (*acp.ListSessionsResponse, error) {
	title := "历史会话"
	return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{
		{SessionId: "hist:one", Cwd: "/tmp", Title: &title},
	}}, nil
}
func (c *testACPClient) LoadSession(_ context.Context, sessionID, _ string) (*acp.LoadSessionResponse, error) {
	c.mu.Lock()
	c.loadCalls = append(c.loadCalls, sessionID)
	c.mu.Unlock()
	return &acp.LoadSessionResponse{}, nil
}
func (c *testACPClient) ResumeSession(_ context.Context, sessionID, _ string) (*acp.ResumeSessionResponse, error) {
	c.mu.Lock()
	c.resumeCalls = append(c.resumeCalls, sessionID)
	c.mu.Unlock()
	return &acp.ResumeSessionResponse{}, nil
}
func (c *testACPClient) SetSessionMode(context.Context, string, string) (*acp.SetSessionModeResponse, error) {
	return &acp.SetSessionModeResponse{}, nil
}
func (c *testACPClient) SetSessionConfigOption(context.Context, string, string, string) error {
	return nil
}
func (c *testACPClient) ForkSession(context.Context, string, string) (*acp.UnstableForkSessionResponse, error) {
	return &acp.UnstableForkSessionResponse{SessionId: "forked"}, nil
}
func (c *testACPClient) DeleteSession(_ context.Context, sessionID string) (*acp.UnstableDeleteSessionResponse, error) {
	c.mu.Lock()
	c.deleteCalls = append(c.deleteCalls, sessionID)
	c.mu.Unlock()
	return &acp.UnstableDeleteSessionResponse{}, nil
}
func (c *testACPClient) RespondPermission(string, string, acp.RequestPermissionResponse) error {
	return nil
}
func (c *testACPClient) PopUpdates(sessionID string) []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	updates := append([]acp.SessionNotification(nil), c.updates[sessionID]...)
	delete(c.updates, sessionID)
	return updates
}
func (c *testACPClient) PeekUpdates(sessionID string) []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acp.SessionNotification(nil), c.updates[sessionID]...)
}
func (c *testACPClient) PermissionEvents(sessionID string) <-chan client.PermissionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	events := c.permissionEvents[sessionID]
	if events == nil {
		events = make(chan client.PermissionEvent, 8)
		c.permissionEvents[sessionID] = events
	}
	return events
}
func (c *testACPClient) ForgetSession(string)  {}
func (c *testACPClient) Done() <-chan struct{} { return c.done }
func (c *testACPClient) Err() error            { return nil }
func (c *testACPClient) Close(context.Context) error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

type testFactory struct {
	mu      sync.Mutex
	clients map[driver.AgentType]*testACPClient
}

func newTestFactory() *testFactory {
	return &testFactory{clients: make(map[driver.AgentType]*testACPClient)}
}

func (f *testFactory) New(_ context.Context, agentType driver.AgentType) (instance.ACPClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing := f.clients[agentType]; existing != nil {
		return nil, errors.New("duplicate instance start")
	}
	cl := newTestACPClient()
	f.clients[agentType] = cl
	return cl, nil
}

func newTestServer(t *testing.T, maxSessions int) (*Server, *instance.Manager, *testFactory) {
	t.Helper()
	cfg := &config.Config{MaxSessions: maxSessions, DefaultTimeout: time.Second}
	factory := newTestFactory()
	manager := instance.NewManager(cfg, factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return NewServer(cfg, manager), manager, factory
}

func TestAcpChatReturnsQualifiedAgentSessionID(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	result, out, err := server.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "处理任务"})
	if err != nil || result.IsError {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
	if out.SessionID != "codex:thread:child" {
		t.Fatalf("session_id=%q", out.SessionID)
	}
	if out.AgentText != "完成" || out.Status != "completed" {
		t.Fatalf("unexpected result: %#v", out)
	}
}

func TestAcpProgressWithoutTurnIDReturnsIdle(t *testing.T) {
	server, manager, _ := newTestServer(t, 10)
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	result, out, err := server.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: created.ID.String(),
	})
	if err != nil || result.IsError || out.Status != "idle" {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
}

func TestAcpProgressWithWrongTurnIDReturnsMismatch(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	_, chat, _ := server.handleAcpChat(context.Background(), nil, acpChatArgs{Prompt: "hello"})
	result, out, err := server.handleAcpProgress(context.Background(), nil, acpProgressArgs{
		SessionID: chat.SessionID,
		TurnID:    "wrong",
	})
	if err != nil || !result.IsError || out.Error != session.ErrTurnMismatch.Error() {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
}

func TestAcpInterruptRequiresTurnID(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	result, out, err := server.handleAcpInterrupt(context.Background(), nil, acpTurnArgs{
		SessionID: "codex:any",
	})
	if err != nil || !result.IsError || out.Error != "turn_id is required" {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
}

func TestAcpRespondValidatesRequiredFields(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	tests := []struct {
		name string
		args acpRespondArgs
		want string
	}{
		{name: "request ID", args: acpRespondArgs{SessionID: "codex:any", Outcome: "allow"}, want: "request_id is required"},
		{name: "outcome", args: acpRespondArgs{SessionID: "codex:any", RequestID: "request"}, want: "outcome must be allow or deny"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, out, err := server.handleAcpRespond(context.Background(), nil, tt.args)
			if err != nil || !result.IsError || out.Error != tt.want {
				t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
			}
		})
	}
}

func TestAcpSessionsReturnsAllItems(t *testing.T) {
	server, manager, _ := newTestServer(t, 0)
	for range 12 {
		if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}
	result, out, err := server.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil || result.IsError || len(out.Sessions) != 12 {
		t.Fatalf("result=%#v sessions=%d err=%v", result, len(out.Sessions), err)
	}
	for _, item := range out.Sessions {
		if item.SessionID == "" {
			t.Fatal("session_id must not be empty")
		}
		if item.TurnID != "" || item.TurnStatus != "" {
			t.Fatalf("idle session should not expose turn fields: %#v", item)
		}
	}
}

func TestAcpSessionsExposesTurnNavigationFields(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10, DefaultTimeout: 30 * time.Millisecond}
	factory := newTestFactory()
	manager := instance.NewManager(cfg, factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	server := NewServer(cfg, manager)
	started := make(chan struct{})
	release := make(chan struct{})
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	cl := factory.clients[driver.AgentTypeCodex]
	cl.promptFn = func(ctx context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		close(started)
		<-release
		<-ctx.Done()
		return nil, ctx.Err()
	}

	chatDone := make(chan chatResultJSON, 1)
	go func() {
		_, out, _ := server.handleAcpChat(context.Background(), nil, acpChatArgs{
			Prompt:    "长时间任务",
			SessionID: created.ID.String(),
		})
		chatDone <- out
	}()
	<-started

	_, listed, err := server.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 {
		t.Fatalf("sessions=%d", len(listed.Sessions))
	}
	item := listed.Sessions[0]
	if item.TurnID == "" || item.TurnStatus != string(session.TurnRunning) || item.State != string(session.StatePrompting) {
		t.Fatalf("running list item=%#v", item)
	}
	turnID := item.TurnID

	chat := <-chatDone
	if chat.TurnID != turnID || chat.Status != string(instance.StatusRunning) {
		t.Fatalf("chat result=%#v, want running turn %s", chat, turnID)
	}

	_, interrupted, err := server.handleAcpInterrupt(context.Background(), nil, acpTurnArgs{
		SessionID: created.ID.String(),
		TurnID:    turnID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != string(instance.StatusInterrupted) {
		t.Fatalf("interrupt result=%#v", interrupted)
	}
	close(release)

	_, listed, err = server.handleAcpSessions(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	item = listed.Sessions[0]
	if item.TurnID != turnID || item.TurnStatus != string(session.TurnInterrupted) || item.State != string(session.StateIdle) {
		t.Fatalf("interrupted list item=%#v", item)
	}
}

func TestAcpListHistoryDefaultsToCodexAndQualifiesIDs(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	result, out, err := server.handleAcpListHistory(context.Background(), nil, acpListHistoryArgs{})
	if err != nil || result.IsError {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != "codex:hist:one" {
		t.Fatalf("unexpected history: %#v", out.Sessions)
	}
}

func TestAcpLoadDerivesAgentTypeFromQualifiedID(t *testing.T) {
	server, _, factory := newTestServer(t, 10)
	result, loaded, err := server.handleAcpLoadSession(context.Background(), nil, acpLoadSessionArgs{
		SessionID: "claude:persisted:one",
		CWD:       "/tmp",
	})
	if err != nil || result.IsError || loaded.SessionID != "claude:persisted:one" {
		t.Fatalf("result=%#v out=%#v err=%v", result, loaded, err)
	}
	cl := factory.clients[driver.AgentTypeClaude]
	if !reflect.DeepEqual(cl.loadCalls, []string{"persisted:one"}) {
		t.Fatalf("load=%v", cl.loadCalls)
	}
}

func TestAcpDeleteDerivesAgentTypeAndRejectsActiveSession(t *testing.T) {
	server, manager, factory := newTestServer(t, 10)
	result, _, err := server.handleAcpDeleteSession(context.Background(), nil, acpDeleteSessionArgs{
		SessionID: "claude:persisted:one",
	})
	if err != nil || result.IsError {
		t.Fatalf("delete result=%#v err=%v", result, err)
	}
	cl := factory.clients[driver.AgentTypeClaude]
	if !reflect.DeepEqual(cl.deleteCalls, []string{"persisted:one"}) {
		t.Fatalf("delete=%v", cl.deleteCalls)
	}

	created, createErr := manager.CreateSession(context.Background(), driver.AgentTypeClaude, "/tmp")
	if createErr != nil {
		t.Fatal(createErr)
	}
	result, out, err := server.handleAcpDeleteSession(context.Background(), nil, acpDeleteSessionArgs{
		SessionID: created.ID.String(),
	})
	if err != nil || !result.IsError || out.Error != instance.ErrSessionActive.Error() {
		t.Fatalf("active delete result=%#v out=%#v err=%v", result, out, err)
	}
}

func TestAcpForkAndCloseSession(t *testing.T) {
	server, manager, factory := newTestServer(t, 10)
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	result, forked, err := server.handleAcpForkSession(context.Background(), nil, acpSessionIDArgs{
		SessionID: created.ID.String(),
	})
	if err != nil || result.IsError || forked.SessionID != "codex:forked" {
		t.Fatalf("fork result=%#v out=%#v err=%v", result, forked, err)
	}
	result, _, err = server.handleAcpClose(context.Background(), nil, acpSessionIDArgs{
		SessionID: forked.SessionID,
	})
	if err != nil || result.IsError {
		t.Fatalf("close result=%#v err=%v", result, err)
	}
	cl := factory.clients[driver.AgentTypeCodex]
	if !containsString(cl.closeSessionCalls, "forked") {
		t.Fatalf("close calls=%v", cl.closeSessionCalls)
	}
}

func TestAcpSessionInfoRequiresValidSessionID(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	result, out, err := server.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{})
	if err != nil || !result.IsError || out.Error != "session_id is required" {
		t.Fatalf("empty id result=%#v out=%#v err=%v", result, out, err)
	}
	result, out, err = server.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{SessionID: "not-qualified"})
	if err != nil || !result.IsError || out.Status != "error" {
		t.Fatalf("invalid id result=%#v out=%#v err=%v", result, out, err)
	}
}

func TestAcpSessionInfoNotFound(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	result, out, err := server.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{
		SessionID: "codex:missing",
	})
	if err != nil || !result.IsError || out.Status != "error" || out.Error == "" {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
}

func TestAcpSessionInfoReturnsSessionMetadata(t *testing.T) {
	server, manager, _ := newTestServer(t, 10)
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp/demo")
	if err != nil {
		t.Fatal(err)
	}

	result, out, err := server.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{
		SessionID: created.ID.String(),
	})
	if err != nil || result.IsError {
		t.Fatalf("result=%#v out=%#v err=%v", result, out, err)
	}
	if out.Status != "ok" || out.SessionID != created.ID.String() || out.AgentType != "codex" {
		t.Fatalf("identity fields: %#v", out)
	}
	if out.State != string(session.StateIdle) || out.Cwd != "/tmp/demo" || out.CurrentMode != "default" {
		t.Fatalf("basic fields: %#v", out)
	}
	if len(out.ConfigOptions) != 1 || out.ConfigOptions[0].ID != "model" || out.ConfigOptions[0].Value != "gpt-5" {
		t.Fatalf("config_options: %#v", out.ConfigOptions)
	}

	if _, err := manager.SetMode(context.Background(), created.ID.String(), "acceptEdits"); err != nil {
		t.Fatal(err)
	}
	_, chat, err := server.handleAcpChat(context.Background(), nil, acpChatArgs{
		Prompt:    "整理元数据",
		SessionID: created.ID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if chat.Status != string(instance.StatusCompleted) {
		t.Fatalf("chat status=%s", chat.Status)
	}

	deadline := time.Now().Add(time.Second)
	for {
		result, out, err = server.handleAcpSessionInfo(context.Background(), nil, acpSessionIDArgs{
			SessionID: created.ID.String(),
		})
		if err != nil || result.IsError {
			t.Fatalf("after chat result=%#v out=%#v err=%v", result, out, err)
		}
		if out.CurrentMode == "acceptEdits" && out.Title == "演示会话" && out.TurnCount == 1 && out.State == string(session.StateIdle) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updated fields not ready: %#v", out)
		}
		time.Sleep(time.Millisecond)
	}
	if len(out.AvailableCommands) != 1 || out.AvailableCommands[0].Name != "/plan" {
		t.Fatalf("available_commands: %#v", out.AvailableCommands)
	}
}

func TestMCPArgumentSchemasUseFinalContracts(t *testing.T) {
	progressType := reflect.TypeFor[acpProgressArgs]()
	turnID, _ := progressType.FieldByName("TurnID")
	if got := turnID.Tag.Get("json"); got != "turn_id,omitempty" {
		t.Fatalf("TurnID json tag=%q", got)
	}
	itemType := reflect.TypeFor[sessionListItem]()
	sessionID, _ := itemType.FieldByName("SessionID")
	if got := sessionID.Tag.Get("json"); got != "session_id" {
		t.Fatalf("SessionID json tag=%q", got)
	}
	turnIDField, ok := itemType.FieldByName("TurnID")
	if !ok || turnIDField.Tag.Get("json") != "turn_id,omitempty" {
		t.Fatal("sessionListItem must expose turn_id")
	}
	if _, ok := reflect.TypeFor[acpListHistoryArgs]().FieldByName("CWD"); ok {
		t.Fatal("acp_list_history must not expose cwd")
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
