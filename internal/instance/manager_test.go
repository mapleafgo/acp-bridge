package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

type fakeClient struct {
	mu                sync.Mutex
	next              int
	done              chan struct{}
	closeOne          sync.Once
	processErr        error
	newSessionFn      func(context.Context, string) (*acp.NewSessionResponse, error)
	promptFn          func(context.Context, string, []acp.ContentBlock) (*acp.PromptResponse, error)
	forkFn            func(context.Context, string, string) (*acp.UnstableForkSessionResponse, error)
	loadFn            func(context.Context, string, string) (*acp.LoadSessionResponse, error)
	resumeFn          func(context.Context, string, string) (*acp.ResumeSessionResponse, error)
	deleteFn          func(context.Context, string) (*acp.UnstableDeleteSessionResponse, error)
	listFn            func(context.Context) (*acp.ListSessionsResponse, error)
	closeSessionFn    func(context.Context, string) (*acp.CloseSessionResponse, error)
	setModeFn         func(context.Context, string, string) (*acp.SetSessionModeResponse, error)
	setConfigFn       func(context.Context, string, string, string) error
	closeFn           func(context.Context)
	cancelFn          func(context.Context)
	cancelErr         error
	cancelCalls       int
	closeSessionCalls []string
	deleteCalls       []string
}

func newFakeClient() *fakeClient {
	return &fakeClient{done: make(chan struct{})}
}

