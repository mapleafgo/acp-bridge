package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// acpClient is the subset of *client.Client methods used by MCP tool
// handlers. Defined as an interface so tests can inject mocks without
// launching a real agent subprocess.
type acpClient interface {
	NewSession(ctx context.Context, cwd string) (*acp.NewSessionResponse, error)
	Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResponse, error)
	Cancel(ctx context.Context, sessionID string) error
	CloseSession(ctx context.Context, sessionID string) (*acp.CloseSessionResponse, error)
	ListSessions(ctx context.Context) (*acp.ListSessionsResponse, error)
	LoadSession(ctx context.Context, sessionID string) (*acp.LoadSessionResponse, error)
	ResumeSession(ctx context.Context, sessionID string) (*acp.ResumeSessionResponse, error)
	SetSessionMode(ctx context.Context, sessionID, modeID string) (*acp.SetSessionModeResponse, error)
	SetSessionConfigOption(ctx context.Context, sessionID, configID, valueID string) error
	ForkSession(ctx context.Context, sessionID string) (*acp.UnstableForkSessionResponse, error)
	DeleteSession(ctx context.Context, sessionID string) (*acp.UnstableDeleteSessionResponse, error)
	RespondPermission(requestID string, resp acp.RequestPermissionResponse) error
	PopUpdates(sessionID string) []acp.SessionNotification
	PeekUpdates(sessionID string) []acp.SessionNotification
	PermissionSignal() <-chan acp.RequestPermissionRequest
}

// clientFactory creates an ACP client for the given agent type.
type clientFactory func(ctx context.Context, agentType string) (acpClient, error)

// promptTurn tracks the current Prompt call and retains its terminal snapshot
// until the session starts another turn or is closed.
type promptTurn struct {
	mu     sync.Mutex
	done   chan struct{}
	result *acp.PromptResponse
	err    error
	// TurnID is a bridge-assigned identifier for this prompt turn,
	// returned to the MCP client for tracking and cancellation.
	turnID string
	// permReq holds the permission request that interrupted this turn,
	// so acp_respond can map the user's outcome to an option_id.
	permReq *acp.RequestPermissionRequest
	// cancel cancels the prompt context, used by acp_interrupt/acp_close
	// to unblock an in-flight prompt goroutine.
	cancel context.CancelFunc
	// terminal snapshots are immutable after they are first stored, allowing
	// acp_progress to return the same completed or interrupted result.
	terminal     bool
	finalResult  chatResultJSON
	finalIsError bool
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func okResult(sid string) (*sdk.CallToolResult, toolResult, error) {
	return &sdk.CallToolResult{}, toolResult{Status: "ok", SessionID: sid}, nil
}

func okSessionResult(sess *session.Session) (*sdk.CallToolResult, toolResult, error) {
	return &sdk.CallToolResult{}, toolResult{
		Status:    "ok",
		SessionID: string(sess.ID),
		Title:     sess.Title,
	}, nil
}

func failResult(msg string) (*sdk.CallToolResult, toolResult, error) {
	return &sdk.CallToolResult{IsError: true}, toolResult{Status: "error", Error: msg}, nil
}

func chatErr(msg string) (*sdk.CallToolResult, chatResultJSON, error) {
	return &sdk.CallToolResult{IsError: true}, chatResultJSON{Status: "error", Error: msg}, nil
}

func sessionsErr(msg string) (*sdk.CallToolResult, sessionsListResult, error) {
	return &sdk.CallToolResult{IsError: true}, sessionsListResult{Status: "error", Error: msg}, nil
}

func defaultAgentType(t string) string {
	if t == "" {
		return string(driver.AgentTypeCodex)
	}
	return t
}

// mapOutcome maps a simplified "allow"/"deny" to the matching ACP
// permission option's option_id. It inspects the options' Kind fields:
//   - allow → first option whose kind starts with "allow"
//   - deny  → first option whose kind starts with "reject"
//
// If no match is found, a cancelled outcome is returned.
func mapOutcome(outcome string, options []acp.PermissionOption) acp.RequestPermissionOutcome {
	var prefix string
	switch outcome {
	case "allow":
		prefix = "allow"
	case "deny":
		prefix = "reject"
	default:
		return acp.NewRequestPermissionOutcomeCancelled()
	}
	for _, opt := range options {
		if strings.HasPrefix(string(opt.Kind), prefix) {
			return acp.NewRequestPermissionOutcomeSelected(opt.OptionId)
		}
	}
	return acp.NewRequestPermissionOutcomeCancelled()
}

// ---------------------------------------------------------------------------
// ACP client management
// ---------------------------------------------------------------------------

func (s *Server) getOrCreateClient(ctx context.Context, agentType string) (acpClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cl, ok := s.clients[agentType]; ok {
		return cl, nil
	}
	cl, err := s.clientFactory(ctx, agentType)
	if err != nil {
		return nil, err
	}
	s.clients[agentType] = cl
	return cl, nil
}

func (s *Server) getClientBySession(sess *session.Session) (acpClient, error) {
	return s.getOrCreateClient(context.Background(), sess.AgentType)
}

// ---------------------------------------------------------------------------
// Prompt turn management
// ---------------------------------------------------------------------------

func (s *Server) storeTurn(sid session.SessionID, t *promptTurn) {
	s.mu.Lock()
	s.turns[sid] = t
	s.mu.Unlock()
}

func (s *Server) popTurn(sid session.SessionID) *promptTurn {
	s.mu.Lock()
	t := s.turns[sid]
	delete(s.turns, sid)
	s.mu.Unlock()
	return t
}

// peekTurn returns the in-flight prompt turn without removing it.
func (s *Server) peekTurn(sid session.SessionID) *promptTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns[sid]
}

