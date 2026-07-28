package session

import (
	"context"
	"testing"
	"time"
)

func TestTurnTerminalStateCanOnlyBeCommittedOnce(t *testing.T) {
	turn := NewTurn("t-1", func() {})
	if !turn.Complete(TurnSnapshot{AgentText: "done"}) {
		t.Fatal("first terminal transition must succeed")
	}
	if turn.Interrupt(TurnSnapshot{Error: "late interrupt"}) {
		t.Fatal("second terminal transition must be ignored")
	}

	view := turn.Snapshot()
	if view.State != TurnCompleted || view.AgentText != "done" || view.Error != "" {
		t.Fatalf("unexpected terminal snapshot: %#v", view)
	}
}

func TestTurnWaitObservesStateChanges(t *testing.T) {
	turn := NewTurn("t-1", func() {})
	go func() {
		time.Sleep(10 * time.Millisecond)
		turn.RequirePermission(PermissionView{RequestID: "p-1"})
	}()

	view := turn.Wait(context.Background(), time.Second)
	if view.State != TurnPermissionRequired || view.Permission == nil || view.Permission.RequestID != "p-1" {
		t.Fatalf("unexpected wait result: %#v", view)
	}
}
