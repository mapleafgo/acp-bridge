package session

import (
	"sync"
	"testing"
	"time"

	"github.com/mapleafgo/acp-bridge/internal/config"
)

func TestAddGetRemove(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10, SessionTTL: 10 * time.Minute}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	s := &Session{
		ID:        "test-1",
		AgentType: "codex",
		CWD:       "/home/test",
		Status:    StatusActive,
	}

	err := pool.Add(s)
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	got, err := pool.Get("test-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ID != "test-1" {
		t.Fatalf("expected ID test-1, got %s", got.ID)
	}
	if got.AgentType != "codex" {
		t.Fatalf("expected AgentType codex, got %s", got.AgentType)
	}
	if got.CWD != "/home/test" {
		t.Fatalf("expected CWD /home/test, got %s", got.CWD)
	}

	pool.Remove("test-1")
	_, err = pool.Get("test-1")
	if err == nil {
		t.Fatal("expected error after Remove")
	}
}

func TestAddGetRemove_NonExistentRemoveNoPanic(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	// Removing a non-existent session must not panic
	pool.Remove("does-not-exist")
}

func TestDuplicateSessionID(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	s1 := &Session{ID: "dup", AgentType: "codex"}
	s2 := &Session{ID: "dup", AgentType: "claude"}

	if err := pool.Add(s1); err != nil {
		t.Fatalf("first Add failed: %v", err)
	}
	if err := pool.Add(s2); err == nil {
		t.Fatal("expected error for duplicate session ID")
	}
}

func TestSessionNotFound(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	_, err := pool.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestMaxSessions(t *testing.T) {
	cfg := &config.Config{MaxSessions: 2}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	for i := range 2 {
		id := SessionID(rune('a' + i))
		err := pool.Add(&Session{ID: id, AgentType: "codex"})
		if err != nil {
			t.Fatalf("Add %d failed: %v", i, err)
		}
	}

	// Adding a third session triggers LRU eviction of 'a'
	err := pool.Add(&Session{ID: "too-many", AgentType: "codex"})
	if err != nil {
		t.Fatalf("Add should succeed with LRU eviction, got: %v", err)
	}

	// The oldest session ('a') should have been evicted.
	if _, err := pool.Get("a"); err == nil {
		t.Error("expected session 'a' to be evicted by LRU")
	}
	// 'b' and 'too-many' should still exist.
	if _, err := pool.Get("b"); err != nil {
		t.Error("session 'b' should survive")
	}
	if _, err := pool.Get("too-many"); err != nil {
		t.Error("session 'too-many' should exist")
	}
}

func TestMaxSessions_ExactLimit(t *testing.T) {
	cfg := &config.Config{MaxSessions: 3}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	for i := range 3 {
		err := pool.Add(&Session{ID: SessionID(string(rune('x' + i))), AgentType: "codex"})
		if err != nil {
			t.Fatalf("Add %d failed: %v", i, err)
		}
	}

	list := pool.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(list))
	}
}

func TestIdleCleanup(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10, SessionTTL: 1 * time.Hour} // long TTL for fresh sessions
	pool := NewPool(cfg, WithCleanupInterval(30*time.Millisecond))
	defer pool.Shutdown()

	pool.Add(&Session{ID: "fresh", AgentType: "codex"})
	pool.Add(&Session{
		ID:        "stale",
		AgentType: "codex",
		LastUsed:  time.Now().Add(-1 * time.Hour),
		// Override the stored LastUsed by re-setting through a direct access
	})

	// Manually force the stale session's LastUsed to be old in the pool
	// (the Add call above set LastUsed=now because the field was not zero,
	// so we need to directly poke it)
	pool.mu.Lock()
	if s, ok := pool.sessions["stale"]; ok {
		s.LastUsed = time.Now().Add(-2 * time.Hour)
	}
	pool.mu.Unlock()

	// Wait for at least one cleanup tick
	time.Sleep(100 * time.Millisecond)

	_, errFresh := pool.Get("fresh")
	_, errStale := pool.Get("stale")

	if errFresh != nil {
		t.Errorf("fresh session should survive cleanup, got: %v", errFresh)
	}
	if errStale == nil {
		t.Error("stale session should have been cleaned up")
	}
}

