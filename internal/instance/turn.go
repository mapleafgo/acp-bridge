package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

// Status 是 MCP 返回的稳定 Turn 状态，不直接暴露内部 Session 状态机。
type Status string

const (
	// StatusIdle 表示 Session 没有正在执行的 Turn。
	StatusIdle Status = "idle"
	// StatusRunning 表示 Turn 正在执行。
	StatusRunning Status = "running"
	// StatusPermissionRequired 表示 Turn 正在等待权限决定。
	StatusPermissionRequired Status = "permission_required"
	// StatusCompleted 表示最近 Turn 已正常结束。
	StatusCompleted Status = "completed"
	// StatusInterrupted 表示最近 Turn 已被中断。
	StatusInterrupted Status = "interrupted"
	// StatusError 表示最近 Turn 以错误结束。
	StatusError Status = "error"
)

// ErrTurnNotInterruptible 表示 Turn 已终态或不再接受中断。
var ErrTurnNotInterruptible = errors.New("turn is not interruptible")

// ChatView 是 MCP 层可直接映射的 Session/Turn 值快照。
type ChatView struct {
	Status    Status
	SessionID string
	Title     string
	State     session.State
	TurnCount int
	Turn      session.TurnView
}

type promptResult struct {
	response *acp.PromptResponse
	err      error
}

var nextTurnID atomic.Uint64

// Chat 在活跃 Session 上启动一个新 Turn。同步等待超时只返回 running；
// handler context 取消会提交 interrupted 快照，但不会关闭 Session。
func (m *Manager) Chat(ctx context.Context, qualifiedID, prompt string, wait time.Duration) (ChatView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return ChatView{}, err
	}
	if err := ctx.Err(); err != nil {
		return ChatView{}, err
	}

	promptCtx, promptCancel := context.WithCancel(ref.instance.ctx)
	turn := session.NewTurn(fmt.Sprintf("t-%d", nextTurnID.Add(1)), promptCancel)
	if err := ref.session.BeginTurn(turn); err != nil {
		promptCancel()
		return ChatView{}, err
	}
	ref.session.Touch()
	go m.runTurn(ref, turn, promptCtx, prompt)

	view := turn.Wait(ctx, wait)
	if ctx.Err() != nil {
		interrupted, _ := m.interrupt(context.WithoutCancel(ctx), ref, turn, "handler cancelled")
		return interrupted, nil
	}
	view = m.withLiveUpdates(ref, view)
	return makeChatView(ref.session.View(), view), nil
}

func (m *Manager) runTurn(ref sessionRef, turn *session.Turn, promptCtx context.Context, prompt string) {
	startedAt := time.Now()
	defer turn.FinishController()
	defer ref.session.FinishTurn(turn)
	results := make(chan promptResult, 1)
	permissions := ref.instance.client.PermissionEvents(ref.session.AgentSessionID())
	go func() {
		response, err := ref.instance.client.Prompt(
			promptCtx,
			ref.session.AgentSessionID(),
			[]acp.ContentBlock{acp.TextBlock(prompt)},
		)
		results <- promptResult{response: response, err: err}
	}()

	for {
		select {
		case result := <-results:
			snapshot := session.TurnSnapshot{
				Updates: ref.instance.client.PopUpdates(ref.session.AgentSessionID()),
			}
			applySessionUpdates(ref.session, snapshot.Updates)
			if result.response != nil {
				snapshot.StopReason = string(result.response.StopReason)
			}
			var committed bool
			if result.err != nil {
				snapshot.Error = result.err.Error()
				committed = turn.Fail(snapshot)
			} else {
				committed = turn.Complete(snapshot)
			}
			if committed {
				logTurnFinished(promptCtx, ref, turn, result.err, time.Since(startedAt))
			}
			return
		case permission := <-permissions:
			if turn.RequirePermission(permissionView(permission)) {
				ref.session.SetTurnState(turn, session.StatePermissionPending)
			}
		case <-promptCtx.Done():
			return
		case <-ref.instance.ctx.Done():
			return
		}
	}
}

