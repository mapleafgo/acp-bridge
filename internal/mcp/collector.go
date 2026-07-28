package mcp

import (
	"strconv"

	acp "github.com/coder/acp-go-sdk"
)

// updateCollector centralises parsing of SessionUpdate notifications.
// Both buildChatResult and handleAcpProgress share this logic to extract
// agent text, reasoning, tool-call lifecycle, plan, and usage data.
type updateCollector struct {
	agentText         string
	reasoningText     string
	toolCalls         []toolCallSummary
	toolCallIdx       map[string]int
	planSteps         []planStep
	fileChanges       []fileChangeSummary
	usage             *usageInfo
	sessionTitle      string
	currentMode       string
	userMessage       string
	configOptions     []configOptionSummary
	availableCommands []availableCommandSummary
}

func newUpdateCollector() *updateCollector {
	return &updateCollector{toolCallIdx: make(map[string]int)}
}

// process handles one session notification.
func (c *updateCollector) process(notif acp.SessionNotification) {
	u := notif.Update

	if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
		c.agentText += u.AgentMessageChunk.Content.Text.Text
	}
	if u.AgentThoughtChunk != nil && u.AgentThoughtChunk.Content.Text != nil {
		c.reasoningText += u.AgentThoughtChunk.Content.Text.Text
	}
	c.processToolCall(u.ToolCall)
	c.processToolCallUpdate(u.ToolCallUpdate)
	c.processPlan(u)
	c.processContent(u.ToolCall)
	c.processContentUpdate(u.ToolCallUpdate)
	if u.UserMessageChunk != nil && u.UserMessageChunk.Content.Text != nil {
		c.userMessage += u.UserMessageChunk.Content.Text.Text
	}
	c.processConfigOptions(u.ConfigOptionUpdate)
	c.processAvailableCommands(u.AvailableCommandsUpdate)
	if u.SessionInfoUpdate != nil && u.SessionInfoUpdate.Title != nil && *u.SessionInfoUpdate.Title != "" {
		c.sessionTitle = *u.SessionInfoUpdate.Title
	}
	if u.CurrentModeUpdate != nil {
		c.currentMode = string(u.CurrentModeUpdate.CurrentModeId)
	}
	if u.UsageUpdate != nil {
		ui := &usageInfo{
			UsedTokens:  u.UsageUpdate.Used,
			TotalTokens: u.UsageUpdate.Size,
		}
		if u.UsageUpdate.Cost != nil {
			ui.Cost = u.UsageUpdate.Cost.Amount
			ui.Currency = u.UsageUpdate.Cost.Currency
		}
		c.usage = ui
	}
}

// processToolCall creates or updates a tool-call summary from a new ToolCall.
func (c *updateCollector) processToolCall(tc *acp.SessionUpdateToolCall) {
	if tc == nil {
		return
	}
	summary := toolCallSummary{
		ID:        string(tc.ToolCallId),
		Title:     tc.Title,
		Kind:      string(tc.Kind),
		Status:    string(tc.Status),
		RawInput:  tc.RawInput,
		RawOutput: tc.RawOutput,
	}
	for _, loc := range tc.Locations {
		summary.Locations = append(summary.Locations, loc.Path)
	}
	c.addToolCall(summary)
}

// processToolCallUpdate merges a tool-call status update into an existing entry.
func (c *updateCollector) processToolCallUpdate(tcu *acp.SessionToolCallUpdate) {
	if tcu == nil {
		return
	}
	tc := toolCallSummary{ID: string(tcu.ToolCallId)}
	if tcu.Status != nil {
		tc.Status = string(*tcu.Status)
	}
	if tcu.Title != nil {
		tc.Title = *tcu.Title
	}
	if tcu.Kind != nil {
		tc.Kind = string(*tcu.Kind)
	}
	if tcu.RawInput != nil {
		tc.RawInput = tcu.RawInput
	}
	if tcu.RawOutput != nil {
		tc.RawOutput = tcu.RawOutput
	}
	c.addToolCall(tc)
}

// processPlan handles Plan, PlanUpdate (Items + Markdown), and PlanRemoved.
func (c *updateCollector) processPlan(u acp.SessionUpdate) {
	switch {
	case u.Plan != nil:
		c.planSteps = entriesToSteps(u.Plan.Entries)
	case u.PlanUpdate != nil:
		if u.PlanUpdate.Plan.Items != nil {
			c.planSteps = entriesToSteps(u.PlanUpdate.Plan.Items.Entries)
		}
		if u.PlanUpdate.Plan.Markdown != nil {
			c.planSteps = []planStep{{Content: u.PlanUpdate.Plan.Markdown.Content}}
		}
	case u.PlanRemoved != nil:
		c.planSteps = nil
	}
}

