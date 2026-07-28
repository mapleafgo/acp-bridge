package session

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mapleafgo/acp-bridge/internal/config"
)

// SessionID is a unique identifier for an ACP session (bridge-side).
type SessionID string

// SessionStatus represents the lifecycle state of a session.
type SessionStatus string

const (
	StatusActive SessionStatus = "active"
	StatusPaused SessionStatus = "paused"
	StatusClosed SessionStatus = "closed"
)

// SessionState tracks the prompt-turn state machine.
// Transitions: idle → prompting → permission_pending → prompting → idle.
type SessionState string

const (
	StateIdle              SessionState = "idle"
	StatePrompting         SessionState = "prompting"
	StatePermissionPending SessionState = "permission_pending"
)

// CanChat returns true only when the session is idle and ready for a new prompt.
func (s *Session) CanChat() bool { return s.State == StateIdle }

// CanRespond returns true only when the session is waiting for a permission decision.
func (s *Session) CanRespond() bool { return s.State == StatePermissionPending }

// Session holds the runtime state for a single ACP session.
type Session struct {
	ID           SessionID
	AgentType    string
	CWD          string
	ACPSessionID string
	CreatedAt    time.Time
	LastUsed     time.Time
	Status       SessionStatus
	State        SessionState
	Title        string
	CurrentMode  string
	Cancel       context.CancelFunc
	Metadata     map[string]string

	// Prompt-turn tracking
	TurnCount int

	// Session-level metadata persisted from agent notifications
	ConfigOpts []ConfigOptionInfo
	AvailCmds  []AvailableCommandInfo
}

// ConfigOptionInfo is a session-level snapshot of an ACP config option.
type ConfigOptionInfo struct {
	ID    string
	Name  string
	Type  string // select, boolean
	Value string
}

// AvailableCommandInfo is a session-level snapshot of an ACP slash command.
type AvailableCommandInfo struct {
	Name        string
	Description string
	InputHint   string
}

// SessionSummary is a lightweight, serialisable view of a session.
type SessionSummary struct {
	ID          SessionID              `json:"session_id"`
	AgentType   string                 `json:"agent_type"`
	State       SessionState           `json:"state"`
	Status      SessionStatus          `json:"status"`
	TurnCount   int                    `json:"turn_count"`
	IdleSeconds int                    `json:"idle_seconds"`
	CWD         string                 `json:"cwd"`
	Title       string                 `json:"title,omitempty"`
	CurrentMode string                 `json:"current_mode,omitempty"`
	ConfigOpts  []ConfigOptionInfo     `json:"config_options,omitempty"`
	AvailCmds   []AvailableCommandInfo `json:"available_commands,omitempty"`
}

// Sentinel errors returned by Pool operations.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionExists   = errors.New("session already exists")
	ErrSessionBusy     = errors.New("session is busy (prompt in progress)")
)

// Pool is a concurrency-safe registry of active ACP sessions with LRU
// eviction. When the pool is full, the least-recently-used session is
// evicted to make room for a new one.
type Pool struct {
	mu              sync.Mutex
	sessions        map[SessionID]*Session
	lru             *list.List
	elemMap         map[SessionID]*list.Element
	config          *config.Config
	stopCh          chan struct{}
	done            chan struct{}
	cleanupInterval time.Duration
}

// PoolOption configures a Pool at construction time.
type PoolOption func(*Pool)

// WithCleanupInterval overrides the default idle-cleanup tick interval (60s).
func WithCleanupInterval(d time.Duration) PoolOption {
	return func(p *Pool) {
		if d > 0 {
			p.cleanupInterval = d
		}
	}
}

// NewPool returns an initialised Pool and starts its background cleanup
// goroutine. Call Shutdown to stop the goroutine and release resources.
func NewPool(cfg *config.Config, opts ...PoolOption) *Pool {
	p := &Pool{
		sessions:        make(map[SessionID]*Session),
		lru:             list.New(),
		elemMap:         make(map[SessionID]*list.Element),
		config:          cfg,
		stopCh:          make(chan struct{}),
		done:            make(chan struct{}),
		cleanupInterval: 60 * time.Second,
	}
	for _, o := range opts {
		o(p)
	}
	go p.cleanupLoop()
	return p
}

