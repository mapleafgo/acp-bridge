package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

var (
	// ErrSessionLimit 表示活跃 Session 与预留总数已达到配置上限。
	ErrSessionLimit = errors.New("session limit reached")
	// ErrSessionNotFound 表示 qualified ID 不在活跃索引中。
	ErrSessionNotFound = session.ErrSessionNotFound
	// ErrSessionExists 表示 load 或 resume 的 Session 已经活跃。
	ErrSessionExists = session.ErrSessionExists
	// ErrSessionActive 表示删除历史前必须先关闭活跃 Session。
	ErrSessionActive = errors.New("session is active; close it before deleting")
	// ErrManagerClosing 表示 Manager 已开始关闭，不再接受新操作。
	ErrManagerClosing = errors.New("agent instance manager is closing")
	// ErrInstanceChanged 表示 ACP 调用期间所属实例已经退出或被替换。
	ErrInstanceChanged = errors.New("agent instance changed during operation")
)

// SessionLimitError 报告容量检查时计入预留名额的当前数量和配置上限。
type SessionLimitError struct {
	Active int
	Limit  int
}

// Error 返回包含当前计数与配置上限的稳定错误文本。
func (e *SessionLimitError) Error() string {
	return fmt.Sprintf("session limit reached: active=%d limit=%d", e.Active, e.Limit)
}

// Unwrap 允许调用方使用 errors.Is(err, ErrSessionLimit) 判断容量错误。
func (e *SessionLimitError) Unwrap() error {
	return ErrSessionLimit
}

type instanceSlot struct {
	ready    chan struct{}
	instance *AgentInstance
	err      error
}

type sessionRef struct {
	instance *AgentInstance
	session  *session.Session
}

type sessionOperation struct {
	done chan struct{}
}

type initialSessionMetadata struct {
	modes   *acp.SessionModeState
	options []acp.SessionConfigOption
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
	sessionOps   map[string]*sessionOperation
	reservations int
	reservedDone chan struct{}
	closing      bool
	closeDone    chan struct{}
	closeErr     error
	nextGen      uint64
}

// NewManager 创建常驻实例管理器。factory 为 nil 时使用生产环境 Driver；
// 返回对象可并发使用，最终必须调用 Close 回收全部子进程。
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
		sessionOps:   make(map[string]*sessionOperation),
		reservedDone: closedSignal(),
		closeDone:    make(chan struct{}),
	}
}

// CreateSession 在指定 agent 上创建并注册永久 Session。
// 达到容量上限、Manager 正在关闭或远端创建失败时不会占用本地名额。
func (m *Manager) CreateSession(ctx context.Context, agentType driver.AgentType, cwd string) (session.SessionView, error) {
	startedAt := time.Now()
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
	operationCtx, cancel := m.operationContext()
	defer cancel()
	response, err := inst.client.NewSession(operationCtx, cwd)
	if err != nil {
		err = fmt.Errorf("create ACP session: %w", err)
		logSessionOperationFailure(ctx, "create", agentType, "", startedAt, err)
		return session.SessionView{}, err
	}
	sess := session.New(session.ID{AgentType: agentType, AgentSessionID: string(response.SessionId)}, cwd)
	if err := m.registerSession(inst, sess); err != nil {
		m.rollbackCreatedSession(inst, sess, err)
		logSessionOperationFailure(ctx, "create", agentType, sess.ID().String(), startedAt, err)
		return session.SessionView{}, err
	}
	registered = true
	applyInitialMetadata(sess, response.Modes, response.ConfigOptions)
	view := sess.View()
	slog.InfoContext(ctx, "ACP Session 已创建",
		"agent_type", agentType,
		"session_id", view.ID.String(),
		"elapsed", time.Since(startedAt),
	)
	return view, nil
}

