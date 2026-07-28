// Package client provides a high-level ACP client that manages an agent
// subprocess lifecycle and exposes the full ACP API surface.
package client

import (
	"context"
	"errors"
	"fmt"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// errNotSupported is returned for ACP Client interface methods that
// acp-bridge does not implement (terminal, filesystem, etc.).
var errNotSupported = errors.New("acp client method not supported by acp-bridge")

// acpClientHandler implements the acp.Client interface that the SDK
// dispatches incoming agent-to-client calls to.
//
// Permission requests use a channel-based synchronous wait pattern:
//  1. RequestPermission creates a channel, stores it by request ID, and blocks.
//  2. The user (or a higher-level caller) injects a decision via Respond.
//  3. The channel unblocks and RequestPermission returns.
//
// Session updates are accumulated per session ID and can be consumed
// after a prompt turn completes.
type acpClientHandler struct {
	mu sync.Mutex

	// permissionCh maps permission request IDs to channels that
	// RequestPermission blocks on. The channel receives exactly one
	// response (or is closed on Close).
	permissionCh map[string]chan acp.RequestPermissionResponse

	// permissionSignal broadcasts each incoming permission request to
	// the goroutine running the prompt turn. The MCP handler selects
	// on this channel to surface the request to the user.
	permissionSignal chan acp.RequestPermissionRequest

	// sessionUpdates accumulates SessionNotifications per session ID.
	// A call to PopUpdates drains the queue for a given session.
	sessionUpdates map[string][]acp.SessionNotification

	// closed is set to true when the handler is shut down. Pending
	// permission channels are closed to unblock waiters.
	closed bool
}

// compile-time interface check
var _ acp.Client = (*acpClientHandler)(nil)

// Elicitation support — the SDK dispatches via type assertion to
// ClientExperimental, so implementing these methods is sufficient.
var _ acp.ClientExperimental = (*acpClientHandler)(nil)

func newHandler() *acpClientHandler {
	return &acpClientHandler{
		permissionCh:     make(map[string]chan acp.RequestPermissionResponse),
		permissionSignal: make(chan acp.RequestPermissionRequest, 8),
		sessionUpdates:   make(map[string][]acp.SessionNotification),
	}
}

// ---------------------------------------------------------------------------
// acp.Client – required interface methods
// ---------------------------------------------------------------------------

// RequestPermission blocks until a response is injected via Respond.
// The requestID is derived from the tool-call ID (or prompt context).
func (h *acpClientHandler) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	id := permissionRequestID(&params)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return acp.RequestPermissionResponse{}, errors.New("handler closed")
	}
	ch := make(chan acp.RequestPermissionResponse, 1)
	h.permissionCh[id] = ch
	h.mu.Unlock()

	// Surface the request to the prompt-turn goroutine. Non-blocking:
	// if no one is selecting yet the request is still tracked in
	// permissionCh and can be resolved via Respond.
	select {
	case h.permissionSignal <- params:
	default:
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return acp.RequestPermissionResponse{}, errors.New("permission request cancelled: handler closed")
		}
		return resp, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.permissionCh, id)
		h.mu.Unlock()
		return acp.RequestPermissionResponse{}, ctx.Err()
	}
}

// SessionUpdate accumulates streaming notifications from the agent.
func (h *acpClientHandler) SessionUpdate(ctx context.Context, notif acp.SessionNotification) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return errors.New("handler closed")
	}
	sid := string(notif.SessionId)
	h.sessionUpdates[sid] = append(h.sessionUpdates[sid], notif)
	return nil
}

// ReadTextFile returns errNotSupported.
func (h *acpClientHandler) ReadTextFile(_ context.Context, _ acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errNotSupported
}

// WriteTextFile returns errNotSupported.
func (h *acpClientHandler) WriteTextFile(_ context.Context, _ acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errNotSupported
}

// CreateTerminal returns errNotSupported.
func (h *acpClientHandler) CreateTerminal(_ context.Context, _ acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNotSupported
}

// KillTerminal returns errNotSupported.
func (h *acpClientHandler) KillTerminal(_ context.Context, _ acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNotSupported
}

// TerminalOutput returns errNotSupported.
func (h *acpClientHandler) TerminalOutput(_ context.Context, _ acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNotSupported
}

// ReleaseTerminal returns errNotSupported.
func (h *acpClientHandler) ReleaseTerminal(_ context.Context, _ acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNotSupported
}

// WaitForTerminalExit returns errNotSupported.
func (h *acpClientHandler) WaitForTerminalExit(_ context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNotSupported
}

