package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/driver"
)

// Client is a high-level ACP client that manages the full lifecycle of an
// agent subprocess and provides a type-safe wrapper around the ACP protocol.
//
// Usage:
//
//	drv := driver.NewDriver(driver.AgentTypeCodex, cfg)
//	if err != nil { … }
//	defer cl.Close()
//
//	initResp, _ := cl.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1})
//	sessResp, _ := cl.NewSession(ctx, "/home/user/project")
type Client struct {
	conn    *acp.ClientSideConnection
	handler *acpClientHandler
	driver  driver.AgentDriver
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	mu      sync.Mutex
	closed  bool
}

// New creates a Client, starts the agent subprocess via drv, establishes
// the ACP connection, and sends the initialize handshake.
//
// The mcpServer is reserved for future use (registering MCP tool transport
// with the agent during session setup); pass nil for now.
func New(ctx context.Context, drv driver.AgentDriver) (*Client, error) {
	stdout, stdin, stderr, err := drv.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	// Drain stderr in the background to avoid blocking the child process.
	go func() {
		if stderr != nil {
			_, _ = io.Copy(io.Discard, stderr)
		}
	}()

	handler := newHandler()
	conn := acp.NewClientSideConnection(handler, stdin, stdout)
	if l := slog.Default(); l.Enabled(ctx, slog.LevelDebug) {
		conn.SetLogger(l)
	}

	// Attempt initialization; if it fails, kill the child.
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	}); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	c := &Client{
		conn:    conn,
		handler: handler,
		driver:  drv,
		stdin:   stdin,
		stdout:  stdout,
	}
	slog.DebugContext(ctx, "acp client initialized", "agent_type", drv.Type())
	return c, nil
}

// ---------------------------------------------------------------------------
// ACP connection-level methods
// ---------------------------------------------------------------------------

// Initialize sends the initialize handshake. Must only be called once,
// right after New — the SDK handles this internally but the method is
// exposed for advanced use (re-initialization after reconnect).
func (c *Client) Initialize(ctx context.Context, req acp.InitializeRequest) (*acp.InitializeResponse, error) {
	resp, err := c.conn.Initialize(ctx, req)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// NewSession creates a new ACP session with the given working directory.
func (c *Client) NewSession(ctx context.Context, cwd string) (*acp.NewSessionResponse, error) {
	resp, err := c.conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{}, // SDK Validate rejects nil
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// Prompt sends a user prompt to the given session and returns the agent's
// response. Streaming session updates are accumulated in the handler and
// can be retrieved with PopUpdates.
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResponse, error) {
	resp, err := c.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: acp.SessionId(sessionID),
		Prompt:    blocks,
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// Cancel sends a cancellation notification for the given session.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.conn.Cancel(ctx, acp.CancelNotification{
		SessionId: acp.SessionId(sessionID),
	})
}

// CloseSession closes an ACP session cleanly.
func (c *Client) CloseSession(ctx context.Context, sessionID string) (*acp.CloseSessionResponse, error) {
	resp, err := c.conn.CloseSession(ctx, acp.CloseSessionRequest{
		SessionId: acp.SessionId(sessionID),
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListSessions returns the list of active sessions from the agent.
func (c *Client) ListSessions(ctx context.Context) (*acp.ListSessionsResponse, error) {
	resp, err := c.conn.ListSessions(ctx, acp.ListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// LoadSession loads and replays a persisted session.
func (c *Client) LoadSession(ctx context.Context, sessionID string) (*acp.LoadSessionResponse, error) {
	resp, err := c.conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId: acp.SessionId(sessionID),
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// ResumeSession resumes a previously closed ACP session.
func (c *Client) ResumeSession(ctx context.Context, sessionID string) (*acp.ResumeSessionResponse, error) {
	resp, err := c.conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId: acp.SessionId(sessionID),
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetSessionMode changes the permission mode of a session.
func (c *Client) SetSessionMode(ctx context.Context, sessionID, modeID string) (*acp.SetSessionModeResponse, error) {
	resp, err := c.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: acp.SessionId(sessionID),
		ModeId:    acp.SessionModeId(modeID),
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetSessionConfigOption sets a configuration key/value on a session.
// The key is the configuration option ID, and the value is the
// configuration value ID (both determined by the agent's capabilities).
func (c *Client) SetSessionConfigOption(ctx context.Context, sessionID, configID, valueID string) error {
	_, err := c.conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sessionID),
			ConfigId:  acp.SessionConfigId(configID),
			Value:     acp.SessionConfigValueId(valueID),
		},
	})
	return err
}

// ForkSession creates a fork (copy) of an existing session.
func (c *Client) ForkSession(ctx context.Context, sessionID string) (*acp.UnstableForkSessionResponse, error) {
	resp, err := c.conn.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{
		SessionId: acp.SessionId(sessionID),
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteSession permanently removes a persisted session.
func (c *Client) DeleteSession(ctx context.Context, sessionID string) (*acp.UnstableDeleteSessionResponse, error) {
	resp, err := c.conn.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{
		SessionId: acp.SessionId(sessionID),
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Permission response injection
// ---------------------------------------------------------------------------

// RespondPermission injects a user decision into a pending permission
// request, unblocking the handler's RequestPermission call.
//
// Example:
//
//	cl.RespondPermission(ctx, requestID, acp.RequestPermissionResponse{
//	    Outcome: acp.RequestPermissionOutcome{
//	        Selected: &acp.RequestPermissionOutcomeSelected{
//	            OptionId: "allow",
//	        },
//	    },
//	})
func (c *Client) RespondPermission(requestID string, resp acp.RequestPermissionResponse) error {
	return c.handler.Respond(requestID, resp)
}

// PopUpdates drains and returns all accumulated SessionNotifications for
// the given session ID. Call this after a Prompt returns to process
// streaming updates the agent sent during the turn.
func (c *Client) PopUpdates(sessionID string) []acp.SessionNotification {
	return c.handler.PopUpdates(sessionID)
}

// PeekUpdates returns a snapshot of buffered SessionNotifications for
// the given session ID without draining. Used to observe real-time
// agent progress while a prompt turn is in flight.
func (c *Client) PeekUpdates(sessionID string) []acp.SessionNotification {
	return c.handler.PeekUpdates(sessionID)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Close gracefully shuts down the ACP connection and closes the pipes
// to the agent subprocess. It is safe to call multiple times.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// Signal the handler to stop blocking permission waiters.
	c.handler.close()

	// Close stdin to signal EOF to the agent (it should exit on its own).
	// Then close stdout to release the read side.
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	return nil
}

// Done returns a channel that closes when the agent disconnects.
func (c *Client) Done() <-chan struct{} {
	return c.conn.Done()
}

// PermissionSignal returns a channel that receives each incoming permission
// request from the agent. Callers select on this channel during a prompt turn
// to detect when the agent needs user authorization.
func (c *Client) PermissionSignal() <-chan acp.RequestPermissionRequest {
	return c.handler.PermissionSignal()
}

// Driver returns the underlying AgentDriver used to start this client.
func (c *Client) Driver() driver.AgentDriver {
	return c.driver
}
