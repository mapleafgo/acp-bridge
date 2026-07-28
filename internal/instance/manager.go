package instance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

var (
	ErrSessionLimit    = errors.New("maximum active sessions reached")
	ErrSessionNotFound = session.ErrSessionNotFound
	ErrSessionExists   = session.ErrSessionExists
	ErrManagerClosing  = errors.New("agent instance manager is closing")
)

type instanceSlot struct {
	ready    chan struct{}
	instance *AgentInstance
	err      error
}

type sessionRef struct {
	instance *AgentInstance
	session  *session.Session
}

// Manager 维护每种 agent 最多一个常驻实例，以及全部活跃 Session 的全局索引。
type Manager struct {
	mu sync.Mutex

	ctx          context.Context
	cancel       context.CancelFunc
	config       *config.Config
	factory      ClientFactory
	instances    map[driver.AgentType]*instanceSlot
	sessionIndex map[string]sessionRef
	reservations int
	closing      bool
	nextGen      uint64
}

func NewManager(cfg *config.Config, factory ClientFactory) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	if factory == nil {
		factory = DefaultFactory(cfg)
	}
	return &Manager{
		ctx:          ctx,
		cancel:       cancel,
		config:       cfg,
		factory:      factory,
		instances:    make(map[driver.AgentType]*instanceSlot),
		sessionIndex: make(map[string]sessionRef),
	}
}

func (m *Manager) CreateSession(ctx context.Context, agentType driver.AgentType, cwd string) (session.SessionView, error) {
	if err := m.reserveSession(); err != nil {
		return session.SessionView{}, err
	}
	registered := false
	defer func() {
		if !registered {
			m.releaseReservation()
		}
	}()

	inst, err := m.getInstance(ctx, agentType)
	if err != nil {
		return session.SessionView{}, err
	}
	response, err := inst.client.NewSession(ctx, cwd)
	if err != nil {
		return session.SessionView{}, fmt.Errorf("create ACP session: %w", err)
	}
	sess := session.New(session.ID{AgentType: agentType, AgentSessionID: string(response.SessionId)}, cwd)
	if err := m.registerSession(inst, sess); err != nil {
		closeCtx, cancel := context.WithTimeout(inst.ctx, closeSessionTimeout)
		defer cancel()
		_, _ = inst.client.CloseSession(closeCtx, sess.AgentSessionID())
		return session.SessionView{}, err
	}
	registered = true
	applyInitialMetadata(sess, response.Modes, response.ConfigOptions)
	return sess.View(), nil
}

func (m *Manager) LoadSession(ctx context.Context, id session.ID, cwd string) (session.SessionView, error) {
	return m.activateExisting(ctx, id, cwd, func(inst *AgentInstance) error {
		response, err := inst.client.LoadSession(ctx, id.AgentSessionID, cwd)
		if err == nil {
			applyInitialMetadataPlaceholder(response)
		}
		return err
	})
}

func (m *Manager) ResumeSession(ctx context.Context, id session.ID, cwd string) (session.SessionView, error) {
	return m.activateExisting(ctx, id, cwd, func(inst *AgentInstance) error {
		_, err := inst.client.ResumeSession(ctx, id.AgentSessionID, cwd)
		return err
	})
}

func (m *Manager) activateExisting(
	ctx context.Context,
	id session.ID,
	cwd string,
	activate func(*AgentInstance) error,
) (session.SessionView, error) {
	if err := m.reserveSession(); err != nil {
		return session.SessionView{}, err
	}
	registered := false
	defer func() {
		if !registered {
			m.releaseReservation()
		}
	}()
	if m.hasSession(id.String()) {
		return session.SessionView{}, ErrSessionExists
	}
	inst, err := m.getInstance(ctx, id.AgentType)
	if err != nil {
		return session.SessionView{}, err
	}
	if err := activate(inst); err != nil {
		return session.SessionView{}, err
	}
	sess := session.New(id, cwd)
	if err := m.registerSession(inst, sess); err != nil {
		return session.SessionView{}, err
	}
	registered = true
	return sess.View(), nil
}

