package main

import (
	"context"
	"errors"
	"testing"
)

type fakeManager struct {
	closeCalls int
}

func (m *fakeManager) Close(context.Context) error {
	m.closeCalls++
	return nil
}

type fakeServer struct {
	err error
}

func (s fakeServer) Run() error {
	return s.err
}

func TestRunClosesManagerWhenServerStops(t *testing.T) {
	manager := &fakeManager{}
	err := runWith(manager, fakeServer{err: errors.New("stdio closed")})
	if err == nil || manager.closeCalls != 1 {
		t.Fatalf("err=%v closeCalls=%d", err, manager.closeCalls)
	}
}

func TestRunClosesManagerAfterNormalEOF(t *testing.T) {
	manager := &fakeManager{}
	if err := runWith(manager, fakeServer{}); err != nil {
		t.Fatal(err)
	}
	if manager.closeCalls != 1 {
		t.Fatalf("closeCalls=%d", manager.closeCalls)
	}
}
