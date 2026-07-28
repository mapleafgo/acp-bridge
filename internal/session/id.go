package session

import (
	"errors"
	"strings"

	"github.com/mapleafgo/acp-bridge/internal/driver"
)

// ErrInvalidSessionID 表示公开 Session ID 不符合 <agent_type>:<agent_session_id> 格式。
var ErrInvalidSessionID = errors.New("invalid qualified session ID")

// ID 是 bridge 对外暴露的限定 Session ID。
type ID struct {
	AgentType      driver.AgentType
	AgentSessionID string
}

// String 返回可供 MCP 工具传递的稳定 ID。
func (id ID) String() string {
	return string(id.AgentType) + ":" + id.AgentSessionID
}

// ParseID 只切分第一个冒号，保证 agent 原始 Session ID 可继续包含冒号。
func ParseID(raw string) (ID, error) {
	agentType, agentSessionID, ok := strings.Cut(raw, ":")
	if !ok || agentSessionID == "" {
		return ID{}, ErrInvalidSessionID
	}

	t := driver.AgentType(agentType)
	switch t {
	case driver.AgentTypeCodex, driver.AgentTypeClaude, driver.AgentTypeGemini, driver.AgentTypeOpenCode:
	default:
		return ID{}, ErrInvalidSessionID
	}
	return ID{AgentType: t, AgentSessionID: agentSessionID}, nil
}