func TestList(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10, SessionTTL: 10 * time.Minute}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	sessions := []*Session{
		{ID: "s1", AgentType: "codex", Status: StatusActive},
		{ID: "s2", AgentType: "claude", Status: StatusActive},
		{ID: "s3", AgentType: "gemini", Status: StatusClosed},
	}

	for _, s := range sessions {
		if err := pool.Add(s); err != nil {
			t.Fatalf("Add %s failed: %v", s.ID, err)
		}
	}

	list := pool.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 active sessions (s3 is closed), got %d: %+v", len(list), list)
	}

	// Verify summaries match
	for _, summary := range list {
		if summary.Status == StatusClosed {
			t.Errorf("List should not include closed sessions, got: %+v", summary)
		}
		if summary.ID == "s1" || summary.ID == "s2" {
			if summary.IdleSeconds < 0 {
				t.Errorf("IdleSeconds should not be negative, got %d", summary.IdleSeconds)
			}
		}
	}
}

func TestConcurrencySafety(t *testing.T) {
	cfg := &config.Config{MaxSessions: 100, SessionTTL: 10 * time.Minute}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	var wg sync.WaitGroup
	n := 50

	// Concurrent Add operations
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := SessionID(string(rune('a' + i)))
			err := pool.Add(&Session{ID: id, AgentType: "codex"})
			if err != nil && err != ErrSessionExists {
				t.Errorf("unexpected Add error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent Get operations
	for i := range n / 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := SessionID(string(rune('a' + i)))
			_, err := pool.Get(id)
			if err != nil && err != ErrSessionNotFound {
				t.Errorf("unexpected Get error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent Remove operations
	for i := range n / 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := SessionID(string(rune('a' + i)))
			pool.Remove(id)
		}(i)
	}
	wg.Wait()

	// Concurrent List operation
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = pool.List()
	}()
	wg.Wait()
}

func TestShutdownCancelsSessions(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10, SessionTTL: 10 * time.Minute}
	pool := NewPool(cfg)

	cancelCalled := false
	cancel := func() {
		cancelCalled = true
	}

	pool.Add(&Session{
		ID:        "will-cancel",
		AgentType: "codex",
		Cancel:    cancel,
	})

	pool.Shutdown()

	if !cancelCalled {
		t.Error("expected Cancel to be called during Shutdown")
	}

	// After shutdown, List should be empty
	list := pool.List()
	if len(list) != 0 {
		t.Errorf("expected empty pool after Shutdown, got %d sessions", len(list))
	}
}

func TestGetUpdatesLastUsed(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10, SessionTTL: 1 * time.Hour}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	s := &Session{
		ID:        "lu-test",
		AgentType: "codex",
		LastUsed:  time.Now().Add(-1 * time.Hour),
	}
	pool.Add(s)

	oldLastUsed := s.LastUsed

	// Get should update LastUsed
	_, err := pool.Get("lu-test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if !s.LastUsed.After(oldLastUsed) {
		t.Error("Get should update LastUsed timestamp")
	}
}

func TestRemoveNonExistent(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	// Must not panic
	pool.Remove("i-dont-exist")
}

// ---------------------------------------------------------------------------
// 状态机测试
// ---------------------------------------------------------------------------

func TestSessionCanChat(t *testing.T) {
	s := &Session{State: StateIdle}
	if !s.CanChat() {
		t.Error("idle session should be able to chat")
	}
	s.State = StatePrompting
	if s.CanChat() {
		t.Error("prompting session should not be able to chat")
	}
	s.State = StatePermissionPending
	if s.CanChat() {
		t.Error("permission_pending session should not be able to chat")
	}
}

func TestSessionCanRespond(t *testing.T) {
	s := &Session{State: StateIdle}
	if s.CanRespond() {
		t.Error("idle session should not be able to respond")
	}
	s.State = StatePermissionPending
	if !s.CanRespond() {
		t.Error("permission_pending session should be able to respond")
	}
}

