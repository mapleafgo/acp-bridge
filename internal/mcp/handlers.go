package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/mapleafgo/acp-bridge/internal/driver"
	"github.com/mapleafgo/acp-bridge/internal/instance"
	"github.com/mapleafgo/acp-bridge/internal/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func okResult(sessionID string) (*sdk.CallToolResult, toolResult, error) {
	return &sdk.CallToolResult{}, toolResult{Status: "ok", SessionID: sessionID}, nil
}

func failResult(message string) (*sdk.CallToolResult, toolResult, error) {
	return &sdk.CallToolResult{IsError: true}, toolResult{Status: "error", Error: message}, nil
}

func chatErr(message string) (*sdk.CallToolResult, chatResultJSON, error) {
	return &sdk.CallToolResult{IsError: true}, chatResultJSON{Status: "error", Error: message}, nil
}

func sessionsErr(message string) (*sdk.CallToolResult, sessionsListResult, error) {
	return &sdk.CallToolResult{IsError: true}, sessionsListResult{Status: "error", Error: message}, nil
}

func defaultAgentType(raw string) (driver.AgentType, error) {
	if raw == "" {
		return driver.AgentTypeCodex, nil
	}
	agentType := driver.AgentType(raw)
	switch agentType {
	case driver.AgentTypeCodex, driver.AgentTypeClaude, driver.AgentTypeGemini, driver.AgentTypeOpenCode:
		return agentType, nil
	default:
		return "", fmt.Errorf("unknown agent type: %s", raw)
	}
}

func validateSessionID(raw string) error {
	if raw == "" {
		return fmt.Errorf("session_id is required")
	}
	_, err := session.ParseID(raw)
	return err
}

func (s *Server) handleAcpChat(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpChatArgs,
) (*sdk.CallToolResult, chatResultJSON, error) {
	if args.Prompt == "" {
		return chatErr("prompt is required")
	}

	sessionID := args.SessionID
	isNew := false
	if sessionID == "" {
		agentType, err := defaultAgentType(args.AgentType)
		if err != nil {
			return chatErr(err.Error())
		}
		cwd := args.CWD
		if cwd == "" {
			cwd = "."
		}
		created, err := s.manager.CreateSession(ctx, agentType, cwd)
		if err != nil {
			return chatErr(fmt.Sprintf("failed to create session: %v", err))
		}
		sessionID = created.ID.String()
		isNew = true
		if err := ctx.Err(); err != nil {
			return chatErr(err.Error())
		}
	} else if err := validateSessionID(sessionID); err != nil {
		return chatErr(err.Error())
	}

	view, err := s.manager.Chat(ctx, sessionID, args.Prompt, s.config.DefaultTimeout)
	if err != nil {
		return chatErr(err.Error())
	}
	out := chatResult(view)
	out.IsNew = isNew
	return &sdk.CallToolResult{IsError: view.Status == instance.StatusError}, out, nil
}

func (s *Server) handleAcpRespond(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpRespondArgs,
) (*sdk.CallToolResult, chatResultJSON, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return chatErr(err.Error())
	}
	if args.RequestID == "" {
		return chatErr("request_id is required")
	}
	if args.Outcome != "allow" && args.Outcome != "deny" {
		return chatErr("outcome must be allow or deny")
	}
	view, err := s.manager.Respond(ctx, args.SessionID, args.RequestID, args.Outcome, s.config.DefaultTimeout)
	if err != nil {
		return chatErr(err.Error())
	}
	out := chatResult(view)
	return &sdk.CallToolResult{IsError: view.Status == instance.StatusError}, out, nil
}

func (s *Server) handleAcpInterrupt(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpTurnArgs,
) (*sdk.CallToolResult, chatResultJSON, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return chatErr(err.Error())
	}
	if args.TurnID == "" {
		return chatErr("turn_id is required")
	}
	view, err := s.manager.Interrupt(ctx, args.SessionID, args.TurnID)
	if err != nil {
		return chatErr(err.Error())
	}
	return &sdk.CallToolResult{}, chatResult(view), nil
}