// Respond 提交当前权限请求的决定，并等待 Turn 下一次状态变化。
// requestID 已失效或不属于当前 Turn 时不会向 agent 发送响应。
func (m *Manager) Respond(
	ctx context.Context,
	qualifiedID string,
	requestID string,
	outcome string,
	wait time.Duration,
) (ChatView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return ChatView{}, err
	}
	turn := ref.session.CurrentTurn()
	if turn == nil {
		return ChatView{}, session.ErrTurnNotFound
	}
	current := turn.Snapshot()
	if current.Permission == nil || current.Permission.RequestID != requestID {
		return ChatView{}, session.ErrPermissionGone
	}

	response := acp.RequestPermissionResponse{
		Outcome: mapPermissionOutcome(outcome, current.Permission.Options),
	}
	if err := ref.instance.client.RespondPermission(ref.session.AgentSessionID(), requestID, response); err != nil {
		return ChatView{}, err
	}
	if err := turn.ResolvePermission(requestID); err != nil {
		return ChatView{}, err
	}
	ref.session.Touch()
	updated := turn.Wait(ctx, wait)
	if ctx.Err() != nil {
		interrupted, _ := m.interrupt(context.WithoutCancel(ctx), ref, turn, "handler cancelled")
		return interrupted, nil
	}
	if updated.State == session.TurnPermissionRequired {
		ref.session.SetTurnState(turn, session.StatePermissionPending)
	} else {
		ref.session.SetTurnState(turn, session.StatePrompting)
	}
	updated = m.withLiveUpdates(ref, updated)
	return makeChatView(ref.session.View(), updated), nil
}

// Progress 返回 Session 当前或最近 Turn 的稳定快照；turnID 非空时执行精确匹配。
func (m *Manager) Progress(qualifiedID, turnID string) (ChatView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return ChatView{}, err
	}
	view := ref.session.View()
	if view.Turn == nil {
		if turnID != "" {
			return ChatView{}, session.ErrTurnNotFound
		}
		return makeChatView(view, session.TurnView{}), nil
	}
	if turnID != "" && view.Turn.ID != turnID {
		return ChatView{}, session.ErrTurnMismatch
	}
	turn := m.withLiveUpdates(ref, *view.Turn)
	return makeChatView(ref.session.View(), turn), nil
}

func (m *Manager) withLiveUpdates(ref sessionRef, turn session.TurnView) session.TurnView {
	if turn.State == session.TurnRunning || turn.State == session.TurnPermissionRequired {
		turn.Updates = ref.instance.client.PeekUpdates(ref.session.AgentSessionID())
	}
	applySessionUpdates(ref.session, turn.Updates)
	return turn
}

func applySessionUpdates(sess *session.Session, updates []acp.SessionNotification) {
	for _, notification := range updates {
		update := notification.Update
		if update.SessionInfoUpdate != nil && update.SessionInfoUpdate.Title != nil {
			sess.SetTitle(*update.SessionInfoUpdate.Title)
		}
		if update.CurrentModeUpdate != nil {
			sess.SetCurrentMode(string(update.CurrentModeUpdate.CurrentModeId))
		}
		if update.ConfigOptionUpdate != nil {
			sess.SetConfigOptions(configOptionViews(update.ConfigOptionUpdate.ConfigOptions))
		}
		if update.AvailableCommandsUpdate != nil {
			commands := make([]session.AvailableCommandInfo, 0, len(update.AvailableCommandsUpdate.AvailableCommands))
			for _, command := range update.AvailableCommandsUpdate.AvailableCommands {
				view := session.AvailableCommandInfo{
					Name:        command.Name,
					Description: command.Description,
				}
				if command.Input != nil && command.Input.Unstructured != nil {
					view.InputHint = command.Input.Unstructured.Hint
				}
				commands = append(commands, view)
			}
			sess.SetAvailableCommands(commands)
		}
	}
}

func configOptionViews(options []acp.SessionConfigOption) []session.ConfigOptionInfo {
	views := make([]session.ConfigOptionInfo, 0, len(options))
	for _, option := range options {
		view := session.ConfigOptionInfo{}
		switch {
		case option.Select != nil:
			view.ID = string(option.Select.Id)
			view.Name = option.Select.Name
			view.Type = "select"
			view.Value = string(option.Select.CurrentValue)
		case option.Boolean != nil:
			view.ID = string(option.Boolean.Id)
			view.Name = option.Boolean.Name
			view.Type = "boolean"
			view.Value = strconv.FormatBool(option.Boolean.CurrentValue)
		default:
			continue
		}
		views = append(views, view)
	}
	return views
}

// Interrupt 中断与 qualifiedID、turnID 同时匹配的运行中 Turn。
// 本地 interrupted 终态一旦提交便不会因 ACP Cancel 失败而回滚。
func (m *Manager) Interrupt(ctx context.Context, qualifiedID, turnID string) (ChatView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
		return ChatView{}, err
	}
	turn := ref.session.CurrentTurn()
	if turn == nil {
		return ChatView{}, session.ErrTurnNotFound
	}
	if turn.ID() != turnID {
		return ChatView{}, session.ErrTurnMismatch
	}
	snapshot := turn.Snapshot()
	if snapshot.State != session.TurnRunning && snapshot.State != session.TurnPermissionRequired {
		return ChatView{}, ErrTurnNotInterruptible
	}
	view, interrupted := m.interrupt(ctx, ref, turn, "interrupted by user")
	if !interrupted {
		return ChatView{}, ErrTurnNotInterruptible
	}
	return view, nil
}

