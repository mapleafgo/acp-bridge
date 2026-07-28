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

	permissionCh     map[permissionKey]chan acp.RequestPermissionResponse
	permissionEvents map[string]chan PermissionEvent

	// sessionUpdates accumulates SessionNotifications per session ID.
	// A call to PopUpdates drains the queue for a given session.
	sessionUpdates map[string][]acp.SessionNotification

	// closed is set to true when the handler is shut down. Pending
	// permission channels are closed to unblock waiters.
	closed  bool
	closeCh chan struct{}
}

type permissionKey struct {
	sessionID string
	requestID string
}

// PermissionEvent 是按 agent Session 路由的权限请求。
type PermissionEvent struct {
	SessionID string
	RequestID string
	Request   acp.RequestPermissionRequest
}

// compile-time interface check
var _ acp.Client = (*acpClientHandler)(nil)

// Elicitation support — the SDK dispatches via type assertion to
// ClientExperimental, so implementing these methods is sufficient.
var _ acp.ClientExperimental = (*acpClientHandler)(nil)

func newHandler() *acpClientHandler {
	return &acpClientHandler{
		permissionCh:     make(map[permissionKey]chan acp.RequestPermissionResponse),
		permissionEvents: make(map[string]chan PermissionEvent),
		sessionUpdates:   make(map[string][]acp.SessionNotification),
		closeCh:          make(chan struct{}),
	}
}

// ---------------------------------------------------------------------------
// acp.Client – required interface methods
// ---------------------------------------------------------------------------

// RequestPermission blocks until a response is injected via Respond.
// The requestID is derived from the tool-call ID (or prompt context).
func (h *acpClientHandler) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	sessionID := string(params.SessionId)
	requestID := string(params.ToolCall.ToolCallId)
	if sessionID == "" {
		return acp.RequestPermissionResponse{}, errors.New("permission request has empty session ID")
	}
	if requestID == "" {
		return acp.RequestPermissionResponse{}, errors.New("permission request has empty tool call ID")
	}
	key := permissionKey{sessionID: sessionID, requestID: requestID}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return acp.RequestPermissionResponse{}, errors.New("handler closed")
	}
	if _, exists := h.permissionCh[key]; exists {
		h.mu.Unlock()
		return acp.RequestPermissionResponse{}, fmt.Errorf("duplicate permission request %q for session %q", requestID, sessionID)
	}
	ch := make(chan acp.RequestPermissionResponse, 1)
	h.permissionCh[key] = ch
	events := h.permissionEventsLocked(sessionID)
	h.mu.Unlock()

	select {
	case events <- PermissionEvent{SessionID: sessionID, RequestID: requestID, Request: params}:
	case <-ctx.Done():
		h.deletePermission(key)
		return acp.RequestPermissionResponse{}, ctx.Err()
	case <-h.closeCh:
		return acp.RequestPermissionResponse{}, errors.New("handler closed")
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return acp.RequestPermissionResponse{}, errors.New("permission request cancelled: handler closed")
		}
		return resp, nil
	case <-ctx.Done():
		h.deletePermission(key)
		return acp.RequestPermissionResponse{}, ctx.Err()
	case <-h.closeCh:
		return acp.RequestPermissionResponse{}, errors.New("handler closed")
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
func (h *acpClientHandler) Respond(sessionID, requestID string, resp acp.RequestPermissionResponse) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := permissionKey{sessionID: sessionID, requestID: requestID}
	ch, ok := h.permissionCh[key]
	if !ok {
		return fmt.Errorf("no pending permission request with ID %q for session %q", requestID, sessionID)
	}
	delete(h.permissionCh, key)
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

func (h *acpClientHandler) PermissionEvents(sessionID string) <-chan PermissionEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.permissionEventsLocked(sessionID)
}

func (h *acpClientHandler) permissionEventsLocked(sessionID string) chan PermissionEvent {
	events, ok := h.permissionEvents[sessionID]
	if !ok {
		events = make(chan PermissionEvent, 16)
		h.permissionEvents[sessionID] = events
	}
	return events
}

func (h *acpClientHandler) ForgetSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, ch := range h.permissionCh {
		if key.sessionID == sessionID {
			close(ch)
			delete(h.permissionCh, key)
		}
	}
	delete(h.permissionEvents, sessionID)
	delete(h.sessionUpdates, sessionID)
}

func (h *acpClientHandler) deletePermission(key permissionKey) {
	h.mu.Lock()
	delete(h.permissionCh, key)
	h.mu.Unlock()
}

// close unblocks all pending permission waiters and marks the handler as
// shut down.
func (h *acpClientHandler) close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.closed = true
	close(h.closeCh)
	for id, ch := range h.permissionCh {
		close(ch)
		delete(h.permissionCh, id)
	}
}

// ---------------------------------------------------------------------------
// Elicitation (ClientExperimental)
// ---------------------------------------------------------------------------

// UnstableCreateElicitation 缺少 Session ID，无法安全路由到具体 Turn。
func (h *acpClientHandler) UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	return acp.UnstableCreateElicitationResponse{}, errNotSupported
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
