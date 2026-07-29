package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewServer(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	if server == nil || server.sdkServer == nil || server.manager == nil {
		t.Fatalf("invalid server: %#v", server)
	}
}

func TestToolsViaInMemoryTransport(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		if err := server.sdkServer.Run(ctx, serverTransport); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	connection, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()

	tools, err := connection.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool)
	for _, tool := range tools.Tools {
		found[tool.Name] = true
	}
	for _, name := range []string{"acp_chat", "acp_progress", "acp_sessions", "acp_interrupt"} {
		if !found[name] {
			t.Fatalf("tool %q not registered", name)
		}
	}

	result, err := connection.CallTool(ctx, &sdk.CallToolParams{
		Name: "acp_chat",
		Arguments: map[string]any{
			"prompt": "hello",
		},
	})
	if err != nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestSkillResourceIsEmbedded(t *testing.T) {
	server, _, _ := newTestServer(t, 10)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		if err := server.sdkServer.Run(ctx, serverTransport); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	connection, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()

	result, err := connection.ReadResource(ctx, &sdk.ReadResourceParams{URI: skillResourceURI})
	if err != nil || len(result.Contents) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Contents[0].Text != skillMD || !strings.Contains(skillMD, "name: acp-bridge") {
		t.Fatal("embedded skill mismatch")
	}
}