func entriesToSteps(entries []acp.PlanEntry) []planStep {
	steps := make([]planStep, len(entries))
	for i, e := range entries {
		steps[i] = planStep{
			Content:  e.Content,
			Status:   string(e.Status),
			Priority: string(e.Priority),
		}
	}
	return steps
}

// processConfigOptions handles select and boolean config option variants.
func (c *updateCollector) processConfigOptions(u *acp.SessionConfigOptionUpdate) {
	if u == nil {
		return
	}
	opts := make([]configOptionSummary, 0, len(u.ConfigOptions))
	for _, opt := range u.ConfigOptions {
		summary := configOptionSummary{}
		if opt.Select != nil {
			summary.ID = string(opt.Select.Id)
			summary.Name = opt.Select.Name
			summary.Type = "select"
			summary.Value = string(opt.Select.CurrentValue)
		} else if opt.Boolean != nil {
			summary.ID = string(opt.Boolean.Id)
			summary.Name = opt.Boolean.Name
			summary.Type = "boolean"
			summary.Value = strconv.FormatBool(opt.Boolean.CurrentValue)
		}
		opts = append(opts, summary)
	}
	c.configOptions = opts
}

// processAvailableCommands handles slash command updates.
func (c *updateCollector) processAvailableCommands(u *acp.SessionAvailableCommandsUpdate) {
	if u == nil {
		return
	}
	cmds := make([]availableCommandSummary, 0, len(u.AvailableCommands))
	for _, cmd := range u.AvailableCommands {
		acs := availableCommandSummary{
			Name:        cmd.Name,
			Description: cmd.Description,
		}
		if cmd.Input != nil && cmd.Input.Unstructured != nil {
			acs.InputHint = cmd.Input.Unstructured.Hint
		}
		cmds = append(cmds, acs)
	}
	c.availableCommands = cmds
}

// addToolCall inserts or merges a tool-call summary by ID, preserving
// first-seen order. Non-empty fields from tc overwrite existing values.
func (c *updateCollector) addToolCall(tc toolCallSummary) {
	if idx, ok := c.toolCallIdx[tc.ID]; ok {
		existing := &c.toolCalls[idx]
		if tc.Title != "" {
			existing.Title = tc.Title
		}
		if tc.Kind != "" {
			existing.Kind = tc.Kind
		}
		if tc.Status != "" {
			existing.Status = tc.Status
		}
		if len(tc.Locations) > 0 {
			existing.Locations = tc.Locations
		}
		if tc.RawInput != nil {
			existing.RawInput = tc.RawInput
		}
		if tc.RawOutput != nil {
			existing.RawOutput = tc.RawOutput
		}
		return
	}
	c.toolCallIdx[tc.ID] = len(c.toolCalls)
	c.toolCalls = append(c.toolCalls, tc)
}

// addFileChange records a file modification, deduplicating by path.
func (c *updateCollector) addFileChange(fc fileChangeSummary) {
	for i, existing := range c.fileChanges {
		if existing.Path == fc.Path {
			if fc.Kind != "" {
				c.fileChanges[i].Kind = fc.Kind
			}
			return
		}
	}
	c.fileChanges = append(c.fileChanges, fc)
}

// processContent extracts file-change diffs from a ToolCall's content.
func (c *updateCollector) processContent(tc *acp.SessionUpdateToolCall) {
	if tc == nil {
		return
	}
	for _, cnt := range tc.Content {
		if cnt.Diff != nil {
			kind := "modified"
			if cnt.Diff.OldText == nil {
				kind = "created"
			}
			c.addFileChange(fileChangeSummary{Path: cnt.Diff.Path, Kind: kind})
		}
	}
}

// processContentUpdate extracts file-change diffs from a ToolCallUpdate's content.
func (c *updateCollector) processContentUpdate(tcu *acp.SessionToolCallUpdate) {
	if tcu == nil {
		return
	}
	for _, cnt := range tcu.Content {
		if cnt.Diff != nil {
			kind := "modified"
			if cnt.Diff.OldText == nil {
				kind = "created"
			}
			c.addFileChange(fileChangeSummary{Path: cnt.Diff.Path, Kind: kind})
		}
	}
}