// LoadSession 激活一个持久化 Session；已经活跃的 qualified ID 返回 ErrSessionExists。
func (m *Manager) LoadSession(ctx context.Context, id session.ID, cwd string) (session.SessionView, error) {
	startedAt := time.Now()
	view, err := m.activateExisting(ctx, id, cwd, "load", func(operationCtx context.Context, inst *AgentInstance) (initialSessionMetadata, error) {
		response, err := inst.client.LoadSession(operationCtx, id.AgentSessionID, cwd)
		if err != nil {
			logSessionOperationFailure(ctx, "load", id.AgentType, id.String(), startedAt, err)
			return initialSessionMetadata{}, err
		}
		return initialSessionMetadata{modes: response.Modes, options: response.ConfigOptions}, nil
	})
	if err == nil {
		slog.InfoContext(ctx, "ACP Session 已加载",
			"agent_type", id.AgentType,
			"session_id", id.String(),
			"elapsed", time.Since(startedAt),
		)
	}
	return view, err
}

// ResumeSession 恢复一个已关闭 Session；已经活跃的 qualified ID 返回 ErrSessionExists。
func (m *Manager) ResumeSession(ctx context.Context, id session.ID, cwd string) (session.SessionView, error) {
	startedAt := time.Now()
	view, err := m.activateExisting(ctx, id, cwd, "resume", func(operationCtx context.Context, inst *AgentInstance) (initialSessionMetadata, error) {
		response, err := inst.client.ResumeSession(operationCtx, id.AgentSessionID, cwd)
		if err != nil {
			logSessionOperationFailure(ctx, "resume", id.AgentType, id.String(), startedAt, err)
			return initialSessionMetadata{}, err
		}
		return initialSessionMetadata{modes: response.Modes, options: response.ConfigOptions}, nil
	})
	if err == nil {
		slog.InfoContext(ctx, "ACP Session 已恢复",
			"agent_type", id.AgentType,
			"session_id", id.String(),
			"elapsed", time.Since(startedAt),
		)
	}
	return view, err
}