func (t *promptTurn) cachedResult() (*sdk.CallToolResult, chatResultJSON, error, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.terminal {
		return nil, chatResultJSON{}, nil, false
	}
	return &sdk.CallToolResult{IsError: t.finalIsError}, t.finalResult, nil, true
}

// ---------------------------------------------------------------------------
// Core prompt flow: acp_chat + acp_respond
// ---------------------------------------------------------------------------

func (s *Server) handleAcpChat(ctx context.Context, _ *sdk.CallToolRequest, args acpChatArgs) (*sdk.CallToolResult, chatResultJSON, error) {
	var (
		sess *session.Session
		cl   acpClient
		err  error
	)
	if args.SessionID != "" {
		sess, err = s.pool.Get(session.SessionID(args.SessionID))
		if err != nil {
			return chatErr(fmt.Sprintf("session not found: %s", args.SessionID))
		}
		// State check: must be idle to start a new prompt.
		if !sess.CanChat() {
			if sess.State == session.StatePermissionPending {
				return chatErr("waiting for permission response, use acp_respond instead")
			}
			return chatErr("session busy (prompt in progress)")
		}
		cl, err = s.getClientBySession(sess)
		if err != nil {
			return chatErr(fmt.Sprintf("agent client unavailable: %v", err))
		}
	} else {
		agentType := defaultAgentType(args.AgentType)
		cl, err = s.getOrCreateClient(ctx, agentType)
		if err != nil {
			return chatErr(fmt.Sprintf("failed to start %s agent: %v", agentType, err))
		}

		cwd := args.CWD
		if cwd == "" {
			cwd = "."
		}
		resp, err := cl.NewSession(ctx, cwd)
		if err != nil {
			return chatErr(fmt.Sprintf("failed to create session: %v", err))
		}
		sess = &session.Session{
			ID:           session.SessionID(newSessionID()),
			AgentType:    agentType,
			CWD:          cwd,
			ACPSessionID: string(resp.SessionId),
			TurnCount:    0,
		}
		if err := s.pool.Add(sess); err != nil {
			return chatErr(fmt.Sprintf("failed to register session: %v", err))
		}
	}

	// Enter prompting state.
	s.pool.SetState(sess.ID, session.StatePrompting)

	blocks := []acp.ContentBlock{acp.TextBlock(args.Prompt)}
	return s.runPromptTurn(ctx, cl, sess, blocks, s.config.DefaultTimeout)
}

