package session

import (
	"context"
	"errors"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// TurnState 表示一次 prompt 的执行状态。
type TurnState string

const (
	TurnRunning            TurnState = "running"
	TurnPermissionRequired TurnState = "permission_required"
	TurnCompleted          TurnState = "completed"
	TurnInterrupted        TurnState = "interrupted"
	TurnError              TurnState = "error"
)

var (
	ErrTurnNotFound   = errors.New("turn not found")
	ErrTurnMismatch   = errors.New("turn ID does not match current turn")
	ErrPermissionGone = errors.New("permission request is no longer pending")
)

type PermissionOption struct {
	ID   string
	Name string
	Kind string
}

type PermissionView struct {
	RequestID  string
	ToolCallID string
	Title      string
	Kind       string
	Options    []PermissionOption
}

type ToolCall struct {
	ID        string
	Title     string
	Status    string
	Kind      string
	Locations []string
	RawInput  any
	RawOutput any
}

type PlanStep struct {
	Content  string
	Status   string
	Priority string
}

type FileChange struct {
	Path string
	Kind string
}

type Usage struct {
	UsedTokens  int
	TotalTokens int
	Cost        float64
	Currency    string
}

// TurnSnapshot 是一次 Turn 可持久查询的输出内容。
type TurnSnapshot struct {
	StopReason  string
	AgentText   string
	Reasoning   string
	ToolCalls   []ToolCall
	Plan        []PlanStep
	FileChanges []FileChange
	Usage       *Usage
	Updates     []acp.SessionNotification
	Error       string
}

// TurnView 是 Turn 当前状态和值快照。
type TurnView struct {
	ID         string
	State      TurnState
	Permission *PermissionView
	TurnSnapshot
}

// Turn 只允许首次终态提交生效。
type Turn struct {
	mu          sync.Mutex
	id          string
	state       TurnState
	cancel      context.CancelFunc
	permission  *PermissionView
	permissions []PermissionView
	snapshot    TurnSnapshot
	changed     chan struct{}
	controller  chan struct{}
	finishOnce  sync.Once
}

func NewTurn(id string, cancel context.CancelFunc) *Turn {
	if cancel == nil {
		cancel = func() {}
	}
	return &Turn{
		id:         id,
		state:      TurnRunning,
		cancel:     cancel,
		changed:    make(chan struct{}),
		controller: make(chan struct{}),
	}
}

func (t *Turn) ControllerDone() <-chan struct{} {
	return t.controller
}

func (t *Turn) FinishController() {
	t.finishOnce.Do(func() {
		close(t.controller)
	})
}

func (t *Turn) ID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

func (t *Turn) Snapshot() TurnView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Turn) snapshotLocked() TurnView {
	view := TurnView{
		ID:           t.id,
		State:        t.state,
		TurnSnapshot: cloneSnapshot(t.snapshot),
	}
	if t.permission != nil {
		p := clonePermission(*t.permission)
		view.Permission = &p
	}
	return view
}

func (t *Turn) RequirePermission(permission PermissionView) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.terminalLocked() {
		return false
	}
	if t.permission != nil {
		t.permissions = append(t.permissions, clonePermission(permission))
		return true
	}
	p := clonePermission(permission)
	t.permission = &p
	t.state = TurnPermissionRequired
	t.notifyLocked()
	return true
}

func (t *Turn) ResolvePermission(requestID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.permission == nil || t.permission.RequestID != requestID {
		return ErrPermissionGone
	}
	if len(t.permissions) > 0 {
		next := clonePermission(t.permissions[0])
		t.permissions = t.permissions[1:]
		t.permission = &next
		t.state = TurnPermissionRequired
	} else {
		t.permission = nil
		t.state = TurnRunning
	}
	t.notifyLocked()
	return nil
}

func (t *Turn) Complete(snapshot TurnSnapshot) bool {
	return t.finish(TurnCompleted, snapshot)
}

func (t *Turn) Interrupt(snapshot TurnSnapshot) bool {
	return t.finish(TurnInterrupted, snapshot)
}

func (t *Turn) Fail(snapshot TurnSnapshot) bool {
	return t.finish(TurnError, snapshot)
}

func (t *Turn) finish(state TurnState, snapshot TurnSnapshot) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.terminalLocked() {
		return false
	}
	t.state = state
	t.permission = nil
	t.permissions = nil
	t.snapshot = cloneSnapshot(snapshot)
	t.notifyLocked()
	return true
}

func (t *Turn) Cancel() {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	cancel()
}

// Wait 等待下一次状态变化；超时或调用方取消时返回当时的最新快照。
func (t *Turn) Wait(ctx context.Context, timeout time.Duration) TurnView {
	t.mu.Lock()
	if t.state != TurnRunning {
		view := t.snapshotLocked()
		t.mu.Unlock()
		return view
	}
	changed := t.changed
	t.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-changed:
	case <-ctx.Done():
	case <-timer:
	}
	return t.Snapshot()
}

func (t *Turn) terminalLocked() bool {
	return t.state == TurnCompleted || t.state == TurnInterrupted || t.state == TurnError
}

func (t *Turn) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func clonePermission(permission PermissionView) PermissionView {
	permission.Options = append([]PermissionOption(nil), permission.Options...)
	return permission
}

func cloneSnapshot(snapshot TurnSnapshot) TurnSnapshot {
	snapshot.ToolCalls = append([]ToolCall(nil), snapshot.ToolCalls...)
	for i := range snapshot.ToolCalls {
		snapshot.ToolCalls[i].Locations = append([]string(nil), snapshot.ToolCalls[i].Locations...)
	}
	snapshot.Plan = append([]PlanStep(nil), snapshot.Plan...)
	snapshot.FileChanges = append([]FileChange(nil), snapshot.FileChanges...)
	snapshot.Updates = append([]acp.SessionNotification(nil), snapshot.Updates...)
	if snapshot.Usage != nil {
		usage := *snapshot.Usage
		snapshot.Usage = &usage
	}
	return snapshot
}