func (m *Manager) activateExisting(
	ctx context.Context,
	id session.ID,
	cwd string,
	operation string,
	activate func(context.Context, *AgentInstance) (initialSessionMetadata, error),
) (session.SessionView, error) {
	startedAt := time.Now()
	releaseOperation, err := m.beginSessionOperation(ctx, id.String())
	if err != nil {
		return session.SessionView{}, err
	}
	defer releaseOperation()
	if m.hasSession(id.String()) {
		return session.SessionView{}, ErrSessionExists
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
	inst, err := m.getInstance(ctx, id.AgentType)
	if err != nil {
		return session.SessionView{}, err
	}
	operationCtx, cancel := m.operationContext()
	defer cancel()
	metadata, err := activate(operationCtx, inst)
	if err != nil {
		return session.SessionView{}, err
	}
	sess := session.New(id, cwd)
	applyInitialMetadata(sess, metadata.modes, metadata.options)
	if err := m.registerSession(inst, sess); err != nil {
		// Load/Resume 已在远端生效；注册失败时补偿关闭，避免留下无本地索引的远端会话。
		// 不走 duplicate-ID 的实例隔离路径，以免误伤并发 Create 已注册的同 ID 会话。
		if !errors.Is(err, ErrSessionExists) {
			m.compensateRemoteSession(inst, sess.AgentSessionID())
		}
		logSessionOperationFailure(ctx, operation, id.AgentType, id.String(), startedAt, err)
		return session.SessionView{}, err
	}
	registered = true
	return sess.View(), nil
}

// ForkSession 从活跃 Session 派生一个新的永久 Session。
// 远端已创建但本地注册失败时会补偿关闭；重复原始 ID 会隔离所属实例。
func (m *Manager) ForkSession(ctx context.Context, qualifiedID string) (session.SessionView, error) {
	startedAt := time.Now()
	releaseOperation, err := m.beginSessionOperation(ctx, qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	defer releaseOperation()
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

	operationCtx, cancel := m.operationContext()
	defer cancel()
	response, err := ref.instance.client.ForkSession(operationCtx, ref.session.AgentSessionID(), ref.session.CWD())
	if err != nil {
		logSessionOperationFailure(ctx, "fork", ref.instance.agentType, qualifiedID, startedAt, err)
		return session.SessionView{}, err
	}
	if err := m.validateSessionRef(qualifiedID, ref); err != nil {
		sess := session.New(
			session.ID{AgentType: ref.instance.agentType, AgentSessionID: string(response.SessionId)},
			ref.session.CWD(),
		)
		m.rollbackCreatedSession(ref.instance, sess, err)
		logSessionOperationFailure(ctx, "fork", ref.instance.agentType, qualifiedID, startedAt, err)
		return session.SessionView{}, err
	}
	id := session.ID{AgentType: ref.instance.agentType, AgentSessionID: string(response.SessionId)}
	sess := session.New(id, ref.session.CWD())
	if err := m.registerSession(ref.instance, sess); err != nil {
		m.rollbackCreatedSession(ref.instance, sess, err)
		logSessionOperationFailure(ctx, "fork", ref.instance.agentType, qualifiedID, startedAt, err)
		return session.SessionView{}, err
	}
	registered = true
	view := sess.View()
	slog.InfoContext(ctx, "ACP Session 已派生",
		"agent_type", view.ID.AgentType,
		"session_id", view.ID.String(),
		"parent_session_id", qualifiedID,
		"elapsed", time.Since(startedAt),
	)
	return view, nil
}

// CloseSession 中断当前 Turn 并关闭远端 Session。远端关闭失败时 Session
// 恢复为 idle 并继续保留在活跃索引中，调用方可以重试。
func (m *Manager) CloseSession(ctx context.Context, qualifiedID string) error {
	startedAt := time.Now()
	ref, turn, releaseOperation, err := m.beginCloseOperation(ctx, qualifiedID)
	if err != nil {
		return err
	}
	defer releaseOperation()
	if turn != nil && isInterruptible(turn.Snapshot().State) {
		_, _ = m.interrupt(ctx, ref, turn, "session closing")
	}
	if _, err := ref.instance.client.CloseSession(ctx, ref.session.AgentSessionID()); err != nil {
		ref.session.ReopenAfterCloseFailure()
		slog.WarnContext(ctx, "ACP Session 关闭失败",
			"agent_type", ref.instance.agentType,
			"session_id", qualifiedID,
			"elapsed", time.Since(startedAt),
			"error", err,
		)
		return err
	}
	ref.instance.client.ForgetSession(ref.session.AgentSessionID())
	m.removeSession(qualifiedID, ref)
	slog.InfoContext(ctx, "ACP Session 已关闭",
		"agent_type", ref.instance.agentType,
		"session_id", qualifiedID,
		"elapsed", time.Since(startedAt),
	)
	return nil
}

// DeleteSession 永久删除未激活的历史 Session；活跃 Session 返回 ErrSessionActive，
// 且不会向 agent 发送删除请求。远端删除一旦成功即视为成功，即使随后实例退出。
func (m *Manager) DeleteSession(ctx context.Context, id session.ID) error {
	startedAt := time.Now()
	releaseOperation, err := m.beginSessionOperation(ctx, id.String())
	if err != nil {
		return err
	}
	defer releaseOperation()
	if m.hasSession(id.String()) {
		return ErrSessionActive
	}
	inst, err := m.getInstance(ctx, id.AgentType)
	if err != nil {
		return err
	}
	if _, err := inst.client.DeleteSession(ctx, id.AgentSessionID); err != nil {
		logSessionOperationFailure(ctx, "delete", id.AgentType, id.String(), startedAt, err)
		return err
	}
	if err := m.validateInstance(inst); err != nil {
		slog.WarnContext(ctx, "ACP 历史 Session 已删除，但实例已变更",
			"agent_type", id.AgentType,
			"session_id", id.String(),
			"elapsed", time.Since(startedAt),
			"error", err,
		)
		return nil
	}
	slog.InfoContext(ctx, "ACP 历史 Session 已删除",
		"agent_type", id.AgentType,
		"session_id", id.String(),
		"elapsed", time.Since(startedAt),
	)
	return nil
}

// SetMode 修改活跃 Session 的权限模式，并在成功后更新本地快照。
func (m *Manager) SetMode(ctx context.Context, qualifiedID, mode string) (session.SessionView, error) {
	startedAt := time.Now()
	releaseOperation, err := m.beginSessionOperation(ctx, qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	defer releaseOperation()
	ref, err := m.session(qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	if _, err := ref.instance.client.SetSessionMode(ctx, ref.session.AgentSessionID(), mode); err != nil {
		logSessionOperationFailure(ctx, "set_mode", ref.instance.agentType, qualifiedID, startedAt, err)
		return session.SessionView{}, err
	}
	if err := m.validateSessionRef(qualifiedID, ref); err != nil {
		logSessionOperationFailure(ctx, "set_mode", ref.instance.agentType, qualifiedID, startedAt, err)
		return session.SessionView{}, err
	}
	ref.session.SetCurrentMode(mode)
	ref.session.Touch()
	view := ref.session.View()
	slog.InfoContext(ctx, "ACP Session 模式已更新",
		"agent_type", ref.instance.agentType,
		"session_id", qualifiedID,
		"mode", mode,
		"elapsed", time.Since(startedAt),
	)
	return view, nil
}

// SetConfig 修改活跃 Session 的一个 agent 配置项；关闭或实例更替期间不会提交旧响应。
func (m *Manager) SetConfig(ctx context.Context, qualifiedID, configID, value string) error {
	startedAt := time.Now()
	releaseOperation, err := m.beginSessionOperation(ctx, qualifiedID)
	if err != nil {
		return err
	}
	defer releaseOperation()
	ref, err := m.session(qualifiedID)
	if err != nil {
		return err
	}
	if err := ref.instance.client.SetSessionConfigOption(ctx, ref.session.AgentSessionID(), configID, value); err != nil {
		logSessionOperationFailure(ctx, "set_config", ref.instance.agentType, qualifiedID, startedAt, err)
		return err
	}
	if err := m.validateSessionRef(qualifiedID, ref); err != nil {
		logSessionOperationFailure(ctx, "set_config", ref.instance.agentType, qualifiedID, startedAt, err)
		return err
	}
	ref.session.Touch()
	slog.InfoContext(ctx, "ACP Session 配置已更新",
		"agent_type", ref.instance.agentType,
		"session_id", qualifiedID,
		"config_id", configID,
		"elapsed", time.Since(startedAt),
	)
	return nil
}

// Session 返回指定活跃 Session 的并发安全值快照。
func (m *Manager) Session(qualifiedID string) (session.SessionView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return session.SessionView{}, err
	}
	return ref.session.View(), nil
}

// Sessions 返回全部活跃 Session，按最后使用时间倒序排列且不分页。
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

// History 查询指定 agent 的持久化 Session 列表；该操作可能惰性启动实例。
func (m *Manager) History(ctx context.Context, agentType driver.AgentType) ([]acp.SessionInfo, error) {
	startedAt := time.Now()
	inst, err := m.getInstance(ctx, agentType)
	if err != nil {
		return nil, err
	}
	response, err := inst.client.ListSessions(ctx)
	if err != nil {
		logSessionOperationFailure(ctx, "history", agentType, "", startedAt, err)
		return nil, err
	}
	sessions := append([]acp.SessionInfo(nil), response.Sessions...)
	if err := m.validateInstance(inst); err != nil {
		slog.WarnContext(ctx, "ACP Session 历史已查询，但实例已变更",
			"agent_type", agentType,
			"count", len(sessions),
			"elapsed", time.Since(startedAt),
			"error", err,
		)
		return sessions, nil
	}
	slog.InfoContext(ctx, "ACP Session 历史已查询",
		"agent_type", agentType,
		"count", len(sessions),
		"elapsed", time.Since(startedAt),
	)
	return sessions, nil
}

// Close 幂等关闭 Manager。首次调用拒绝新操作，在 ctx 的统一预算内中断 Turn、
// 关闭 Session 与 Client，并等待或强制回收全部 agent 子进程。
func (m *Manager) Close(ctx context.Context) error {
	startedAt := time.Now()
	m.mu.Lock()
	if m.closing {
		done := m.closeDone
		m.mu.Unlock()
		select {
		case <-done:
			m.mu.Lock()
			err := m.closeErr
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.closing = true
	starting := make([]<-chan struct{}, 0, len(m.instances))
	for _, slot := range m.instances {
		if slot.ready != nil {
			starting = append(starting, slot.ready)
		}
	}
	operations := make([]<-chan struct{}, 0, len(m.sessionOps))
	for _, operation := range m.sessionOps {
		operations = append(operations, operation.done)
	}
	reservedDone := m.reservedDone
	m.mu.Unlock()

	var (
		closeErrors   []error
		closeErrorsMu sync.Mutex
	)
	addCloseError := func(err error) {
		if err == nil {
			return
		}
		closeErrorsMu.Lock()
		closeErrors = append(closeErrors, err)
		closeErrorsMu.Unlock()
	}
	for _, ready := range starting {
		select {
		case <-ready:
		case <-ctx.Done():
			addCloseError(ctx.Err())
		}
	}
	for _, done := range operations {
		select {
		case <-done:
		case <-ctx.Done():
			addCloseError(ctx.Err())
		}
	}
	select {
	case <-reservedDone:
	case <-ctx.Done():
		addCloseError(ctx.Err())
	}

	m.mu.Lock()
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

	var closeSessions sync.WaitGroup
	for _, ref := range refs {
		closeSessions.Go(func() {
			turn := ref.session.Close()
			if turn != nil && isInterruptible(turn.Snapshot().State) {
				_, _ = m.interrupt(ctx, ref, turn, "bridge shutting down")
			}
			_, err := ref.instance.client.CloseSession(ctx, ref.session.AgentSessionID())
			addCloseError(err)
			ref.instance.client.ForgetSession(ref.session.AgentSessionID())
		})
	}
	closeSessions.Wait()

	var closeClients sync.WaitGroup
	for _, inst := range instances {
		closeClients.Go(func() {
			addCloseError(inst.client.Close(ctx))
		})
	}
	closeClients.Wait()
	m.cancel()

	m.mu.Lock()
	m.instances = make(map[driver.AgentType]*instanceSlot)
	m.sessionIndex = make(map[string]sessionRef)
	m.sessionOps = make(map[string]*sessionOperation)
	if m.reservations > 0 {
		close(m.reservedDone)
	}
	m.reservations = 0
	m.closeErr = errors.Join(closeErrors...)
	close(m.closeDone)
	closeErr := m.closeErr
	m.mu.Unlock()
	if closeErr != nil {
		slog.WarnContext(ctx, "ACP Manager 关闭完成但存在错误",
			"instances", len(instances),
			"sessions", len(refs),
			"elapsed", time.Since(startedAt),
			"error", closeErr,
		)
	} else {
		slog.InfoContext(ctx, "ACP Manager 已关闭",
			"instances", len(instances),
			"sessions", len(refs),
			"elapsed", time.Since(startedAt),
		)
	}
	return closeErr
}

const closeSessionTimeout = 3 * time.Second

func (m *Manager) reserveSession() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return ErrManagerClosing
	}
	if max := m.config.MaxSessions; max > 0 && len(m.sessionIndex)+m.reservations >= max {
		return &SessionLimitError{
			Active: len(m.sessionIndex) + m.reservations,
			Limit:  max,
		}
	}
	if m.reservations == 0 {
		m.reservedDone = make(chan struct{})
	}
	m.reservations++
	return nil
}

func (m *Manager) releaseReservation() {
	m.mu.Lock()
	m.finishReservationLocked()
	m.mu.Unlock()
}

func (m *Manager) registerSession(inst *AgentInstance, sess *session.Session) error {
	id := sess.ID().String()
	if _, err := session.ParseID(id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return ErrManagerClosing
	}
	if !m.instanceCurrentLocked(inst) {
		return ErrInstanceChanged
	}
	if _, exists := m.sessionIndex[id]; exists {
		return ErrSessionExists
	}
	if err := inst.addSession(sess.AgentSessionID(), sess); err != nil {
		return err
	}
	m.sessionIndex[id] = sessionRef{instance: inst, session: sess}
	m.finishReservationLocked()
	return nil
}

func (m *Manager) finishReservationLocked() {
	if m.reservations == 0 {
		return
	}
	m.reservations--
	if m.reservations == 0 {
		close(m.reservedDone)
	}
}

func (m *Manager) rollbackCreatedSession(inst *AgentInstance, sess *session.Session, registerErr error) {
	if errors.Is(registerErr, ErrSessionExists) {
		m.invalidateInstance(inst)
		return
	}
	m.compensateRemoteSession(inst, sess.AgentSessionID())
}

// compensateRemoteSession 在本地未注册成功时尽力关闭远端 Session。
// 使用独立预算，避免依赖可能已取消的实例/Manager context。
func (m *Manager) compensateRemoteSession(inst *AgentInstance, agentSessionID string) {
	closeCtx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
	defer cancel()
	_, _ = inst.client.CloseSession(closeCtx, agentSessionID)
}

func (m *Manager) invalidateInstance(inst *AgentInstance) {
	sessions := m.detachInstance(inst)
	slog.Error("agent 返回重复 Session ID，实例已隔离",
		"agent_type", inst.agentType,
		"generation", inst.generation,
		"sessions", len(sessions),
		"error", ErrSessionExists,
	)
	for _, sess := range sessions {
		inst.client.ForgetSession(sess.AgentSessionID())
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
	defer cancel()
	_ = inst.client.Close(closeCtx)
}

func (m *Manager) getInstance(ctx context.Context, agentType driver.AgentType) (*AgentInstance, error) {
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
			return slot.instance, slot.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	m.nextGen++
	generation := m.nextGen
	slot := &instanceSlot{ready: make(chan struct{})}
	m.instances[agentType] = slot
	m.mu.Unlock()

	startedAt := time.Now()
	slog.InfoContext(m.ctx, "启动 ACP agent 实例",
		"agent_type", agentType,
		"generation", generation,
	)
	cl, err := m.factory(m.ctx, agentType)
	var inst *AgentInstance
	if err == nil {
		inst = newAgentInstance(m.ctx, agentType, generation, cl)
	}

	m.mu.Lock()
	current := m.instances[agentType]
	rejected := current != slot || m.closing
	if current == slot && rejected {
		slot.err = ErrManagerClosing
		close(slot.ready)
		slot.ready = nil
		delete(m.instances, agentType)
	} else if current == slot {
		slot.instance = inst
		slot.err = err
		close(slot.ready)
		slot.ready = nil
		if err != nil {
			delete(m.instances, agentType)
		}
	}
	m.mu.Unlock()

	if rejected {
		if cl != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
			_ = cl.Close(closeCtx)
			cancel()
		}
		return nil, ErrManagerClosing
	}
	if err != nil {
		slog.ErrorContext(m.ctx, "ACP agent 实例启动失败",
			"agent_type", agentType,
			"generation", generation,
			"elapsed", time.Since(startedAt),
			"error", err,
		)
		return nil, err
	}
	slog.InfoContext(m.ctx, "ACP agent 实例已启动",
		"agent_type", agentType,
		"generation", generation,
		"elapsed", time.Since(startedAt),
	)
	go m.watchInstance(inst)
	return inst, nil
}

func (m *Manager) watchInstance(inst *AgentInstance) {
	<-inst.client.Done()
	startedAt := time.Now()
	closeCtx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
	closeErr := inst.client.Close(closeCtx)
	cancel()
	exitErr := inst.client.Err()
	sessions := m.detachInstance(inst)
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		slog.Info("ACP agent 实例已关闭",
			"agent_type", inst.agentType,
			"generation", inst.generation,
			"sessions", len(sessions),
			"elapsed", time.Since(startedAt),
		)
	} else {
		attributes := []any{
			"agent_type", inst.agentType,
			"generation", inst.generation,
			"sessions", len(sessions),
			"elapsed", time.Since(startedAt),
		}
		if err := errors.Join(exitErr, closeErr); err != nil {
			attributes = append(attributes, "error", err)
		}
		slog.Warn("ACP agent 实例意外退出", attributes...)
	}
}

func (m *Manager) detachInstance(inst *AgentInstance) []*session.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := m.instances[inst.agentType]
	if slot == nil || slot.instance != inst || inst.generation != slot.instance.generation {
		return nil
	}
	delete(m.instances, inst.agentType)
	sessions := inst.allSessions()
	for _, sess := range sessions {
		delete(m.sessionIndex, sess.ID().String())
		if turn := sess.Close(); turn != nil {
			turn.Cancel()
		}
	}
	return sessions
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

func (m *Manager) beginSessionOperation(ctx context.Context, qualifiedID string) (func(), error) {
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return nil, ErrManagerClosing
		}
		if operation, exists := m.sessionOps[qualifiedID]; exists {
			done := operation.done
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-m.ctx.Done():
				return nil, ErrManagerClosing
			}
		}
		operation := &sessionOperation{done: make(chan struct{})}
		m.sessionOps[qualifiedID] = operation
		m.mu.Unlock()
		return m.releaseSessionOperation(qualifiedID, operation), nil
	}
}