func (s *Server) handleAcpRespond(ctx context.Context, _ *sdk.CallToolRequest, args acpRespondArgs) (*sdk.CallToolResult, chatResultJSON, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return chatErr(fmt.Sprintf("session not found: %s", args.SessionID))
	}
	if !sess.CanRespond() {
		return chatErr("no pending permission for this session")
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return chatErr(fmt.Sprintf("agent client unavailable: %v", err))
	}

	turn := s.peekTurn(session.SessionID(args.SessionID))
	if turn == nil {
		return chatErr("no active prompt turn for this session")
	}
	turn.mu.Lock()
	if turn.permReq == nil {
		turn.mu.Unlock()
		return chatErr("no pending permission request in turn")
	}
	expectedRequestID := string(turn.permReq.ToolCall.ToolCallId)
	if args.RequestID != expectedRequestID {
		turn.mu.Unlock()
		return chatErr("permission request mismatch")
	}

	outcome := mapOutcome(args.Outcome, turn.permReq.Options)
	permResp := acp.RequestPermissionResponse{Outcome: outcome}
	if err := cl.RespondPermission(args.RequestID, permResp); err != nil {
		turn.mu.Unlock()
		return chatErr(fmt.Sprintf("respond failed: %v", err))
	}

	turn.permReq = nil
	turn.mu.Unlock()
	s.pool.SetState(sess.ID, session.StatePrompting)
	return s.waitForTurn(ctx, cl, sess, turn, s.config.DefaultTimeout)
}

// runPromptTurn starts a Prompt call in a background goroutine that
// survives handler return, then waits up to timeout for completion.
func (s *Server) runPromptTurn(ctx context.Context, cl acpClient, sess *session.Session, blocks []acp.ContentBlock, timeout time.Duration) (*sdk.CallToolResult, chatResultJSON, error) {
	promptCtx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	turn := &promptTurn{done: done}
	turn.turnID = newTurnID()
	turn.cancel = cancel
	s.storeTurn(sess.ID, turn)

	go func() {
		defer close(done)
		resp, err := cl.Prompt(promptCtx, sess.ACPSessionID, blocks)
		turn.mu.Lock()
		turn.result = resp
		turn.err = err
		turn.mu.Unlock()
	}()

	return s.waitForTurn(ctx, cl, sess, turn, timeout)
}

// waitForTurn selects on prompt completion, permission request, or timeout.
// On timeout the turn stays alive so acp_progress can finalise it later.
func (s *Server) waitForTurn(ctx context.Context, cl acpClient, sess *session.Session, turn *promptTurn, timeout time.Duration) (*sdk.CallToolResult, chatResultJSON, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-turn.done:
		return s.finalizeTurn(sess, cl, turn)

	case permReq := <-cl.PermissionSignal():
		s.pool.SetState(sess.ID, session.StatePermissionPending)
		turn.mu.Lock()
		turn.permReq = &permReq
		turn.mu.Unlock()
		return s.buildPermissionResult(sess, cl, turn, &permReq)

	case <-timer.C:
		return s.buildRunningResult(sess, cl, turn)

	case <-ctx.Done():
		s.popTurn(sess.ID)
		turn.cancel()
		s.pool.SetState(sess.ID, session.StateIdle)
		return chatErr("prompt cancelled")
	}
}