func (m *Manager) interrupt(
	budget context.Context,
	ref sessionRef,
	turn *session.Turn,
	reason string,
) (ChatView, bool) {
	startedAt := time.Now()
	snapshot := session.TurnSnapshot{
		Updates: ref.instance.client.PopUpdates(ref.session.AgentSessionID()),
		Error:   reason,
	}
	applySessionUpdates(ref.session, snapshot.Updates)
	if !turn.Interrupt(snapshot) {
		return makeChatView(ref.session.View(), turn.Snapshot()), false
	}
	turn.Cancel()

	cancelCtx, cancel := interruptContext(ref.instance.ctx, budget)
	defer cancel()
	if err := ref.instance.client.Cancel(cancelCtx, ref.session.AgentSessionID()); err != nil {
		slog.WarnContext(context.Background(), "ACP cancel 失败，本地 Turn 已中断",
			"agent_type", ref.instance.agentType,
			"session_id", ref.session.ID().String(),
			"turn_id", turn.ID(),
			"reason", reason,
			"elapsed", time.Since(startedAt),
			"error", err,
		)
	}
	select {
	case <-turn.ControllerDone():
	case <-cancelCtx.Done():
		if errors.Is(cancelCtx.Err(), context.DeadlineExceeded) {
			slog.Warn("等待 Turn controller 退出超时",
				"agent_type", ref.instance.agentType,
				"session_id", ref.session.ID().String(),
				"turn_id", turn.ID(),
				"reason", reason,
				"elapsed", time.Since(startedAt),
			)
		}
	}
	slog.Info("ACP Turn 已中断",
		"agent_type", ref.instance.agentType,
		"session_id", ref.session.ID().String(),
		"turn_id", turn.ID(),
		"reason", reason,
		"elapsed", time.Since(startedAt),
	)
	return makeChatView(ref.session.View(), turn.Snapshot()), true
}

func logTurnFinished(ctx context.Context, ref sessionRef, turn *session.Turn, err error, elapsed time.Duration) {
	attributes := []any{
		"agent_type", ref.instance.agentType,
		"session_id", ref.session.ID().String(),
		"turn_id", turn.ID(),
		"turn_status", turn.Snapshot().State,
		"elapsed", elapsed,
	}
	if err != nil {
		attributes = append(attributes, "error", err)
		slog.WarnContext(ctx, "ACP Turn 执行失败", attributes...)
		return
	}
	slog.InfoContext(ctx, "ACP Turn 已结束", attributes...)
}

const interruptTimeout = 3 * time.Second

func interruptContext(instanceCtx, budget context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(instanceCtx, interruptTimeout)
	if budget == nil {
		return ctx, cancel
	}
	if budget.Err() != nil {
		cancel()
		return ctx, cancel
	}
	stop := context.AfterFunc(budget, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func permissionView(event client.PermissionEvent) session.PermissionView {
	request := event.Request
	view := session.PermissionView{
		RequestID:  event.RequestID,
		ToolCallID: string(request.ToolCall.ToolCallId),
		Options:    make([]session.PermissionOption, 0, len(request.Options)),
	}
	if request.ToolCall.Title != nil {
		view.Title = *request.ToolCall.Title
	}
	if request.ToolCall.Kind != nil {
		view.Kind = string(*request.ToolCall.Kind)
	}
	for _, option := range request.Options {
		view.Options = append(view.Options, session.PermissionOption{
			ID:   string(option.OptionId),
			Name: option.Name,
			Kind: string(option.Kind),
		})
	}
	return view
}

func mapPermissionOutcome(outcome string, options []session.PermissionOption) acp.RequestPermissionOutcome {
	var prefix string
	switch outcome {
	case "allow":
		prefix = "allow"
	case "deny":
		prefix = "reject"
	default:
		return acp.NewRequestPermissionOutcomeCancelled()
	}
	for _, option := range options {
		if strings.HasPrefix(option.Kind, prefix) {
			return acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(option.ID))
		}
	}
	return acp.NewRequestPermissionOutcomeCancelled()
}

func makeChatView(sess session.SessionView, turn session.TurnView) ChatView {
	status := StatusIdle
	switch turn.State {
	case session.TurnRunning:
		status = StatusRunning
	case session.TurnPermissionRequired:
		status = StatusPermissionRequired
	case session.TurnCompleted:
		status = StatusCompleted
	case session.TurnInterrupted:
		status = StatusInterrupted
	case session.TurnError:
		status = StatusError
	}
	return ChatView{
		Status:    status,
		SessionID: sess.ID.String(),
		Title:     sess.Title,
		State:     sess.State,
		TurnCount: sess.TurnCount,
		Turn:      turn,
	}
}