// Add inserts a session into the pool. If the pool is full, the
// least-recently-used session is evicted first.
func (p *Pool) Add(s *Session) error {
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.LastUsed.IsZero() {
		s.LastUsed = now
	}
	if s.Status == "" {
		s.Status = StatusActive
	}
	if s.State == "" {
		s.State = StateIdle
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.sessions[s.ID]; exists {
		return fmt.Errorf("%w: %s", ErrSessionExists, s.ID)
	}

	// LRU eviction when at capacity.
	if len(p.sessions) >= p.config.MaxSessions {
		p.evictOldestLocked()
	}

	p.sessions[s.ID] = s
	p.elemMap[s.ID] = p.lru.PushBack(s.ID)
	return nil
}

// Get retrieves a session by ID and marks it as recently used.
// It returns ErrSessionNotFound when the ID is not in the pool.
func (p *Pool) Get(id SessionID) (*Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s, ok := p.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	s.LastUsed = time.Now()
	// Move to back of LRU.
	if elem, ok := p.elemMap[id]; ok {
		p.lru.MoveToBack(elem)
	}
	return s, nil
}

// Remove deletes a session from the pool. If the session has a non-nil
// Cancel function it is called. Removing a non-existent session is a no-op.
func (p *Pool) Remove(id SessionID) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		delete(p.sessions, id)
		if elem, ok := p.elemMap[id]; ok {
			p.lru.Remove(elem)
			delete(p.elemMap, id)
		}
	}
	p.mu.Unlock()

	if ok && s.Cancel != nil {
		s.Cancel()
	}
}

// List returns a summary of every active session in the pool.
func (p *Pool) List() []SessionSummary {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	summaries := make([]SessionSummary, 0, len(p.sessions))
	for _, s := range p.sessions {
		if s.Status == StatusClosed {
			continue
		}
		summaries = append(summaries, SessionSummary{
			ID:          s.ID,
			AgentType:   s.AgentType,
			State:       s.State,
			Status:      s.Status,
			TurnCount:   s.TurnCount,
			IdleSeconds: int(now.Sub(s.LastUsed).Seconds()),
			CWD:         s.CWD,
			Title:       s.Title,
			CurrentMode: s.CurrentMode,
			ConfigOpts:  s.ConfigOpts,
			AvailCmds:   s.AvailCmds,
		})
	}
	return summaries
}

// Shutdown stops the background cleanup goroutine and removes every session.
func (p *Pool) Shutdown() {
	close(p.stopCh)
	<-p.done

	p.mu.Lock()
	defer p.mu.Unlock()

	for id, s := range p.sessions {
		if s.Cancel != nil {
			s.Cancel()
		}
		delete(p.sessions, id)
	}
	p.lru.Init()
	p.elemMap = make(map[SessionID]*list.Element)
}

// SetState updates the prompt-turn state of a session. This is called by
// the MCP layer as the prompt lifecycle progresses.
func (p *Pool) SetState(id SessionID, state SessionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.State = state
	}
}

// SetTitle updates the session title reported by the agent.
func (p *Pool) SetTitle(id SessionID, title string) {
	if title == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.Title = title
	}
}

// SetCurrentMode updates the permission mode reported by the agent.
func (p *Pool) SetCurrentMode(id SessionID, mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.CurrentMode = mode
	}
}

// SetConfigOptions updates the config options reported by the agent.
func (p *Pool) SetConfigOptions(id SessionID, opts []ConfigOptionInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.ConfigOpts = opts
	}
}

// SetAvailableCommands updates the available commands reported by the agent.
func (p *Pool) SetAvailableCommands(id SessionID, cmds []AvailableCommandInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.AvailCmds = cmds
	}
}

// Touch increments the turn counter and refreshes LastUsed, called after
// a prompt turn completes.
func (p *Pool) Touch(id SessionID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.TurnCount++
		s.LastUsed = time.Now()
		if elem, ok := p.elemMap[id]; ok {
			p.lru.MoveToBack(elem)
		}
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// evictOldestLocked removes the front of the LRU list (least recently used).
// Caller must hold p.mu.
func (p *Pool) evictOldestLocked() {
	front := p.lru.Front()
	if front == nil {
		return
	}
	id := front.Value.(SessionID)
	s, ok := p.sessions[id]
	if !ok {
		return
	}
	delete(p.sessions, id)
	p.lru.Remove(front)
	delete(p.elemMap, id)
	if s.Cancel != nil {
		s.Cancel()
	}
}

func (p *Pool) cleanupLoop() {
	ticker := time.NewTicker(p.cleanupInterval)
	defer ticker.Stop()
	defer close(p.done)

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.evictIdle()
		}
	}
}

func (p *Pool) evictIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	ttl := p.config.SessionTTL
	for id, s := range p.sessions {
		if now.Sub(s.LastUsed) > ttl {
			if s.Cancel != nil {
				s.Cancel()
			}
			delete(p.sessions, id)
			if elem, ok := p.elemMap[id]; ok {
				p.lru.Remove(elem)
				delete(p.elemMap, id)
			}
		}
	}
}