func TestPoolSetState(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	pool.Add(&Session{ID: "s1", AgentType: "codex"})

	pool.SetState("s1", StatePrompting)
	s, _ := pool.Get("s1")
	if s.State != StatePrompting {
		t.Fatalf("expected state=prompting, got %s", s.State)
	}

	pool.SetState("s1", StatePermissionPending)
	s, _ = pool.Get("s1")
	if s.State != StatePermissionPending {
		t.Fatalf("expected state=permission_pending, got %s", s.State)
	}

	pool.SetState("s1", StateIdle)
	s, _ = pool.Get("s1")
	if s.State != StateIdle {
		t.Fatalf("expected state=idle, got %s", s.State)
	}
}

func TestPoolSetStateNonExistent(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	// Must not panic
	pool.SetState("ghost", StatePrompting)
}

func TestPoolSetTitleIgnoresEmpty(t *testing.T) {
	cfg := config.Load()
	pool := NewPool(cfg)
	defer pool.Shutdown()

	id := SessionID("title-session")
	if err := pool.Add(&Session{ID: id}); err != nil {
		t.Fatalf("add session: %v", err)
	}
	pool.SetTitle(id, "Agent title")
	pool.SetTitle(id, "")

	sess, err := pool.Get(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Title != "Agent title" {
		t.Fatalf("empty title must not overwrite existing title, got %q", sess.Title)
	}
}

func TestPoolTouch(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	s := &Session{ID: "touch-1", AgentType: "codex"}
	pool.Add(s)

	oldTurn := s.TurnCount
	oldLastUsed := s.LastUsed

	time.Sleep(10 * time.Millisecond)
	pool.Touch("touch-1")

	s, _ = pool.Get("touch-1")
	if s.TurnCount != oldTurn+1 {
		t.Fatalf("expected turn_count=%d, got %d", oldTurn+1, s.TurnCount)
	}
	if !s.LastUsed.After(oldLastUsed) {
		t.Error("Touch should update LastUsed")
	}
}

func TestPoolTouchNonExistent(t *testing.T) {
	cfg := &config.Config{MaxSessions: 10}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	// Must not panic
	pool.Touch("ghost")
}

// ---------------------------------------------------------------------------
// LRU 淘汰顺序验证
// ---------------------------------------------------------------------------

func TestLRUEvictionOrder(t *testing.T) {
	cfg := &config.Config{MaxSessions: 3}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	// Add 3 sessions: a, b, c (c is most recent)
	for _, id := range []SessionID{"a", "b", "c"} {
		if err := pool.Add(&Session{ID: id, AgentType: "codex"}); err != nil {
			t.Fatalf("Add %s failed: %v", id, err)
		}
	}

	// Access 'a' to make it recently used (LRU order now: b, c, a)
	pool.Get("a")

	// Adding 'd' should evict 'b' (the new least recently used)
	if err := pool.Add(&Session{ID: "d", AgentType: "codex"}); err != nil {
		t.Fatalf("Add d failed: %v", err)
	}

	if _, err := pool.Get("b"); err == nil {
		t.Error("expected 'b' to be evicted (LRU)")
	}
	// 'a', 'c', 'd' should survive
	for _, id := range []SessionID{"a", "c", "d"} {
		if _, err := pool.Get(id); err != nil {
			t.Errorf("session %s should survive, got: %v", id, err)
		}
	}
}

func TestLRUEvictionOnFullPoolFromStart(t *testing.T) {
	cfg := &config.Config{MaxSessions: 1}
	pool := NewPool(cfg)
	defer pool.Shutdown()

	pool.Add(&Session{ID: "first", AgentType: "codex"})
	// Adding second should evict 'first'
	pool.Add(&Session{ID: "second", AgentType: "codex"})

	if _, err := pool.Get("first"); err == nil {
		t.Error("expected 'first' to be evicted")
	}
	if _, err := pool.Get("second"); err != nil {
		t.Error("expected 'second' to exist")
	}
}
