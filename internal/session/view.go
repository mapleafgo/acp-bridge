package session

import "time"

// State 表示 Session 当前是否可接受新 prompt。
type State string

const (
	StateIdle              State = "idle"
	StatePrompting         State = "prompting"
	StatePermissionPending State = "permission_pending"
	StateClosing           State = "closing"
)

// ConfigOptionInfo 是 agent 配置项的值快照。
type ConfigOptionInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// AvailableCommandInfo 是 agent slash command 的值快照。
type AvailableCommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputHint   string `json:"input_hint"`
}

// SessionView 是并发安全、不可反向修改 Session 的值快照。
type SessionView struct {
	ID          ID
	CWD         string
	CreatedAt   time.Time
	LastUsed    time.Time
	State       State
	Title       string
	CurrentMode string
	TurnCount   int
	ConfigOpts  []ConfigOptionInfo
	AvailCmds   []AvailableCommandInfo
	Turn        *TurnView
}
