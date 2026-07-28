package driver

import (
	"context"
	"io"
	"os/exec"
	"sync"
)

// AgentProcess 是 agent 子进程的完整所有权边界。
type AgentProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}

type agentProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan struct{}

	mu        sync.Mutex
	waitErr   error
	closeErr  error
	closeOnce sync.Once
}

func startProcess(ctx context.Context, exe string, args []string) (AgentProcess, error) {
	cmd, err := buildCmd(ctx, exe, args)
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	process := &agentProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan struct{}),
	}
	go process.wait()
	return process, nil
}

func (p *agentProcess) wait() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	close(p.done)
}

func (p *agentProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *agentProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *agentProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *agentProcess) Done() <-chan struct{} { return p.done }

func (p *agentProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *agentProcess) Close(ctx context.Context) error {
	p.closeOnce.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			<-p.done
			p.mu.Lock()
			p.closeErr = ctx.Err()
			p.mu.Unlock()
		}
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeErr
}