func (m *Manager) beginCloseOperation(
	ctx context.Context,
	qualifiedID string,
) (sessionRef, *session.Turn, func(), error) {
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return sessionRef{}, nil, nil, ErrManagerClosing
		}
		ref, exists := m.sessionIndex[qualifiedID]
		if !exists {
			m.mu.Unlock()
			return sessionRef{}, nil, nil, fmt.Errorf("%w: %s", ErrSessionNotFound, qualifiedID)
		}
		if operation, busy := m.sessionOps[qualifiedID]; busy {
			if ref.session.View().State == session.StateClosing {
				m.mu.Unlock()
				return sessionRef{}, nil, nil, session.ErrSessionClosed
			}
			done := operation.done
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return sessionRef{}, nil, nil, ctx.Err()
			case <-m.ctx.Done():
				return sessionRef{}, nil, nil, ErrManagerClosing
			}
		}
		operation := &sessionOperation{done: make(chan struct{})}
		m.sessionOps[qualifiedID] = operation
		turn, err := ref.session.BeginClose()
		if err != nil {
			delete(m.sessionOps, qualifiedID)
			close(operation.done)
			m.mu.Unlock()
			return sessionRef{}, nil, nil, err
		}
		m.mu.Unlock()
		return ref, turn, m.releaseSessionOperation(qualifiedID, operation), nil
	}
}