// finalizeTurn drains updates once and retains the completed snapshot for
// repeatable acp_progress queries.
func (s *Server) finalizeTurn(sess *session.Session, cl acpClient, turn *promptTurn) (*sdk.CallToolResult, chatResultJSON, error) {
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.terminal {
		return &sdk.CallToolResult{IsError: turn.finalIsError}, turn.finalResult, nil
	}

	if turn.err != nil {
		s.pool.SetState(sess.ID, session.StateIdle)
		turn.terminal = true
		turn.finalIsError = true
		turn.finalResult = chatResultJSON{
			Status:    "error",
			SessionID: string(sess.ID),
			Title:     sess.Title,
			TurnID:    turn.turnID,
			Error:     fmt.Sprintf("prompt failed: %v", turn.err),
		}
		return &sdk.CallToolResult{IsError: true}, turn.finalResult, nil
	}
	collector := newUpdateCollector()
	for _, notif := range cl.PopUpdates(sess.ACPSessionID) {
		collector.process(notif)
	}
	title := s.applyCollectorMetadata(sess, collector)
	s.pool.SetState(sess.ID, session.StateIdle)
	s.pool.Touch(sess.ID)
	_, out, _ := buildChatResult(sess, turn.result, collector, turn.turnID, title)
	turn.terminal = true
	turn.finalResult = out
	return &sdk.CallToolResult{}, out, nil
}

func (s *Server) applyCollectorMetadata(sess *session.Session, collector *updateCollector) string {
	if collector.sessionTitle != "" {
		s.pool.SetTitle(sess.ID, collector.sessionTitle)
	}
	if collector.currentMode != "" {
		s.pool.SetCurrentMode(sess.ID, collector.currentMode)
	}
	if len(collector.configOptions) > 0 {
		opts := make([]session.ConfigOptionInfo, len(collector.configOptions))
		for i, co := range collector.configOptions {
			opts[i] = session.ConfigOptionInfo{ID: co.ID, Name: co.Name, Type: co.Type, Value: co.Value}
		}
		s.pool.SetConfigOptions(sess.ID, opts)
	}
	if len(collector.availableCommands) > 0 {
		cmds := make([]session.AvailableCommandInfo, len(collector.availableCommands))
		for i, ac := range collector.availableCommands {
			cmds[i] = session.AvailableCommandInfo{Name: ac.Name, Description: ac.Description, InputHint: ac.InputHint}
		}
		s.pool.SetAvailableCommands(sess.ID, cmds)
	}
	if collector.sessionTitle != "" {
		return collector.sessionTitle
	}
	return sess.Title
}

// buildRunningResult returns a partial-progress snapshot for a still-running
// turn. Uses PeekUpdates (non-consuming) so the data remains for later.
func (s *Server) buildRunningResult(sess *session.Session, cl acpClient, turn *promptTurn) (*sdk.CallToolResult, chatResultJSON, error) {
	collector := newUpdateCollector()
	for _, notif := range cl.PeekUpdates(sess.ACPSessionID) {
		collector.process(notif)
	}
	title := s.applyCollectorMetadata(sess, collector)
	return &sdk.CallToolResult{}, chatResultJSON{
		Status:    "running",
		State:     string(sess.State),
		SessionID: string(sess.ID),
		Title:     title,
		TurnID:    turn.turnID,
		AgentText: collector.agentText,
		Reasoning: collector.reasoningText,
		ToolCalls: collector.toolCalls,
		Plan:      collector.planSteps,
	}, nil
}

// buildChatResult assembles the agent's response from session updates.
func buildChatResult(sess *session.Session, resp *acp.PromptResponse, collector *updateCollector, turnID, title string) (*sdk.CallToolResult, chatResultJSON, error) {
	return &sdk.CallToolResult{}, chatResultJSON{
		Status:      "completed",
		SessionID:   string(sess.ID),
		Title:       title,
		TurnID:      turnID,
		StopReason:  string(resp.StopReason),
		AgentText:   collector.agentText,
		Reasoning:   collector.reasoningText,
		ToolCalls:   collector.toolCalls,
		Plan:        collector.planSteps,
		FileChanges: collector.fileChanges,
		TurnCount:   sess.TurnCount,
		IsNew:       sess.TurnCount <= 1,
		Usage:       collector.usage,
	}, nil
}

