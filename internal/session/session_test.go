package session

import (
	"errors"
	"testing"

	"github.com/mapleafgo/acp-bridge/internal/driver"
)

func TestSessionBeginTurnRejectsBusySession(t *testing.T) {
	s := New(ID{AgentType: driver.AgentTypeCodex, AgentSessionID: "one"}, "/tmp")
	first := NewTurn("t-1", func() {})
	if err := s.BeginTurn(first); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginTurn(NewTurn("t-2", func() {})); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("expected ErrSessionBusy, got %v", err)
	}
}

func TestSessionViewIsAValueSnapshot(t *testing.T) {
	s := New(ID{AgentType: driver.AgentTypeClaude, AgentSessionID: "one"}, "/tmp")
	s.SetConfigOptions([]ConfigOptionInfo{{ID: "model", Value: "a"}})

	view := s.View()
	view.ConfigOpts[0].Value = "changed"

	if got := s.View().ConfigOpts[0].Value; got != "a" {
		t.Fatalf("session metadata leaked through view: %q", got)
	}
}
