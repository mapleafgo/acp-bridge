package session

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrSessionNotFound 表示 qualified ID 不在活跃 Session 索引中。
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionExists 表示相同 qualified ID 已经处于活跃状态。
	ErrSessionExists = errors.New("session already active")
	// ErrSessionBusy 表示 Session 已有尚未排空的 Turn controller。
	ErrSessionBusy = errors.New("session busy (prompt in progress)")
	// ErrSessionClosed 表示 Session 已进入 closing，不再接受新操作。
	ErrSessionClosed = errors.New("session is closing")
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

// New 创建初始为 idle 的 Session；返回对象的所有公开方法均可并发调用。
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

// ID 返回包含 agent 类型的 qualified ID 值。
func (s *Session) ID() ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// AgentSessionID 返回 agent 原始 Session ID，不包含类型前缀。
func (s *Session) AgentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id.AgentSessionID
}

// CWD 返回 Session 创建或加载时使用的工作目录。
func (s *Session) CWD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// View 返回无法反向修改 Session 内部切片和 Turn 状态的值快照。
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

// BeginTurn 在 Session idle 时安装新 Turn；closing 或已有运行中 Turn 时返回错误。
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

// CurrentTurn 返回当前或最近的 Turn 指针；调用方只能通过 Turn 的并发安全方法操作它。
func (s *Session) CurrentTurn() *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// SetTurnState 仅在 turn 仍是当前 Turn 且 Session 未关闭时更新运行态。
func (s *Session) SetTurnState(turn *Turn, state State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != turn || s.state == StateClosing {
		return false
	}
	s.state = state
	return true
}

// FinishTurn 在匹配 Turn 的 controller 退出后计入完成次数并恢复 idle；
// closing 状态由生命周期 owner 保留，不会被 Turn 清理覆盖。
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

// Touch 更新最后使用时间，用于活跃 Session 列表排序。
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// ReopenAfterCloseFailure 撤销 closing。若 Turn controller 尚未退出，Session
// 保持 prompting，直到 controller 调用 FinishTurn 后才重新接受新 Turn。
func (s *Session) ReopenAfterCloseFailure() {
	s.mu.Lock()
	if s.state == StateClosing {
		s.state = StateIdle
		if s.turn != nil {
			select {
			case <-s.turn.ControllerDone():
			default:
				s.state = StatePrompting
			}
		}
	}
	s.mu.Unlock()
}

// BeginClose 原子拒绝后续 Turn 并返回当前 Turn。已经 closing 时返回
// ErrSessionClosed，保证一次用户关闭只触发一次远端 Close。
func (s *Session) BeginClose() (*Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateClosing {
		return nil, ErrSessionClosed
	}
	s.state = StateClosing
	return s.turn, nil
}

// Close 强制将 Session 标记为 closing，供 Manager 退出和实例崩溃清理使用；
// 该方法幂等，并返回需要由生命周期 owner 取消的当前 Turn。
func (s *Session) Close() *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = StateClosing
	return s.turn
}

// SetTitle 更新非空标题；空标题不会覆盖已知标题。
func (s *Session) SetTitle(title string) {
	if title == "" {
		return
	}
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
}

// SetCurrentMode 更新 agent 当前权限模式的本地快照。
func (s *Session) SetCurrentMode(mode string) {
	s.mu.Lock()
	s.currentMode = mode
	s.mu.Unlock()
}

// SetConfigOptions 用深拷贝替换 agent 配置项快照。
func (s *Session) SetConfigOptions(opts []ConfigOptionInfo) {
	s.mu.Lock()
	s.configOpts = append([]ConfigOptionInfo(nil), opts...)
	s.mu.Unlock()
}

// SetAvailableCommands 用深拷贝替换 agent 可用命令快照。
func (s *Session) SetAvailableCommands(commands []AvailableCommandInfo) {
	s.mu.Lock()
	s.availCmds = append([]AvailableCommandInfo(nil), commands...)
	s.mu.Unlock()
}
