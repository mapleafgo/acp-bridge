package driver

import (
	"context"
	"io"
	"os/exec"
	"sync"
)

// AgentProcess 是 agent 子进程的完整所有权边界。
type AgentProcess interface {
	// Stdin 返回只归 ACP Client 使用的进程标准输入。
	Stdin() io.WriteCloser
	// Stdout 返回只归 ACP Client 使用的进程标准输出。
	Stdout() io.ReadCloser
	// Stderr 返回调用方必须持续消费或关闭的进程标准错误。
	Stderr() io.ReadCloser
	// Done 在唯一 Wait goroutine 回收进程后关闭。
	Done() <-chan struct{}
	// Err 在 Done 关闭后返回 cmd.Wait 的结果；进程仍运行或正常退出时为 nil。
	Err() error
	// Close 幂等请求进程退出，并在 ctx 到期时强制终止后等待回收。
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
		_ = stdout.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
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