// buildPermissionResult formats a permission request for the MCP client.
func (s *Server) buildPermissionResult(sess *session.Session, cl acpClient, turn *promptTurn, req *acp.RequestPermissionRequest) (*sdk.CallToolResult, chatResultJSON, error) {
	opts := make([]permissionOption, len(req.Options))
	for i, o := range req.Options {
		opts[i] = permissionOption{
			ID:   string(o.OptionId),
			Name: o.Name,
			Kind: string(o.Kind),
		}
	}

	perm := &permissionInfo{
		ToolCallID: string(req.ToolCall.ToolCallId),
		Options:    opts,
	}
	if req.ToolCall.Title != nil {
		perm.Title = *req.ToolCall.Title
	}
	if req.ToolCall.Kind != nil {
		perm.Kind = string(*req.ToolCall.Kind)
	}

	collector := newUpdateCollector()
	for _, notif := range cl.PeekUpdates(sess.ACPSessionID) {
		collector.process(notif)
	}
	title := s.applyCollectorMetadata(sess, collector)
	return &sdk.CallToolResult{}, chatResultJSON{
		Status:     "permission_required",
		SessionID:  string(sess.ID),
		Title:      title,
		TurnID:     turn.turnID,
		RequestID:  string(req.ToolCall.ToolCallId),
		Permission: perm,
	}, nil
}

// ---------------------------------------------------------------------------
// Simple handlers
// ---------------------------------------------------------------------------

func (s *Server) handleAcpInterrupt(ctx context.Context, _ *sdk.CallToolRequest, args acpTurnArgs) (*sdk.CallToolResult, chatResultJSON, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return chatErr(fmt.Sprintf("session not found: %s", args.SessionID))
	}
	if args.TurnID == "" {
		return chatErr("turn_id is required")
	}

	turn := s.peekTurn(sess.ID)
	if turn == nil {
		return chatErr("turn not found")
	}
	if turn.turnID != args.TurnID {
		return chatErr("turn mismatch")
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return chatErr(fmt.Sprintf("agent client unavailable: %v", err))
	}

	turn.mu.Lock()
	if turn.terminal {
		turn.mu.Unlock()
		return chatErr("turn is not interruptible")
	}
	select {
	case <-turn.done:
		turn.mu.Unlock()
		if _, _, finalizeErr := s.finalizeTurn(sess, cl, turn); finalizeErr != nil {
			return chatErr(fmt.Sprintf("finalize turn: %v", finalizeErr))
		}
		return chatErr("turn is not interruptible")
	default:
	}

	if err := cl.Cancel(ctx, sess.ACPSessionID); err != nil {
		turn.mu.Unlock()
		return chatErr(fmt.Sprintf("interrupt failed: %v", err))
	}
	turn.cancel()

	collector := newUpdateCollector()
	for _, notif := range cl.PopUpdates(sess.ACPSessionID) {
		collector.process(notif)
	}
	title := s.applyCollectorMetadata(sess, collector)
	s.pool.SetState(sess.ID, session.StateIdle)
	turn.permReq = nil
	turn.terminal = true
	turn.finalResult = chatResultJSON{
		Status:      "interrupted",
		SessionID:   string(sess.ID),
		Title:       title,
		State:       string(session.StateIdle),
		TurnID:      turn.turnID,
		AgentText:   collector.agentText,
		Reasoning:   collector.reasoningText,
		ToolCalls:   collector.toolCalls,
		Plan:        collector.planSteps,
		FileChanges: collector.fileChanges,
		TurnCount:   sess.TurnCount,
		Usage:       collector.usage,
	}
	out := turn.finalResult
	turn.mu.Unlock()
	return &sdk.CallToolResult{}, out, nil
}