func (s *Server) handleAcpProgress(
	_ context.Context,
	_ *sdk.CallToolRequest,
	args acpProgressArgs,
) (*sdk.CallToolResult, chatResultJSON, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return chatErr(err.Error())
	}
	view, err := s.manager.Progress(args.SessionID, args.TurnID)
	if err != nil {
		return chatErr(err.Error())
	}
	out := chatResult(view)
	return &sdk.CallToolResult{IsError: view.Status == instance.StatusError}, out, nil
}

func (s *Server) handleAcpClose(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpSessionIDArgs,
) (*sdk.CallToolResult, toolResult, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return failResult(err.Error())
	}
	if err := s.manager.CloseSession(ctx, args.SessionID); err != nil {
		return failResult(err.Error())
	}
	return okResult(args.SessionID)
}

func (s *Server) handleAcpSessions(
	_ context.Context,
	_ *sdk.CallToolRequest,
	_ struct{},
) (*sdk.CallToolResult, sessionsListResult, error) {
	views := s.manager.Sessions()
	items := make([]sessionListItem, 0, len(views))
	now := time.Now()
	for _, view := range views {
		turnStatus := ""
		if view.Turn != nil {
			turnStatus = string(view.Turn.State)
		}
		items = append(items, sessionListItem{
			SessionID:   view.ID.String(),
			AgentType:   string(view.ID.AgentType),
			State:       string(view.State),
			Status:      "active",
			TurnStatus:  turnStatus,
			TurnCount:   view.TurnCount,
			IdleSeconds: max(0, int(now.Sub(view.LastUsed).Seconds())),
			Title:       view.Title,
			Cwd:         view.CWD,
			CurrentMode: view.CurrentMode,
		})
	}
	return &sdk.CallToolResult{}, sessionsListResult{Status: "ok", Sessions: items}, nil
}

func (s *Server) handleAcpSessionInfo(
	_ context.Context,
	_ *sdk.CallToolRequest,
	args acpSessionIDArgs,
) (*sdk.CallToolResult, sessionInfoResult, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return &sdk.CallToolResult{IsError: true}, sessionInfoResult{Status: "error", Error: err.Error()}, nil
	}
	view, err := s.manager.Session(args.SessionID)
	if err != nil {
		return &sdk.CallToolResult{IsError: true}, sessionInfoResult{Status: "error", Error: err.Error()}, nil
	}
	configOptions := make([]configOptionSummary, 0, len(view.ConfigOpts))
	for _, option := range view.ConfigOpts {
		configOptions = append(configOptions, configOptionSummary{
			ID: option.ID, Name: option.Name, Type: option.Type, Value: option.Value,
		})
	}
	commands := make([]availableCommandSummary, 0, len(view.AvailCmds))
	for _, command := range view.AvailCmds {
		commands = append(commands, availableCommandSummary{
			Name: command.Name, Description: command.Description, InputHint: command.InputHint,
		})
	}
	return &sdk.CallToolResult{}, sessionInfoResult{
		Status:            "ok",
		SessionID:         view.ID.String(),
		AgentType:         string(view.ID.AgentType),
		State:             string(view.State),
		Title:             view.Title,
		Cwd:               view.CWD,
		CurrentMode:       view.CurrentMode,
		TurnCount:         view.TurnCount,
		ConfigOptions:     configOptions,
		AvailableCommands: commands,
	}, nil
}

func (s *Server) handleAcpSetMode(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpSetModeArgs,
) (*sdk.CallToolResult, toolResult, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return failResult(err.Error())
	}
	view, err := s.manager.SetMode(ctx, args.SessionID, args.Mode)
	if err != nil {
		return failResult(err.Error())
	}
	return &sdk.CallToolResult{}, toolResult{Status: "ok", SessionID: view.ID.String(), Title: view.Title}, nil
}

func (s *Server) handleAcpSetConfig(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpSetConfigArgs,
) (*sdk.CallToolResult, toolResult, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return failResult(err.Error())
	}
	if err := s.manager.SetConfig(ctx, args.SessionID, args.ConfigID, args.Value); err != nil {
		return failResult(err.Error())
	}
	return okResult(args.SessionID)
}