func (m *Manager) releaseSessionOperation(qualifiedID string, expected *sessionOperation) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.sessionOps[qualifiedID] == expected {
				delete(m.sessionOps, qualifiedID)
			}
			close(expected.done)
			m.mu.Unlock()
		})
	}
}

func (m *Manager) validateSessionRef(qualifiedID string, expected sessionRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, exists := m.sessionIndex[qualifiedID]
	if !exists || ref.instance != expected.instance || ref.session != expected.session {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, qualifiedID)
	}
	if !m.instanceCurrentLocked(expected.instance) {
		return ErrInstanceChanged
	}
	if expected.session.View().State == session.StateClosing {
		return session.ErrSessionClosed
	}
	return nil
}

func (m *Manager) validateInstance(expected *AgentInstance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.instanceCurrentLocked(expected) {
		return ErrInstanceChanged
	}
	return nil
}

func (m *Manager) instanceCurrentLocked(expected *AgentInstance) bool {
	slot := m.instances[expected.agentType]
	return slot != nil &&
		slot.ready == nil &&
		slot.instance == expected &&
		slot.instance.generation == expected.generation
}

func logSessionOperationFailure(
	ctx context.Context,
	operation string,
	agentType driver.AgentType,
	qualifiedID string,
	startedAt time.Time,
	err error,
) {
	attributes := []any{
		"operation", operation,
		"agent_type", agentType,
		"elapsed", time.Since(startedAt),
		"error", err,
	}
	if qualifiedID != "" {
		attributes = append(attributes, "session_id", qualifiedID)
	}
	slog.WarnContext(ctx, "ACP Session 操作失败", attributes...)
}

func closedSignal() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
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
	sess.SetConfigOptions(configOptionViews(options))
}

func (m *Manager) operationContext() (context.Context, context.CancelFunc) {
	if m.config.DefaultTimeout > 0 {
		return context.WithTimeout(m.ctx, m.config.DefaultTimeout)
	}
	return context.WithCancel(m.ctx)
}

func isInterruptible(state session.TurnState) bool {
	return state == session.TurnRunning || state == session.TurnPermissionRequired
}