func (m *Manager) ForkSession(ctx context.Context, qualifiedID string) (session.SessionView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	if err := m.reserveSession(); err != nil {
		return session.SessionView{}, err
	}
	registered := false
	defer func() {
		if !registered {
			m.releaseReservation()
		}
	}()

	response, err := ref.instance.client.ForkSession(ctx, ref.session.AgentSessionID(), ref.session.CWD())
	if err != nil {
		return session.SessionView{}, err
	}
	id := session.ID{AgentType: ref.instance.agentType, AgentSessionID: string(response.SessionId)}
	sess := session.New(id, ref.session.CWD())
	if err := m.registerSession(ref.instance, sess); err != nil {
		return session.SessionView{}, err
	}
	registered = true
	return sess.View(), nil
}

func (m *Manager) CloseSession(ctx context.Context, qualifiedID string) error {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return err
	}
	if turn := ref.session.Close(); turn != nil {
		turn.Cancel()
	}
	if _, err := ref.instance.client.CloseSession(ctx, ref.session.AgentSessionID()); err != nil {
		ref.session.ReopenAfterCloseFailure()
		return err
	}
	ref.instance.client.ForgetSession(ref.session.AgentSessionID())
	m.removeSession(qualifiedID, ref)
	return nil
}

func (m *Manager) DeleteSession(ctx context.Context, id session.ID) error {
	inst, err := m.getInstance(ctx, id.AgentType)
	if err != nil {
		return err
	}
	if _, err := inst.client.DeleteSession(ctx, id.AgentSessionID); err != nil {
		return err
	}
	if ref, err := m.session(id.String()); err == nil {
		if turn := ref.session.Close(); turn != nil {
			turn.Cancel()
		}
		inst.client.ForgetSession(id.AgentSessionID)
		m.removeSession(id.String(), ref)
	}
	return nil
}

func (m *Manager) SetMode(ctx context.Context, qualifiedID, mode string) (session.SessionView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	if _, err := ref.instance.client.SetSessionMode(ctx, ref.session.AgentSessionID(), mode); err != nil {
		return session.SessionView{}, err
	}
	ref.session.SetCurrentMode(mode)
	return ref.session.View(), nil
}

func (m *Manager) SetConfig(ctx context.Context, qualifiedID, configID, value string) error {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return err
	}
	return ref.instance.client.SetSessionConfigOption(ctx, ref.session.AgentSessionID(), configID, value)
}

func (m *Manager) Session(qualifiedID string) (session.SessionView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	return ref.session.View(), nil
}

func (m *Manager) Sessions() []session.SessionView {
	m.mu.Lock()
	refs := make([]sessionRef, 0, len(m.sessionIndex))
	for _, ref := range m.sessionIndex {
		refs = append(refs, ref)
	}
	m.mu.Unlock()

	views := make([]session.SessionView, 0, len(refs))
	for _, ref := range refs {
		views = append(views, ref.session.View())
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].LastUsed.Equal(views[j].LastUsed) {
			return views[i].ID.String() < views[j].ID.String()
		}
		return views[i].LastUsed.After(views[j].LastUsed)
	})
	return views
}

func (m *Manager) History(ctx context.Context, agentType driver.AgentType) ([]acp.SessionInfo, error) {
	inst, err := m.getInstance(ctx, agentType)
	if err != nil {
		return nil, err
	}
	response, err := inst.client.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	return append([]acp.SessionInfo(nil), response.Sessions...), nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return nil
	}
	m.closing = true
	m.cancel()
	instances := make([]*AgentInstance, 0, len(m.instances))
	for _, slot := range m.instances {
		if slot.instance != nil {
			instances = append(instances, slot.instance)
		}
	}
	refs := make([]sessionRef, 0, len(m.sessionIndex))
	for _, ref := range m.sessionIndex {
		refs = append(refs, ref)
	}
	m.mu.Unlock()

	for _, ref := range refs {
		if turn := ref.session.Close(); turn != nil {
			turn.Cancel()
		}
	}
	var firstErr error
	for _, inst := range instances {
		if err := inst.client.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	m.mu.Lock()
	m.instances = make(map[driver.AgentType]*instanceSlot)
	m.sessionIndex = make(map[string]sessionRef)
	m.reservations = 0
	m.mu.Unlock()
	return firstErr
}