func (s *Server) handleAcpForkSession(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpSessionIDArgs,
) (*sdk.CallToolResult, toolResult, error) {
	if err := validateSessionID(args.SessionID); err != nil {
		return failResult(err.Error())
	}
	view, err := s.manager.ForkSession(ctx, args.SessionID)
	if err != nil {
		return failResult(err.Error())
	}
	return &sdk.CallToolResult{}, toolResult{Status: "ok", SessionID: view.ID.String(), Title: view.Title}, nil
}

func (s *Server) handleAcpLoadSession(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpLoadSessionArgs,
) (*sdk.CallToolResult, toolResult, error) {
	id, err := session.ParseID(args.SessionID)
	if err != nil {
		return failResult(err.Error())
	}
	cwd := args.CWD
	if cwd == "" {
		cwd = "."
	}
	view, err := s.manager.LoadSession(ctx, id, cwd)
	if err != nil {
		return failResult(err.Error())
	}
	return &sdk.CallToolResult{}, toolResult{Status: "ok", SessionID: view.ID.String(), Title: view.Title}, nil
}

func (s *Server) handleAcpResumeSession(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpLoadSessionArgs,
) (*sdk.CallToolResult, toolResult, error) {
	id, err := session.ParseID(args.SessionID)
	if err != nil {
		return failResult(err.Error())
	}
	cwd := args.CWD
	if cwd == "" {
		cwd = "."
	}
	view, err := s.manager.ResumeSession(ctx, id, cwd)
	if err != nil {
		return failResult(err.Error())
	}
	return &sdk.CallToolResult{}, toolResult{Status: "ok", SessionID: view.ID.String(), Title: view.Title}, nil
}

func (s *Server) handleAcpDeleteSession(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpDeleteSessionArgs,
) (*sdk.CallToolResult, toolResult, error) {
	id, err := session.ParseID(args.SessionID)
	if err != nil {
		return failResult(err.Error())
	}
	if err := s.manager.DeleteSession(ctx, id); err != nil {
		return failResult(err.Error())
	}
	return okResult(args.SessionID)
}

func (s *Server) handleAcpListHistory(
	ctx context.Context,
	_ *sdk.CallToolRequest,
	args acpListHistoryArgs,
) (*sdk.CallToolResult, sessionsListResult, error) {
	agentType, err := defaultAgentType(args.AgentType)
	if err != nil {
		return sessionsErr(err.Error())
	}
	history, err := s.manager.History(ctx, agentType)
	if err != nil {
		return sessionsErr(err.Error())
	}
	items := make([]sessionListItem, 0, len(history))
	for _, item := range history {
		title := ""
		if item.Title != nil {
			title = *item.Title
		}
		items = append(items, sessionListItem{
			SessionID: string(agentType) + ":" + string(item.SessionId),
			AgentType: string(agentType),
			Status:    "persisted",
			Title:     title,
			Cwd:       item.Cwd,
		})
	}
	return &sdk.CallToolResult{}, sessionsListResult{Status: "ok", Sessions: items}, nil
}

func chatResult(view instance.ChatView) chatResultJSON {
	collector := newUpdateCollector()
	for _, notification := range view.Turn.Updates {
		collector.process(notification)
	}
	title := view.Title
	if collector.sessionTitle != "" {
		title = collector.sessionTitle
	}
	out := chatResultJSON{
		Status:      string(view.Status),
		SessionID:   view.SessionID,
		Title:       title,
		State:       string(view.State),
		TurnID:      view.Turn.ID,
		StopReason:  view.Turn.StopReason,
		AgentText:   collector.agentText,
		Reasoning:   collector.reasoningText,
		ToolCalls:   collector.toolCalls,
		Plan:        collector.planSteps,
		FileChanges: collector.fileChanges,
		TurnCount:   view.TurnCount,
		Usage:       collector.usage,
		Error:       view.Turn.Error,
	}
	if permission := view.Turn.Permission; permission != nil {
		options := make([]permissionOption, 0, len(permission.Options))
		for _, option := range permission.Options {
			options = append(options, permissionOption{
				ID: option.ID, Name: option.Name, Kind: option.Kind,
			})
		}
		out.RequestID = permission.RequestID
		out.Permission = &permissionInfo{
			ToolCallID: permission.ToolCallID,
			Title:      permission.Title,
			Kind:       permission.Kind,
			Options:    options,
		}
	}
	return out
}