func (s *Server) handleAcpClose(ctx context.Context, _ *sdk.CallToolRequest, args acpSessionIDArgs) (*sdk.CallToolResult, toolResult, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return failResult(fmt.Sprintf("session not found: %s", args.SessionID))
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return failResult(fmt.Sprintf("agent client unavailable: %v", err))
	}

	if _, err := cl.CloseSession(ctx, sess.ACPSessionID); err != nil {
		return failResult(fmt.Sprintf("close failed: %v", err))
	}

	if turn := s.popTurn(session.SessionID(args.SessionID)); turn != nil {
		turn.cancel()
	}
	s.pool.Remove(sess.ID)
	return okSessionResult(sess)
}

func (s *Server) handleAcpSessions(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, sessionsListResult, error) {
	summaries := s.pool.List()
	items := make([]sessionListItem, len(summaries))
	for i, sum := range summaries {
		items[i] = sessionListItem{
			ID:          string(sum.ID),
			AgentType:   sum.AgentType,
			State:       string(sum.State),
			Status:      string(sum.Status),
			TurnCount:   sum.TurnCount,
			IdleSeconds: sum.IdleSeconds,
			Cwd:         sum.CWD,
			Title:       sum.Title,
			CurrentMode: sum.CurrentMode,
		}
	}
	return &sdk.CallToolResult{}, sessionsListResult{Status: "ok", Sessions: items}, nil
}

// handleAcpProgress checks the state of an in-flight or recently-completed
// turn. Returns "completed" with the full result, "permission_required"
// if the agent needs authorisation, or "running" with partial progress.
func (s *Server) handleAcpProgress(_ context.Context, _ *sdk.CallToolRequest, args acpProgressArgs) (*sdk.CallToolResult, chatResultJSON, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return chatErr(fmt.Sprintf("session not found: %s", args.SessionID))
	}
	if args.TurnID == "" {
		return chatErr("turn_id is required")
	}

	turn := s.peekTurn(sess.ID)
	if turn == nil {
		return chatErr("turn not found")
	}
	if turn.turnID != args.TurnID {
		return chatErr("turn mismatch")
	}
	if result, out, err, ok := turn.cachedResult(); ok {
		return result, out, err
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return chatErr(fmt.Sprintf("agent client unavailable: %v", err))
	}

	// Turn completed since acp_chat returned?
	select {
	case <-turn.done:
		return s.finalizeTurn(sess, cl, turn)
	default:
	}

	// Stored permission request from a prior waitForTurn?
	turn.mu.Lock()
	permReq := turn.permReq
	turn.mu.Unlock()
	if permReq != nil {
		return s.buildPermissionResult(sess, cl, turn, permReq)
	}

	// New permission request arrived?
	select {
	case permReq := <-cl.PermissionSignal():
		s.pool.SetState(sess.ID, session.StatePermissionPending)
		turn.mu.Lock()
		turn.permReq = &permReq
		turn.mu.Unlock()
		return s.buildPermissionResult(sess, cl, turn, &permReq)
	default:
	}

	// Still running
	return s.buildRunningResult(sess, cl, turn)
}

// handleAcpSessionInfo returns session-level metadata: config options,
// available commands, current mode, title, etc. This data persists across
// turns — call this to inspect what the agent reported, not turn progress.
func (s *Server) handleAcpSessionInfo(_ context.Context, _ *sdk.CallToolRequest, args acpSessionIDArgs) (*sdk.CallToolResult, sessionInfoResult, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return &sdk.CallToolResult{IsError: true}, sessionInfoResult{Status: "error", Error: fmt.Sprintf("session not found: %s", args.SessionID)}, nil
	}

	configOpts := make([]configOptionSummary, len(sess.ConfigOpts))
	for i, co := range sess.ConfigOpts {
		configOpts[i] = configOptionSummary{ID: co.ID, Name: co.Name, Type: co.Type, Value: co.Value}
	}
	availCmds := make([]availableCommandSummary, len(sess.AvailCmds))
	for i, ac := range sess.AvailCmds {
		availCmds[i] = availableCommandSummary{Name: ac.Name, Description: ac.Description, InputHint: ac.InputHint}
	}

	return &sdk.CallToolResult{}, sessionInfoResult{
		Status:            "ok",
		SessionID:         string(sess.ID),
		AgentType:         sess.AgentType,
		State:             string(sess.State),
		Title:             sess.Title,
		Cwd:               sess.CWD,
		CurrentMode:       sess.CurrentMode,
		TurnCount:         sess.TurnCount,
		ConfigOptions:     configOpts,
		AvailableCommands: availCmds,
	}, nil
}

