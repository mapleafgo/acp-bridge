package session

import (
	"errors"
	"testing"

	"github.com/mapleafgo/acp-bridge/internal/driver"
)

func TestParseIDPreservesColonInAgentSessionID(t *testing.T) {
	id, err := ParseID("codex:thread:child")
	if err != nil {
		t.Fatal(err)
	}
	if id.AgentType != driver.AgentTypeCodex || id.AgentSessionID != "thread:child" {
		t.Fatalf("unexpected ID: %#v", id)
	}
	if got := id.String(); got != "codex:thread:child" {
		t.Fatalf("String()=%q", got)
	}
}

func TestParseIDRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{"", "codex", "codex:", "unknown:id"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseID(raw); !errors.Is(err, ErrInvalidSessionID) {
				t.Fatalf("ParseID(%q) error=%v", raw, err)
			}
		})
	}
}
