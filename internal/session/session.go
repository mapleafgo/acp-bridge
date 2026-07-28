package session

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionExists   = errors.New("session already exists")
	ErrSessionBusy     = errors.New("session is busy (prompt in progress)")
	ErrSessionClosed   = errors.New("session is closing")
)

// Session 保存单个 agent Session 的 bridge 运行时状态。
type Session struct {
	mu sync.Mutex

	id          ID
	cwd         string
	createdAt   time.Time
	lastUsed    time.Time
	state       State
	title       string
	currentMode string
	turnCount   int
	configOpts  []ConfigOptionInfo
	availCmds   []AvailableCommandInfo
	turn        *Turn
}

func New(id ID, cwd string) *Session {
	now := time.Now()
	return &Session{
		id:        id,
		cwd:       cwd,
		createdAt: now,
		lastUsed:  now,
		state:     StateIdle,
	}
}

func (s *Session) ID() ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *Session) AgentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id.AgentSessionID
}

func (s *Session) CWD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()

	view := SessionView{
		ID:          s.id,
		CWD:         s.cwd,
		CreatedAt:   s.createdAt,
		LastUsed:    s.lastUsed,
		State:       s.state,
		Title:       s.title,
		CurrentMode: s.currentMode,
		TurnCount:   s.turnCount,
		ConfigOpts:  append([]ConfigOptionInfo(nil), s.configOpts...),
		AvailCmds:   append([]AvailableCommandInfo(nil), s.availCmds...),
	}
	if s.turn != nil {
		turn := s.turn.Snapshot()
		view.Turn = &turn
	}
	return view
}

func (s *Session) BeginTurn(turn *Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateClosing {
		return ErrSessionClosed
	}
	if s.state != StateIdle {
		return ErrSessionBusy
	}
	s.turn = turn
	s.state = StatePrompting
	return nil
}

func (s *Session) CurrentTurn() *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

func (s *Session) SetTurnState(turn *Turn, state State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != turn || s.state == StateClosing {
		return false
	}
	s.state = state
	return true
}

func (s *Session) FinishTurn(turn *Turn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != turn || s.state == StateClosing {
		return false
	}
	s.state = StateIdle
	s.turnCount++
	s.lastUsed = time.Now()
	return true
}

func (s *Session) Touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *Session) ReopenAfterCloseFailure() {
	s.mu.Lock()
	if s.state == StateClosing {
		s.state = StateIdle
	}
	s.mu.Unlock()
}

func (s *Session) Close() *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = StateClosing
	return s.turn
}

func (s *Session) SetTitle(title string) {
	if title == "" {
		return
	}
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
}

func (s *Session) SetCurrentMode(mode string) {
	s.mu.Lock()
	s.currentMode = mode
	s.mu.Unlock()
}

func (s *Session) SetConfigOptions(opts []ConfigOptionInfo) {
	s.mu.Lock()
	s.configOpts = append([]ConfigOptionInfo(nil), opts...)
	s.mu.Unlock()
}

func (s *Session) SetAvailableCommands(commands []AvailableCommandInfo) {
	s.mu.Lock()
	s.availCmds = append([]AvailableCommandInfo(nil), commands...)
	s.mu.Unlock()
}