func (s *Server) handleAcpSetMode(ctx context.Context, _ *sdk.CallToolRequest, args acpSetModeArgs) (*sdk.CallToolResult, toolResult, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return failResult(fmt.Sprintf("session not found: %s", args.SessionID))
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return failResult(fmt.Sprintf("agent client unavailable: %v", err))
	}

	if _, err := cl.SetSessionMode(ctx, sess.ACPSessionID, args.Mode); err != nil {
		return failResult(fmt.Sprintf("set mode failed: %v", err))
	}

	return okSessionResult(sess)
}

func (s *Server) handleAcpSetConfig(ctx context.Context, _ *sdk.CallToolRequest, args acpSetConfigArgs) (*sdk.CallToolResult, toolResult, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return failResult(fmt.Sprintf("session not found: %s", args.SessionID))
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return failResult(fmt.Sprintf("agent client unavailable: %v", err))
	}

	if err := cl.SetSessionConfigOption(ctx, sess.ACPSessionID, args.ConfigID, args.Value); err != nil {
		return failResult(fmt.Sprintf("set config failed: %v", err))
	}

	return okSessionResult(sess)
}

func (s *Server) handleAcpForkSession(ctx context.Context, _ *sdk.CallToolRequest, args acpSessionIDArgs) (*sdk.CallToolResult, toolResult, error) {
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return failResult(fmt.Sprintf("session not found: %s", args.SessionID))
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return failResult(fmt.Sprintf("agent client unavailable: %v", err))
	}

	resp, err := cl.ForkSession(ctx, sess.ACPSessionID)
	if err != nil {
		return failResult(fmt.Sprintf("fork failed: %v", err))
	}

	forked := &session.Session{
		ID:           session.SessionID(newSessionID()),
		AgentType:    sess.AgentType,
		CWD:          sess.CWD,
		ACPSessionID: string(resp.SessionId),
		Title:        sess.Title,
	}
	if err := s.pool.Add(forked); err != nil {
		return failResult(fmt.Sprintf("register forked session: %v", err))
	}

	return okSessionResult(forked)
}

func (s *Server) handleAcpLoadSession(ctx context.Context, _ *sdk.CallToolRequest, args acpLoadSessionArgs) (*sdk.CallToolResult, toolResult, error) {
	agentType := defaultAgentType(args.AgentType)

	cl, err := s.getOrCreateClient(ctx, agentType)
	if err != nil {
		return failResult(fmt.Sprintf("failed to start %s agent: %v", agentType, err))
	}

	cwd := args.CWD
	if cwd == "" {
		cwd = "."
	}

	if _, err := cl.LoadSession(ctx, args.SessionID); err != nil {
		return failResult(fmt.Sprintf("load session failed: %v", err))
	}

	sess := &session.Session{
		ID:           session.SessionID(newSessionID()),
		AgentType:    agentType,
		CWD:          cwd,
		ACPSessionID: args.SessionID,
	}
	if err := s.pool.Add(sess); err != nil {
		return failResult(fmt.Sprintf("register session: %v", err))
	}

	return okSessionResult(sess)
}

