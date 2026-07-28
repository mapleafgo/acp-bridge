package instance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

type fakeClient struct {
	mu          sync.Mutex
	next        int
	done        chan struct{}
	closeOne    sync.Once
	promptFn    func(context.Context, string, []acp.ContentBlock) (*acp.PromptResponse, error)
	cancelErr   error
	cancelCalls int
}

func newFakeClient() *fakeClient {
	return &fakeClient{done: make(chan struct{})}
}

func (c *fakeClient) NewSession(context.Context, string) (*acp.NewSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return &acp.NewSessionResponse{SessionId: acp.SessionId(fmt.Sprintf("session-%d", c.next))}, nil
}
func (c *fakeClient) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResponse, error) {
	if c.promptFn != nil {
		return c.promptFn(ctx, sessionID, blocks)
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}
func (c *fakeClient) Cancel(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelCalls++
	return c.cancelErr
}
func (c *fakeClient) CloseSession(context.Context, string) (*acp.CloseSessionResponse, error) {
	return &acp.CloseSessionResponse{}, nil
}
func (c *fakeClient) ListSessions(context.Context) (*acp.ListSessionsResponse, error) {
	return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
}
func (c *fakeClient) LoadSession(context.Context, string, string) (*acp.LoadSessionResponse, error) {
	return &acp.LoadSessionResponse{}, nil
}
func (c *fakeClient) ResumeSession(context.Context, string, string) (*acp.ResumeSessionResponse, error) {
	return &acp.ResumeSessionResponse{}, nil
}
func (c *fakeClient) SetSessionMode(context.Context, string, string) (*acp.SetSessionModeResponse, error) {
	return &acp.SetSessionModeResponse{}, nil
}
func (c *fakeClient) SetSessionConfigOption(context.Context, string, string, string) error {
	return nil
}
func (c *fakeClient) ForkSession(context.Context, string, string) (*acp.UnstableForkSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return &acp.UnstableForkSessionResponse{SessionId: acp.SessionId(fmt.Sprintf("fork-%d", c.next))}, nil
}
func (c *fakeClient) DeleteSession(context.Context, string) (*acp.UnstableDeleteSessionResponse, error) {
	return &acp.UnstableDeleteSessionResponse{}, nil
}
func (c *fakeClient) RespondPermission(string, string, acp.RequestPermissionResponse) error {
	return nil
}
func (c *fakeClient) PopUpdates(string) []acp.SessionNotification  { return nil }
func (c *fakeClient) PeekUpdates(string) []acp.SessionNotification { return nil }
func (c *fakeClient) PermissionEvents(string) <-chan client.PermissionEvent {
	return make(chan client.PermissionEvent)
}
func (c *fakeClient) ForgetSession(string)  {}
func (c *fakeClient) Done() <-chan struct{} { return c.done }
func (c *fakeClient) Close(context.Context) error {
	c.closeOne.Do(func() { close(c.done) })
	return nil
}

type fakeFactory struct {
	mu      sync.Mutex
	starts  map[driver.AgentType]int
	clients map[driver.AgentType]*fakeClient
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{
		starts:  make(map[driver.AgentType]int),
		clients: make(map[driver.AgentType]*fakeClient),
	}
}

func (f *fakeFactory) New(_ context.Context, agentType driver.AgentType) (ACPClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts[agentType]++
	c := newFakeClient()
	f.clients[agentType] = c
	return c, nil
}

func (f *fakeFactory) Starts(agentType driver.AgentType) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[agentType]
}

func testConfig(max int) *config.Config {
	return &config.Config{MaxSessions: max}
}

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
	factory := newFakeFactory()
	manager := NewManager(testConfig(2), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	first, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	second, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("expected limit error, got %v", err)
	}
	if got := len(manager.Sessions()); got != 2 || first.ID == second.ID {
		t.Fatalf("existing sessions changed: %#v", manager.Sessions())
	}
}

func TestZeroSessionInstanceStaysAlive(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseSession(context.Background(), created.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
		t.Fatal(err)
	}
	if got := factory.Starts(driver.AgentTypeCodex); got != 1 {
		t.Fatalf("instance restarted after reaching zero sessions: %d", got)
	}
}

func TestMaxSessionsZeroIsUnlimited(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(0), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	for range 20 {
		if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(manager.Sessions()); got != 20 {
		t.Fatalf("sessions=%d, want 20", got)
	}
}

func TestAgentExitRemovesOnlyItsSessions(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
		t.Fatal(err)
	}
	claude, err := manager.CreateSession(context.Background(), driver.AgentTypeClaude, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	factory.clients[driver.AgentTypeCodex].closeOne.Do(func() {
		close(factory.clients[driver.AgentTypeCodex].done)
	})
	deadline := time.Now().Add(time.Second)
	for len(manager.Sessions()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	views := manager.Sessions()
	if len(views) != 1 || views[0].ID.String() != claude.ID.String() {
		t.Fatalf("unexpected remaining sessions: %#v", views)
	}
}

func TestProgressWithoutTurnReturnsIdle(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	view, err := manager.Progress(created.ID.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusIdle || view.Turn.ID != "" {
		t.Fatalf("unexpected idle progress: %#v", view)
	}
}

func TestProgressOptionalTurnIDValidatesWhenPresent(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if _, err := manager.Chat(context.Background(), created.ID.String(), "hello", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Progress(created.ID.String(), "wrong"); !errors.Is(err, session.ErrTurnMismatch) {
		t.Fatalf("expected turn mismatch, got %v", err)
	}
}

func TestHandlerCancellationCommitsInterruptedSnapshot(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	cl := factory.clients[driver.AgentTypeCodex]
	started := make(chan struct{})
	cl.promptFn = func(ctx context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := manager.Chat(ctx, created.ID.String(), "wait", time.Minute)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	view, err := manager.Progress(created.ID.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusInterrupted {
		t.Fatalf("status=%q, want interrupted", view.Status)
	}
}

func TestInterruptCancelFailureStillReturnsInterrupted(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, _ := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	cl := factory.clients[driver.AgentTypeCodex]
	cl.cancelErr = errors.New("cancel failed")
	cl.promptFn = func(ctx context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	running, err := manager.Chat(context.Background(), created.ID.String(), "wait", 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := manager.Interrupt(context.Background(), created.ID.String(), running.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != StatusInterrupted {
		t.Fatalf("status=%q", interrupted.Status)
	}
}