func (c *fakeClient) NewSession(ctx context.Context, cwd string) (*acp.NewSessionResponse, error) {
	if c.newSessionFn != nil {
		return c.newSessionFn(ctx, cwd)
	}
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
func (c *fakeClient) Cancel(ctx context.Context, _ string) error {
	if c.cancelFn != nil {
		c.cancelFn(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelCalls++
	return c.cancelErr
}
func (c *fakeClient) CloseSession(ctx context.Context, sessionID string) (*acp.CloseSessionResponse, error) {
	c.mu.Lock()
	c.closeSessionCalls = append(c.closeSessionCalls, sessionID)
	c.mu.Unlock()
	if c.closeSessionFn != nil {
		return c.closeSessionFn(ctx, sessionID)
	}
	return &acp.CloseSessionResponse{}, nil
}
func (c *fakeClient) ListSessions(ctx context.Context) (*acp.ListSessionsResponse, error) {
	if c.listFn != nil {
		return c.listFn(ctx)
	}
	return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
}
func (c *fakeClient) LoadSession(ctx context.Context, sessionID, cwd string) (*acp.LoadSessionResponse, error) {
	if c.loadFn != nil {
		return c.loadFn(ctx, sessionID, cwd)
	}
	return &acp.LoadSessionResponse{}, nil
}
func (c *fakeClient) ResumeSession(ctx context.Context, sessionID, cwd string) (*acp.ResumeSessionResponse, error) {
	if c.resumeFn != nil {
		return c.resumeFn(ctx, sessionID, cwd)
	}
	return &acp.ResumeSessionResponse{}, nil
}
func (c *fakeClient) SetSessionMode(ctx context.Context, sessionID, mode string) (*acp.SetSessionModeResponse, error) {
	if c.setModeFn != nil {
		return c.setModeFn(ctx, sessionID, mode)
	}
	return &acp.SetSessionModeResponse{}, nil
}
func (c *fakeClient) SetSessionConfigOption(ctx context.Context, sessionID, configID, value string) error {
	if c.setConfigFn != nil {
		return c.setConfigFn(ctx, sessionID, configID, value)
	}
	return nil
}
func (c *fakeClient) ForkSession(ctx context.Context, sessionID, cwd string) (*acp.UnstableForkSessionResponse, error) {
	if c.forkFn != nil {
		return c.forkFn(ctx, sessionID, cwd)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return &acp.UnstableForkSessionResponse{SessionId: acp.SessionId(fmt.Sprintf("fork-%d", c.next))}, nil
}
func (c *fakeClient) DeleteSession(ctx context.Context, sessionID string) (*acp.UnstableDeleteSessionResponse, error) {
	c.mu.Lock()
	c.deleteCalls = append(c.deleteCalls, sessionID)
	c.mu.Unlock()
	if c.deleteFn != nil {
		return c.deleteFn(ctx, sessionID)
	}
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
func (c *fakeClient) Err() error            { return c.processErr }
func (c *fakeClient) Close(ctx context.Context) error {
	if c.closeFn != nil {
		c.closeFn(ctx)
	}
	c.closeOne.Do(func() { close(c.done) })
	return nil
}

type emptyIDClient struct {
	*fakeClient
}

func (c *emptyIDClient) NewSession(context.Context, string) (*acp.NewSessionResponse, error) {
	return &acp.NewSessionResponse{}, nil
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

func TestConcurrentStartFailureIsSharedByWaiters(t *testing.T) {
	var (
		mu     sync.Mutex
		starts int
	)
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	var startOnce sync.Once
	factory := func(context.Context, driver.AgentType) (ACPClient, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		startOnce.Do(func() { close(factoryStarted) })
		<-releaseFactory
		return nil, errors.New("start failed")
	}
	manager := NewManager(testConfig(10), factory)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	startCalls := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCalls
			_, _ = manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
		}()
	}
	close(startCalls)
	<-factoryStarted
	time.Sleep(10 * time.Millisecond)
	close(releaseFactory)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts=%d, want 1", starts)
	}
}

func TestManagerCloseDuringInstanceStartReclaimsClient(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cl := newFakeClient()
	factory := func(context.Context, driver.AgentType) (ACPClient, error) {
		close(started)
		<-release
		return cl, nil
	}
	manager := NewManager(testConfig(10), factory)

	createDone := make(chan error, 1)
	go func() {
		_, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
		createDone <- err
	}()
	<-started

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.Close(context.Background())
	}()
	for {
		manager.mu.Lock()
		closing := manager.closing
		manager.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)

	if err := <-createDone; !errors.Is(err, ErrManagerClosing) {
		t.Fatalf("create error=%v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-cl.Done():
	default:
		t.Fatal("client was not closed")
	}
}

func TestManagerCloseWaitsForSessionCreationReservation(t *testing.T) {
	cl := newFakeClient()
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	cl.newSessionFn = func(context.Context, string) (*acp.NewSessionResponse, error) {
		close(createStarted)
		<-releaseCreate
		return &acp.NewSessionResponse{SessionId: "created-during-close"}, nil
	}
	clientCloseStarted := make(chan struct{})
	var closeOnce sync.Once
	cl.closeFn = func(context.Context) {
		closeOnce.Do(func() { close(clientCloseStarted) })
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})

	createDone := make(chan error, 1)
	go func() {
		_, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
		createDone <- err
	}()
	<-createStarted
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.Close(context.Background())
	}()
	clientClosedEarly := false
	select {
	case <-clientCloseStarted:
		clientClosedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseCreate)
	if err := <-createDone; !errors.Is(err, ErrManagerClosing) {
		t.Fatalf("expected ErrManagerClosing, got %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if clientClosedEarly {
		t.Fatal("client closed before the session creation reservation drained")
	}
}

func TestManagerCloseHonorsExpiredDeadlineWhileInterruptingTurn(t *testing.T) {
	cl := newFakeClient()
	cl.cancelFn = func(ctx context.Context) {
		<-ctx.Done()
	}
	started := make(chan struct{})
	release := make(chan struct{})
	cl.promptFn = func(context.Context, string, []acp.ContentBlock) (*acp.PromptResponse, error) {
		close(started)
		<-release
		return nil, context.Canceled
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Chat(context.Background(), created.ID.String(), "wait", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_ = manager.Close(ctx)
	elapsed := time.Since(start)
	close(release)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Close ignored expired deadline: %v", elapsed)
	}
}

func TestCloseMarksSessionClosingBeforeInterrupt(t *testing.T) {
	cl := newFakeClient()
	promptStarted := make(chan struct{})
	cl.promptFn = func(ctx context.Context, _ string, _ []acp.ContentBlock) (*acp.PromptResponse, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cancelStarted := make(chan struct{})
	releaseCancel := make(chan struct{})
	cl.cancelFn = func(context.Context) {
		close(cancelStarted)
		<-releaseCancel
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Chat(context.Background(), created.ID.String(), "wait", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-promptStarted

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseSession(context.Background(), created.ID.String())
	}()
	<-cancelStarted
	if _, err := manager.Chat(context.Background(), created.ID.String(), "new", time.Millisecond); !errors.Is(err, session.ErrSessionClosed) {
		close(releaseCancel)
		t.Fatalf("expected ErrSessionClosed while interrupting, got %v", err)
	}
	close(releaseCancel)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCloseRejectsSecondRequest(t *testing.T) {
	cl := newFakeClient()
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var startOnce sync.Once
	cl.closeSessionFn = func(context.Context, string) (*acp.CloseSessionResponse, error) {
		startOnce.Do(func() { close(closeStarted) })
		<-releaseClose
		return &acp.CloseSessionResponse{}, nil
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- manager.CloseSession(context.Background(), created.ID.String())
	}()
	<-closeStarted
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- manager.CloseSession(context.Background(), created.ID.String())
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, session.ErrSessionClosed) {
			close(releaseClose)
			t.Fatalf("expected ErrSessionClosed, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(releaseClose)
		<-firstDone
		err := <-secondDone
		t.Fatalf("second close reached agent instead of being rejected: %v", err)
	}
	close(releaseClose)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	cl.mu.Lock()
	closeCalls := append([]string(nil), cl.closeSessionCalls...)
	cl.mu.Unlock()
	if len(closeCalls) != 1 {
		t.Fatalf("remote close calls=%d, want 1", len(closeCalls))
	}
}

func TestInterruptKeepsSessionBusyUntilControllerExits(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := manager.session(created.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	turn := session.NewTurn("manual", func() {})
	if err := ref.session.BeginTurn(turn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, interrupted := manager.interrupt(ctx, ref, turn, "test"); !interrupted {
		t.Fatal("turn was not interrupted")
	}
	next := session.NewTurn("next", func() {})
	if err := ref.session.BeginTurn(next); !errors.Is(err, session.ErrSessionBusy) {
		if err == nil {
			next.Interrupt(session.TurnSnapshot{Error: "test cleanup"})
			next.FinishController()
			ref.session.FinishTurn(next)
		}
		t.Fatalf("expected ErrSessionBusy before controller exit, got %v", err)
	}
	turn.FinishController()
	ref.session.FinishTurn(turn)
}

func TestManagerCancelsLifecycleContextAfterClientCloseStarts(t *testing.T) {
	cl := newFakeClient()
	var manager *Manager
	var lifecycleWasActive atomic.Bool
	var closeObserved sync.Once
	cl.closeFn = func(context.Context) {
		closeObserved.Do(func() {
			lifecycleWasActive.Store(manager.ctx.Err() == nil)
		})
	}
	manager = NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !lifecycleWasActive.Load() {
		t.Fatal("manager lifecycle context was cancelled before Client.Close")
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

func TestSessionLimitErrorIncludesCounts(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(1), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); err != nil {
		t.Fatal(err)
	}
	_, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("expected ErrSessionLimit, got %v", err)
	}
	if got := err.Error(); got != "session limit reached: active=1 limit=1" {
		t.Fatalf("error = %q", got)
	}
}

func TestCreateRejectsEmptyAgentSessionID(t *testing.T) {
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return &emptyIDClient{fakeClient: newFakeClient()}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); !errors.Is(err, session.ErrInvalidSessionID) {
		t.Fatalf("expected invalid session ID, got %v", err)
	}
	if len(manager.Sessions()) != 0 {
		t.Fatalf("invalid session was registered: %#v", manager.Sessions())
	}
}

func TestDuplicateCreatedSessionIDInvalidatesInstanceWithoutClosingExistingSession(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	first, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	cl := factory.clients[driver.AgentTypeCodex]
	cl.mu.Lock()
	cl.next = 0
	cl.mu.Unlock()

	if _, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp"); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("expected ErrSessionExists, got %v", err)
	}
	cl.mu.Lock()
	closeCalls := append([]string(nil), cl.closeSessionCalls...)
	cl.mu.Unlock()
	if len(closeCalls) != 0 {
		t.Fatalf("duplicate ID closed existing remote session %q: %v", first.ID, closeCalls)
	}
	if got := manager.Sessions(); len(got) != 0 {
		t.Fatalf("invalidated instance sessions still registered: %#v", got)
	}
}

func TestForkRegistrationFailureClosesNewRemoteSession(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	parent, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	cl := factory.clients[driver.AgentTypeCodex]
	started := make(chan struct{})
	release := make(chan struct{})
	cl.forkFn = func(context.Context, string, string) (*acp.UnstableForkSessionResponse, error) {
		close(started)
		<-release
		return &acp.UnstableForkSessionResponse{SessionId: "fork-new"}, nil
	}

	result := make(chan error, 1)
	go func() {
		_, err := manager.ForkSession(context.Background(), parent.ID.String())
		result <- err
	}()
	<-started
	manager.mu.Lock()
	manager.closing = true
	manager.mu.Unlock()
	close(release)
	if err := <-result; !errors.Is(err, ErrManagerClosing) {
		t.Fatalf("expected ErrManagerClosing, got %v", err)
	}
	manager.mu.Lock()
	manager.closing = false
	manager.mu.Unlock()

	cl.mu.Lock()
	closeCalls := append([]string(nil), cl.closeSessionCalls...)
	cl.mu.Unlock()
	if !containsString(closeCalls, "fork-new") {
		t.Fatalf("forked remote session was not closed: %v", closeCalls)
	}
}

func TestDeleteActiveSessionRequiresCloseFirst(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.DeleteSession(context.Background(), created.ID); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("expected ErrSessionActive, got %v", err)
	}
	cl := factory.clients[driver.AgentTypeCodex]
	cl.mu.Lock()
	deleteCalls := append([]string(nil), cl.deleteCalls...)
	cl.mu.Unlock()
	if len(deleteCalls) != 0 {
		t.Fatalf("active session reached remote delete: %v", deleteCalls)
	}
	if got := len(manager.Sessions()); got != 1 {
		t.Fatalf("active session was removed, sessions=%d", got)
	}
}

func TestDeleteSerializesWithLoadOfSameSession(t *testing.T) {
	cl := newFakeClient()
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	cl.deleteFn = func(context.Context, string) (*acp.UnstableDeleteSessionResponse, error) {
		close(deleteStarted)
		<-releaseDelete
		return &acp.UnstableDeleteSessionResponse{}, nil
	}
	loadStarted := make(chan struct{})
	cl.loadFn = func(context.Context, string, string) (*acp.LoadSessionResponse, error) {
		close(loadStarted)
		return nil, errors.New("session was deleted")
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	id := session.ID{AgentType: driver.AgentTypeCodex, AgentSessionID: "history-1"}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- manager.DeleteSession(context.Background(), id)
	}()
	<-deleteStarted
	loadDone := make(chan error, 1)
	go func() {
		_, err := manager.LoadSession(context.Background(), id, "/tmp")
		loadDone <- err
	}()
	overlapped := false
	select {
	case <-loadStarted:
		overlapped = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseDelete)
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err := <-loadDone; err == nil {
		t.Fatal("load unexpectedly succeeded after delete")
	}
	if overlapped {
		t.Fatal("load reached agent before delete of the same session completed")
	}
}

func TestConcurrentLoadAndResumeUseOneRemoteActivation(t *testing.T) {
	cl := newFakeClient()
	loadStarted := make(chan struct{})
	releaseLoad := make(chan struct{})
	cl.loadFn = func(context.Context, string, string) (*acp.LoadSessionResponse, error) {
		close(loadStarted)
		<-releaseLoad
		return &acp.LoadSessionResponse{}, nil
	}
	resumeStarted := make(chan struct{})
	cl.resumeFn = func(context.Context, string, string) (*acp.ResumeSessionResponse, error) {
		close(resumeStarted)
		return &acp.ResumeSessionResponse{}, nil
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	id := session.ID{AgentType: driver.AgentTypeCodex, AgentSessionID: "history-1"}

	loadDone := make(chan error, 1)
	go func() {
		_, err := manager.LoadSession(context.Background(), id, "/tmp")
		loadDone <- err
	}()
	<-loadStarted
	resumeDone := make(chan error, 1)
	go func() {
		_, err := manager.ResumeSession(context.Background(), id, "/tmp")
		resumeDone <- err
	}()
	overlapped := false
	select {
	case <-resumeStarted:
		overlapped = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseLoad)
	if err := <-loadDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resumeDone; !errors.Is(err, ErrSessionExists) {
		t.Fatalf("expected ErrSessionExists, got %v", err)
	}
	if overlapped {
		t.Fatal("resume reached agent while load of the same session was in flight")
	}
}

func TestCloseSerializesWithForkOfSameSession(t *testing.T) {
	cl := newFakeClient()
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	parent, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	forkStarted := make(chan struct{})
	releaseFork := make(chan struct{})
	cl.forkFn = func(context.Context, string, string) (*acp.UnstableForkSessionResponse, error) {
		close(forkStarted)
		<-releaseFork
		return &acp.UnstableForkSessionResponse{SessionId: "fork-1"}, nil
	}
	parentCloseStarted := make(chan struct{})
	cl.closeSessionFn = func(_ context.Context, sessionID string) (*acp.CloseSessionResponse, error) {
		if sessionID == parent.ID.AgentSessionID {
			close(parentCloseStarted)
		}
		return &acp.CloseSessionResponse{}, nil
	}

	forkDone := make(chan error, 1)
	go func() {
		_, err := manager.ForkSession(context.Background(), parent.ID.String())
		forkDone <- err
	}()
	<-forkStarted
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseSession(context.Background(), parent.ID.String())
	}()
	overlapped := false
	select {
	case <-parentCloseStarted:
		overlapped = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFork)
	if err := <-forkDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if overlapped {
		t.Fatal("close reached agent while fork of the same session was in flight")
	}
}

func TestLoadCompensatesRemoteSessionWhenInstanceExitsBeforeRegister(t *testing.T) {
	cl := newFakeClient()
	loadStarted := make(chan struct{})
	releaseLoad := make(chan struct{})
	cl.loadFn = func(context.Context, string, string) (*acp.LoadSessionResponse, error) {
		close(loadStarted)
		<-releaseLoad
		return &acp.LoadSessionResponse{}, nil
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	id := session.ID{AgentType: driver.AgentTypeCodex, AgentSessionID: "history-1"}

	loadDone := make(chan error, 1)
	go func() {
		_, err := manager.LoadSession(context.Background(), id, "/tmp")
		loadDone <- err
	}()
	<-loadStarted
	cl.closeOne.Do(func() { close(cl.done) })
	waitForInstanceAbsent(t, manager, driver.AgentTypeCodex)
	close(releaseLoad)
	if err := <-loadDone; !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("expected ErrInstanceChanged, got %v", err)
	}
	if sessions := manager.Sessions(); len(sessions) != 0 {
		t.Fatalf("stale load response registered a session: %#v", sessions)
	}
	cl.mu.Lock()
	closeCalls := append([]string(nil), cl.closeSessionCalls...)
	cl.mu.Unlock()
	if !containsString(closeCalls, id.AgentSessionID) {
		t.Fatalf("loaded remote session was not closed after register failure: %v", closeCalls)
	}
}

func TestDeleteKeepsSuccessWhenInstanceExitsAfterRemoteDelete(t *testing.T) {
	cl := newFakeClient()
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	cl.deleteFn = func(context.Context, string) (*acp.UnstableDeleteSessionResponse, error) {
		close(deleteStarted)
		<-releaseDelete
		return &acp.UnstableDeleteSessionResponse{}, nil
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	id := session.ID{AgentType: driver.AgentTypeCodex, AgentSessionID: "history-1"}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- manager.DeleteSession(context.Background(), id)
	}()
	<-deleteStarted
	cl.closeOne.Do(func() { close(cl.done) })
	waitForInstanceAbsent(t, manager, driver.AgentTypeCodex)
	close(releaseDelete)
	if err := <-deleteDone; err != nil {
		t.Fatalf("expected successful delete after remote success, got %v", err)
	}
	cl.mu.Lock()
	deleteCalls := append([]string(nil), cl.deleteCalls...)
	cl.mu.Unlock()
	if !containsString(deleteCalls, id.AgentSessionID) {
		t.Fatalf("remote delete was not invoked: %v", deleteCalls)
	}
}

func TestHistoryKeepsResultWhenInstanceExitsAfterList(t *testing.T) {
	cl := newFakeClient()
	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	cl.listFn = func(context.Context) (*acp.ListSessionsResponse, error) {
		close(listStarted)
		<-releaseList
		return &acp.ListSessionsResponse{
			Sessions: []acp.SessionInfo{{SessionId: "history-1"}},
		}, nil
	}
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	type result struct {
		sessions []acp.SessionInfo
		err      error
	}
	historyDone := make(chan result, 1)
	go func() {
		sessions, err := manager.History(context.Background(), driver.AgentTypeCodex)
		historyDone <- result{sessions: sessions, err: err}
	}()
	<-listStarted
	cl.closeOne.Do(func() { close(cl.done) })
	waitForInstanceAbsent(t, manager, driver.AgentTypeCodex)
	close(releaseList)
	got := <-historyDone
	if got.err != nil {
		t.Fatalf("expected successful history after remote success, got %v", got.err)
	}
	if len(got.sessions) != 1 || string(got.sessions[0].SessionId) != "history-1" {
		t.Fatalf("unexpected history result: %#v", got.sessions)
	}
}

func TestSetModeRejectsResponseAfterSessionWasDetached(t *testing.T) {
	cl := newFakeClient()
	manager := NewManager(testConfig(10), func(context.Context, driver.AgentType) (ACPClient, error) {
		return cl, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	setModeStarted := make(chan struct{})
	releaseSetMode := make(chan struct{})
	cl.setModeFn = func(context.Context, string, string) (*acp.SetSessionModeResponse, error) {
		close(setModeStarted)
		<-releaseSetMode
		return &acp.SetSessionModeResponse{}, nil
	}

	setModeDone := make(chan error, 1)
	go func() {
		_, err := manager.SetMode(context.Background(), created.ID.String(), "acceptEdits")
		setModeDone <- err
	}()
	<-setModeStarted
	cl.closeOne.Do(func() { close(cl.done) })
	waitForSessionCount(t, manager, 0)
	close(releaseSetMode)
	if err := <-setModeDone; !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestSetModeLogsLifecycleOutcome(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if _, err := manager.SetMode(context.Background(), created.ID.String(), "acceptEdits"); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	for _, expected := range []string{
		`msg="ACP Session 模式已更新"`,
		"agent_type=codex",
		"session_id=codex:session-1",
		"mode=acceptEdits",
		"elapsed=",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("log missing %q: %s", expected, output)
		}
	}
}

func waitForSessionCount(t *testing.T, manager *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(manager.Sessions()) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(manager.Sessions()); got != want {
		t.Fatalf("sessions=%d, want %d", got, want)
	}
}

func waitForInstanceAbsent(t *testing.T, manager *Manager, agentType driver.AgentType) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		_, exists := manager.instances[agentType]
		manager.mu.Unlock()
		if !exists {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s instance was not detached", agentType)
		}
		time.Sleep(time.Millisecond)
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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

func TestCancelledChatBeforeTurnRegistrationLeavesSessionIdle(t *testing.T) {
	factory := newFakeFactory()
	manager := NewManager(testConfig(10), factory.New)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	created, err := manager.CreateSession(context.Background(), driver.AgentTypeCodex, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Chat(ctx, created.ID.String(), "ignored", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	progress, err := manager.Progress(created.ID.String(), "")
	if err != nil || progress.Status != StatusIdle {
		t.Fatalf("progress=%#v err=%v", progress, err)
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