// ---------------------------------------------------------------------------
// Response injection
// ---------------------------------------------------------------------------

// Respond injects a permission response for the given requestID, unblocking
// the corresponding RequestPermission call. Returns an error if no pending
// request with that ID exists.
func (h *acpClientHandler) Respond(requestID string, resp acp.RequestPermissionResponse) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch, ok := h.permissionCh[requestID]
	if !ok {
		return fmt.Errorf("no pending permission request with ID %q", requestID)
	}
	delete(h.permissionCh, requestID)
	ch <- resp
	return nil
}

// PopUpdates drains and returns all buffered SessionNotifications for the
// given session ID.
func (h *acpClientHandler) PopUpdates(sessionID string) []acp.SessionNotification {
	h.mu.Lock()
	defer h.mu.Unlock()

	updates := h.sessionUpdates[sessionID]
	delete(h.sessionUpdates, sessionID)
	return updates
}

// PeekUpdates returns a snapshot of buffered SessionNotifications for
// the given session ID without draining them. Used by acp_progress so
// users can observe real-time progress while a prompt is in flight.
func (h *acpClientHandler) PeekUpdates(sessionID string) []acp.SessionNotification {
	h.mu.Lock()
	defer h.mu.Unlock()

	src := h.sessionUpdates[sessionID]
	out := make([]acp.SessionNotification, len(src))
	copy(out, src)
	return out
}

// PermissionSignal returns the channel that receives each incoming
// permission request. Consumers select on it during a prompt turn.
func (h *acpClientHandler) PermissionSignal() <-chan acp.RequestPermissionRequest {
	return h.permissionSignal
}

// close unblocks all pending permission waiters and marks the handler as
// shut down.
func (h *acpClientHandler) close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.closed = true
	for id, ch := range h.permissionCh {
		close(ch)
		delete(h.permissionCh, id)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// permissionRequestID constructs a stable key for a permission request.
func permissionRequestID(req *acp.RequestPermissionRequest) string {
	// Use the tool-call ID as the canonical key; fall back to session ID
	// if no tool-call is present.
	if req.ToolCall.ToolCallId != "" {
		return string(req.ToolCall.ToolCallId)
	}
	return fmt.Sprintf("session:%s", string(req.SessionId))
}

// ---------------------------------------------------------------------------
// Elicitation (ClientExperimental)
// ---------------------------------------------------------------------------

// UnstableCreateElicitation routes an elicitation request through the same
// permission signal channel. The MCP handler surfaces it as a
// permission_required result so the user can respond via acp_respond.
func (h *acpClientHandler) UnstableCreateElicitation(_ context.Context, params acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	title := "Elicitation"
	if params.Form != nil {
		title = params.Form.Message
	}

	// Build a synthetic permission request and broadcast it.
	permReq := acp.RequestPermissionRequest{
		SessionId: "",
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "elicitation",
			Title:      &title,
			Kind:       acp.Ptr(acp.ToolKindOther),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
		},
		Options: []acp.PermissionOption{
			{OptionId: "accept", Name: "Accept", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "decline", Name: "Decline", Kind: acp.PermissionOptionKindRejectOnce},
		},
	}

	select {
	case h.permissionSignal <- permReq:
	default:
	}

	// Block until a response arrives via Respond (same mechanism as permission).
	id := "elicitation"
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return acp.UnstableCreateElicitationResponse{}, errors.New("handler closed")
	}
	ch := make(chan acp.RequestPermissionResponse, 1)
	h.permissionCh[id] = ch
	h.mu.Unlock()

	resp := <-ch

	// Map the permission outcome back to an elicitation response.
	if resp.Outcome.Selected != nil {
		return acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Action: "accept"},
		}, nil
	}
	return acp.UnstableCreateElicitationResponse{
		Decline: &acp.UnstableCreateElicitationDecline{Action: "decline"},
	}, nil
}

// UnstableCompleteElicitation is a no-op — acp-bridge does not need to
// track elicitation lifecycle beyond the initial request.
func (h *acpClientHandler) UnstableCompleteElicitation(_ context.Context, _ acp.UnstableCompleteElicitationNotification) error {
	return nil
}

// UnstableConnectMcp is not supported by acp-bridge.
func (h *acpClientHandler) UnstableConnectMcp(_ context.Context, _ acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error) {
	return acp.UnstableConnectMcpResponse{}, errNotSupported
}

// UnstableDisconnectMcp is not supported by acp-bridge.
func (h *acpClientHandler) UnstableDisconnectMcp(_ context.Context, _ acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error) {
	return acp.UnstableDisconnectMcpResponse{}, errNotSupported
}