func (s *Server) handleAcpListHistory(ctx context.Context, _ *sdk.CallToolRequest, args acpListHistoryArgs) (*sdk.CallToolResult, sessionsListResult, error) {
	cl, err := s.getOrCreateClient(ctx, string(driver.AgentTypeCodex))
	if err != nil {
		return sessionsErr(fmt.Sprintf("failed to start agent: %v", err))
	}

	resp, err := cl.ListSessions(ctx)
	if err != nil {
		return sessionsErr(fmt.Sprintf("list sessions failed: %v", err))
	}

	items := make([]sessionListItem, len(resp.Sessions))
	for i, si := range resp.Sessions {
		item := sessionListItem{
			ID:  string(si.SessionId),
			Cwd: si.Cwd,
		}
		if si.Title != nil {
			item.Title = *si.Title
		}
		items[i] = item
	}

	return &sdk.CallToolResult{}, sessionsListResult{Status: "ok", Sessions: items}, nil
}

func (s *Server) handleAcpResumeSession(ctx context.Context, _ *sdk.CallToolRequest, args acpSessionIDArgs) (*sdk.CallToolResult, toolResult, error) {
	// Resume operates on an existing bridge session — look it up first.
	sess, err := s.pool.Get(session.SessionID(args.SessionID))
	if err != nil {
		return failResult(fmt.Sprintf("session not found: %s", args.SessionID))
	}

	cl, err := s.getClientBySession(sess)
	if err != nil {
		return failResult(fmt.Sprintf("agent client unavailable: %v", err))
	}

	resp, err := cl.ResumeSession(ctx, sess.ACPSessionID)
	if err != nil {
		return failResult(fmt.Sprintf("resume session failed: %v", err))
	}

	// Create a new bridge session for the resumed ACP session.
	// ResumeSessionResponse does not return a new session ID — the
	// resumed session keeps its original ACP session ID.
	_ = resp
	resumed := &session.Session{
		ID:           session.SessionID(newSessionID()),
		AgentType:    sess.AgentType,
		CWD:          sess.CWD,
		ACPSessionID: sess.ACPSessionID,
		Title:        sess.Title,
	}
	if err := s.pool.Add(resumed); err != nil {
		return failResult(fmt.Sprintf("register resumed session: %v", err))
	}

	return okSessionResult(resumed)
}

func (s *Server) handleAcpDeleteSession(ctx context.Context, _ *sdk.CallToolRequest, args acpDeleteSessionArgs) (*sdk.CallToolResult, toolResult, error) {
	agentType := defaultAgentType(args.AgentType)

	cl, err := s.getOrCreateClient(ctx, agentType)
	if err != nil {
		return failResult(fmt.Sprintf("failed to start %s agent: %v", agentType, err))
	}

	if _, err := cl.DeleteSession(ctx, args.SessionID); err != nil {
		return failResult(fmt.Sprintf("delete session failed: %v", err))
	}

	return okResult(args.SessionID)
}

// ---------------------------------------------------------------------------
// Session ID generation
// ---------------------------------------------------------------------------

var sessionCounter struct {
	sync.Mutex
	n uint64
}

func newSessionID() string {
	sessionCounter.Lock()
	sessionCounter.n++
	sessionCounter.Unlock()
	return fmt.Sprintf("s-%d-%d", time.Now().UnixNano(), sessionCounter.n)
}

var turnCounter struct {
	sync.Mutex
	n uint64
}

func newTurnID() string {
	turnCounter.Lock()
	turnCounter.n++
	turnCounter.Unlock()
	return fmt.Sprintf("t-%d-%d", time.Now().UnixNano(), turnCounter.n)
}

// defaultClientFactory creates a real *client.Client via driver + client.New.
func defaultClientFactory(cfg *config.Config) clientFactory {
	return func(ctx context.Context, agentType string) (acpClient, error) {
		drv, err := driver.NewDriver(driver.AgentType(agentType), cfg)
		if err != nil {
			return nil, err
		}
		return client.New(ctx, drv)
	}
}