const closeSessionTimeout = 3_000_000_000

func (m *Manager) reserveSession() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return ErrManagerClosing
	}
	if max := m.config.MaxSessions; max > 0 && len(m.sessionIndex)+m.reservations >= max {
		return ErrSessionLimit
	}
	m.reservations++
	return nil
}

func (m *Manager) releaseReservation() {
	m.mu.Lock()
	if m.reservations > 0 {
		m.reservations--
	}
	m.mu.Unlock()
}

func (m *Manager) registerSession(inst *AgentInstance, sess *session.Session) error {
	id := sess.ID().String()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return ErrManagerClosing
	}
	if _, exists := m.sessionIndex[id]; exists {
		return ErrSessionExists
	}
	if err := inst.addSession(sess.AgentSessionID(), sess); err != nil {
		return err
	}
	m.sessionIndex[id] = sessionRef{instance: inst, session: sess}
	if m.reservations > 0 {
		m.reservations--
	}
	return nil
}

func (m *Manager) getInstance(ctx context.Context, agentType driver.AgentType) (*AgentInstance, error) {
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return nil, ErrManagerClosing
		}
		if slot, ok := m.instances[agentType]; ok {
			ready := slot.ready
			if ready == nil {
				inst, err := slot.instance, slot.err
				m.mu.Unlock()
				return inst, err
			}
			m.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		m.nextGen++
		generation := m.nextGen
		slot := &instanceSlot{ready: make(chan struct{})}
		m.instances[agentType] = slot
		m.mu.Unlock()

		cl, err := m.factory(m.ctx, agentType)
		var inst *AgentInstance
		if err == nil {
			inst = newAgentInstance(m.ctx, agentType, generation, cl)
		}

		m.mu.Lock()
		current := m.instances[agentType]
		if current == slot {
			slot.instance = inst
			slot.err = err
			close(slot.ready)
			slot.ready = nil
			if err != nil {
				delete(m.instances, agentType)
			}
		}
		m.mu.Unlock()

		if err != nil {
			return nil, err
		}
		go m.watchInstance(inst)
		return inst, nil
	}
}

func (m *Manager) watchInstance(inst *AgentInstance) {
	<-inst.client.Done()
	m.mu.Lock()
	slot := m.instances[inst.agentType]
	if slot == nil || slot.instance != inst || inst.generation != slot.instance.generation {
		m.mu.Unlock()
		return
	}
	delete(m.instances, inst.agentType)
	for _, sess := range inst.allSessions() {
		delete(m.sessionIndex, sess.ID().String())
		if turn := sess.Close(); turn != nil {
			turn.Cancel()
		}
	}
	m.mu.Unlock()
}

func (m *Manager) session(qualifiedID string) (sessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.sessionIndex[qualifiedID]
	if !ok {
		return sessionRef{}, fmt.Errorf("%w: %s", ErrSessionNotFound, qualifiedID)
	}
	return ref, nil
}

func (m *Manager) hasSession(qualifiedID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessionIndex[qualifiedID]
	return ok
}

func (m *Manager) removeSession(qualifiedID string, expected sessionRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.sessionIndex[qualifiedID]
	if !ok || ref.instance != expected.instance || ref.session != expected.session {
		return
	}
	delete(m.sessionIndex, qualifiedID)
	expected.instance.removeSession(expected.session.AgentSessionID())
}

func applyInitialMetadata(sess *session.Session, modes *acp.SessionModeState, options []acp.SessionConfigOption) {
	if modes != nil {
		sess.SetCurrentMode(string(modes.CurrentModeId))
	}
	_ = options
}

func applyInitialMetadataPlaceholder(*acp.LoadSessionResponse) {}
