package driver

import (
	"context"
	"testing"
	"time"
)

func TestAgentProcessCloseWaitsExactlyOnce(t *testing.T) {
	process, err := startProcess(context.Background(), "sh", []string{"-c", "read line"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(ctx); err != nil {
		t.Fatalf("second Close must be idempotent: %v", err)
	}
	select {
	case <-process.Done():
	default:
		t.Fatal("Done must be closed")
	}
}

func TestAgentProcessReportsNaturalExit(t *testing.T) {
	process, err := startProcess(context.Background(), "sh", []string{"-c", "exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for process")
	}
	if process.Err() == nil {
		t.Fatal("expected exit error")
	}
}
