package mcp

import (
	"context"
	"log/slog"

	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/instance"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps the SDK MCP server.
type Server struct {
	sdkServer *sdk.Server
	config    *config.Config
	manager   *instance.Manager
}

// NewServer creates a new MCP server, registers all tools, and returns it.
func NewServer(cfg *config.Config, manager *instance.Manager) *Server {
	srv := &Server{
		sdkServer: sdk.NewServer(&sdk.Implementation{
			Name:    "acp-bridge",
			Version: "0.1.0",
		}, &sdk.ServerOptions{
			Logger: slog.Default(),
		}),
		config:  cfg,
		manager: manager,
	}
	srv.registerTools()
	srv.registerSkill()
	return srv
}

// registerTools registers all MCP tools on the SDK server.
func (s *Server) registerTools() {
	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_chat",
		Description: "Send a prompt to an ACP agent as a subagent backend. Returns the agent's full reply, or a permission_required JSON if the agent requests authorization for a tool call. Use session_id to continue a multi-turn conversation.",
	}, s.handleAcpChat)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_respond",
		Description: "Respond to a permission_required request from an ACP agent. Provide the session_id, request_id, and outcome to approve or deny the requested action, then receive the agent's continued response.",
	}, s.handleAcpRespond)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_interrupt",
		Description: "Interrupt the current ACP turn. Requires the session_id and turn_id returned by acp_chat, and retains an interrupted progress snapshot until the next turn.",
	}, s.handleAcpInterrupt)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_close",
		Description: "Close an ACP session and release its resources.",
	}, s.handleAcpClose)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_sessions",
		Description: "List all active ACP sessions managed by acp-bridge.",
	}, s.handleAcpSessions)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_progress",
		Description: "Check a session's current or most recent turn. turn_id is optional and enables exact-match validation.",
	}, s.handleAcpProgress)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_session_info",
		Description: "Get detailed metadata for a session: config options (model, reasoning effort, etc.), available slash commands, current permission mode, title, and working directory. Call this to inspect what the agent reported about its capabilities.",
	}, s.handleAcpSessionInfo)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_set_mode",
		Description: "Set the permission mode for an ACP session (e.g. default, accept-edits).",
	}, s.handleAcpSetMode)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_set_config",
		Description: "Set a configuration option on an ACP session.",
	}, s.handleAcpSetConfig)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_fork_session",
		Description: "Fork (copy) an existing ACP session to create a new independent session.",
	}, s.handleAcpForkSession)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_load_session",
		Description: "Load a persisted ACP session by its ID from the agent.",
	}, s.handleAcpLoadSession)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_list_history",
		Description: "List persisted ACP sessions from the agent's session store.",
	}, s.handleAcpListHistory)

	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_resume_session",
		Description: "Resume a previously closed ACP session.",
	}, s.handleAcpResumeSession)
	sdk.AddTool(s.sdkServer, &sdk.Tool{
		Name:        "acp_delete_session",
		Description: "Delete a persisted ACP session from the agent's session store.",
	}, s.handleAcpDeleteSession)
}

// Run starts the MCP server on stdin/stdout using the SDK's StdioTransport.
func (s *Server) Run() error {
	return s.sdkServer.Run(context.Background(), &sdk.StdioTransport{})
}
