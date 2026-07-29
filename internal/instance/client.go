package instance

import (
	"context"

	acp "github.com/coder/acp-go-sdk"
	"github.com/mapleafgo/acp-bridge/internal/client"
)

// ACPClient 是 Instance 使用的完整 agent 客户端合同。
type ACPClient interface {
	NewSession(context.Context, string) (*acp.NewSessionResponse, error)
	Prompt(context.Context, string, []acp.ContentBlock) (*acp.PromptResponse, error)
	Cancel(context.Context, string) error
	CloseSession(context.Context, string) (*acp.CloseSessionResponse, error)
	ListSessions(context.Context) (*acp.ListSessionsResponse, error)
	LoadSession(context.Context, string, string) (*acp.LoadSessionResponse, error)
	ResumeSession(context.Context, string, string) (*acp.ResumeSessionResponse, error)
	SetSessionMode(context.Context, string, string) (*acp.SetSessionModeResponse, error)
	SetSessionConfigOption(context.Context, string, string, string) error
	ForkSession(context.Context, string, string) (*acp.UnstableForkSessionResponse, error)
	DeleteSession(context.Context, string) (*acp.UnstableDeleteSessionResponse, error)
	RespondPermission(string, string, acp.RequestPermissionResponse) error
	PopUpdates(string) []acp.SessionNotification
	PeekUpdates(string) []acp.SessionNotification
	PermissionEvents(string) <-chan client.PermissionEvent
	ForgetSession(string)
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}
