package mcp

// ---------------------------------------------------------------------------
// Argument types (MCP tool input schemas inferred by the SDK)
// ---------------------------------------------------------------------------

type acpChatArgs struct {
	Prompt    string `json:"prompt" jsonschema:"The prompt text to send to the agent"`
	SessionID string `json:"session_id,omitempty" jsonschema:"Existing session ID to continue (omit for a new session)"`
	CWD       string `json:"cwd,omitempty" jsonschema:"Working directory for the agent"`
	AgentType string `json:"agent_type,omitempty" jsonschema:"Agent type: codex, claude, gemini, or opencode (default: codex)"`
}

type acpRespondArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session ID from the permission_required response"`
	RequestID string `json:"request_id" jsonschema:"The request ID from the permission_required response"`
	Outcome   string `json:"outcome" jsonschema:"Permission decision: allow or deny"`
}

type acpSessionIDArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session ID"`
}

type acpSetModeArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session ID"`
	Mode      string `json:"mode" jsonschema:"Permission mode: read-only, agent, or agent-full-access"`
}

type acpSetConfigArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session ID"`
	ConfigID  string `json:"config_id" jsonschema:"The configuration option ID (e.g. model, reasoning_effort)"`
	Value     string `json:"value" jsonschema:"The new value for the configuration option"`
}

type acpListHistoryArgs struct {
	AgentType string `json:"agent_type,omitempty" jsonschema:"Agent type: codex, claude, gemini, or opencode (default: codex)"`
}

type acpTurnArgs struct {
	SessionID string `json:"session_id" jsonschema:"The session ID to check"`
	TurnID    string `json:"turn_id" jsonschema:"The turn ID returned by acp_chat"`
}

type acpProgressArgs struct {
	SessionID string `json:"session_id" jsonschema:"The qualified session ID"`
	TurnID    string `json:"turn_id,omitempty" jsonschema:"Optional turn ID for exact-match validation"`
}

type acpLoadSessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"Qualified persisted session ID in <agent_type>:<agent_session_id> form"`
	CWD       string `json:"cwd,omitempty" jsonschema:"Working directory for the loaded session"`
}

type acpDeleteSessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"Qualified persisted session ID in <agent_type>:<agent_session_id> form"`
}

// ---------------------------------------------------------------------------
// Structured output types
// ---------------------------------------------------------------------------

type toolResult struct {
	Status    string `json:"status"`
	SessionID string `json:"session_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Error     string `json:"error,omitempty"`
}

type chatResultJSON struct {
	Status      string              `json:"status"`
	SessionID   string              `json:"session_id,omitempty"`
	Title       string              `json:"title,omitempty"`
	State       string              `json:"state,omitempty"`
	TurnID      string              `json:"turn_id,omitempty"`
	StopReason  string              `json:"stop_reason,omitempty"`
	AgentText   string              `json:"agent_text,omitempty"`
	Reasoning   string              `json:"reasoning,omitempty"`
	ToolCalls   []toolCallSummary   `json:"tool_calls,omitempty"`
	Plan        []planStep          `json:"plan,omitempty"`
	FileChanges []fileChangeSummary `json:"file_changes,omitempty"`
	RequestID   string              `json:"request_id,omitempty"`
	Permission  *permissionInfo     `json:"permission,omitempty"`
	TurnCount   int                 `json:"turn_count,omitempty"`
	IsNew       bool                `json:"is_new,omitempty"`
	Usage       *usageInfo          `json:"usage,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type toolCallSummary struct {
	ID        string   `json:"id"`
	Title     string   `json:"title,omitempty"`
	Status    string   `json:"status,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Locations []string `json:"locations,omitempty"`
	RawInput  any      `json:"raw_input,omitempty"`
	RawOutput any      `json:"raw_output,omitempty"`
}

type permissionInfo struct {
	ToolCallID    string             `json:"tool_call_id"`
	Title         string             `json:"title,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Options       []permissionOption `json:"options"`
	IsElicitation bool               `json:"is_elicitation,omitempty"`
}

type permissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type sessionListItem struct {
	SessionID   string `json:"session_id"`
	AgentType   string `json:"agent_type,omitempty"`
	State       string `json:"state,omitempty"`
	Status      string `json:"status,omitempty"`
	TurnStatus  string `json:"turn_status,omitempty"`
	TurnCount   int    `json:"turn_count,omitempty"`
	IdleSeconds int    `json:"idle_seconds,omitempty"`
	Title       string `json:"title,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	CurrentMode string `json:"current_mode,omitempty"`
}

type sessionsListResult struct {
	Status   string            `json:"status"`
	Sessions []sessionListItem `json:"sessions"`
	Error    string            `json:"error,omitempty"`
}

type sessionInfoResult struct {
	Status            string                    `json:"status"`
	SessionID         string                    `json:"session_id,omitempty"`
	AgentType         string                    `json:"agent_type,omitempty"`
	State             string                    `json:"state,omitempty"`
	Title             string                    `json:"title,omitempty"`
	Cwd               string                    `json:"cwd,omitempty"`
	CurrentMode       string                    `json:"current_mode,omitempty"`
	TurnCount         int                       `json:"turn_count,omitempty"`
	ConfigOptions     []configOptionSummary     `json:"config_options,omitempty"`
	AvailableCommands []availableCommandSummary `json:"available_commands,omitempty"`
	Error             string                    `json:"error,omitempty"`
}

// usageInfo carries token and cost data from the agent's usage_update
// session notification. Exposed in acp_chat results so callers can
// track token consumption.
type usageInfo struct {
	UsedTokens  int     `json:"used_tokens,omitempty"`
	TotalTokens int     `json:"total_tokens,omitempty"`
	Cost        float64 `json:"cost,omitempty"`
	Currency    string  `json:"currency,omitempty"`
}

// planStep mirrors a single ACP PlanEntry for structured output.
type planStep struct {
	Content  string `json:"content"`
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
}

// fileChangeSummary captures a file modification reported via ToolCallContent.
type fileChangeSummary struct {
	Path string `json:"path"`
	Kind string `json:"kind,omitempty"` // created, modified
}

// configOptionSummary mirrors a single ACP SessionConfigOption for
// structured output. Covers both select (dropdown) and boolean variants.
type configOptionSummary struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Type  string `json:"type"`            // select, boolean
	Value string `json:"value,omitempty"` // current value as string
}

// availableCommandSummary mirrors an ACP AvailableCommand — a slash
// command the agent supports (e.g. /plan, /research).
type availableCommandSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputHint   string `json:"input_hint,omitempty"`
}
