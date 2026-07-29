package instance

import (
	"context"
	"sync"

	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

// AgentInstance 保存单一 agent 进程、generation 和其拥有的全部活跃 Session。
// 实例只由 Manager 创建和销毁，内部 Session 索引可并发访问。
type AgentInstance struct {
	mu sync.Mutex

	agentType  driver.AgentType
	generation uint64
	client     ACPClient
	ctx        context.Context
	sessions   map[string]*session.Session
}

func newAgentInstance(ctx context.Context, agentType driver.AgentType, generation uint64, client ACPClient) *AgentInstance {
	return &AgentInstance{
		agentType:  agentType,
		generation: generation,
		client:     client,
		ctx:        ctx,
		sessions:   make(map[string]*session.Session),
	}
}

func (i *AgentInstance) addSession(sessionID string, sess *session.Session) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, exists := i.sessions[sessionID]; exists {
		return ErrSessionExists
	}
	i.sessions[sessionID] = sess
	return nil
}

func (i *AgentInstance) removeSession(sessionID string) *session.Session {
	i.mu.Lock()
	defer i.mu.Unlock()
	sess := i.sessions[sessionID]
	delete(i.sessions, sessionID)
	return sess
}

func (i *AgentInstance) allSessions() []*session.Session {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]*session.Session, 0, len(i.sessions))
	for _, sess := range i.sessions {
		out = append(out, sess)
	}
	return out
}
