package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/session"
)

type Status string

const (
	StatusIdle               Status = "idle"
	StatusRunning            Status = "running"
	StatusPermissionRequired Status = "permission_required"
	StatusCompleted          Status = "completed"
	StatusInterrupted        Status = "interrupted"
	StatusError              Status = "error"
)

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

func (m *Manager) Chat(ctx context.Context, qualifiedID, prompt string, wait time.Duration) (ChatView, error) {
	ref, err := m.session(qualifiedID)
	if err != nil {
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
		return m.interrupt(ref, turn, "handler cancelled"), nil
	}
	return makeChatView(ref.session.View(), view), nil
}

func (m *Manager) runTurn(ref sessionRef, turn *session.Turn, promptCtx context.Context, prompt string) {
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
				ref.session.FinishTurn(turn)
			}
			return
		case permission := <-permissions:
			if turn.RequirePermission(permissionView(permission)) {
				ref.session.SetTurnState(turn, session.StatePermissionPending)
			}
		case <-ref.instance.ctx.Done():
			return
		}
	}
}

func (m *Manager) Respond(
	ctx context.Context,
	qualifiedID string,
	requestID string,
	outcome string,
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
	updated := turn.Snapshot()
	if updated.State == session.TurnPermissionRequired {
		ref.session.SetTurnState(turn, session.StatePermissionPending)
	} else {
		ref.session.SetTurnState(turn, session.StatePrompting)
	}
	return makeChatView(ref.session.View(), updated), nil
}

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
	return makeChatView(view, *view.Turn), nil
}

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
	return m.interrupt(ref, turn, "interrupted by user"), nil
}

func (m *Manager) interrupt(ref sessionRef, turn *session.Turn, reason string) ChatView {
	snapshot := session.TurnSnapshot{
		Updates: ref.instance.client.PeekUpdates(ref.session.AgentSessionID()),
		Error:   reason,
	}
	if !turn.Interrupt(snapshot) {
		return makeChatView(ref.session.View(), turn.Snapshot())
	}
	ref.session.FinishTurn(turn)
	turn.Cancel()

	cancelCtx, cancel := context.WithTimeout(ref.instance.ctx, 3*time.Second)
	defer cancel()
	if err := ref.instance.client.Cancel(cancelCtx, ref.session.AgentSessionID()); err != nil {
		slog.WarnContext(context.Background(), "ACP cancel 失败，本地 Turn 已中断",
			"session_id", ref.session.ID().String(),
			"turn_id", turn.ID(),
			"error", err,
		)
	}
	return makeChatView(ref.session.View(), turn.Snapshot())
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
