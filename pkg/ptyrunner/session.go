package ptyrunner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
)

type Monitor interface {
	Consume([]byte)
}

type Config struct {
	Label   string
	Command string
	Args    []string
	Workdir string
	Stdout  io.Writer
	Stdin   *os.File
	// ResizeRef is the terminal whose dimensions the inner PTY mirrors via SIGWINCH; not the output destination (use Stdout).
	ResizeRef *os.File
	Monitor   Monitor
}

type Session struct {
	cmd       *exec.Cmd
	pty       *os.File
	stdout    io.Writer
	stdin     *os.File
	resizeRef *os.File
	monitor   Monitor
	inputMode terminalInputMode
	done      chan struct{}
	doneOnce  sync.Once
	waitErr   error
	waitMu    sync.Mutex
	resizeSig chan os.Signal
}

const stdinPollInterval = 100 * time.Millisecond

func Start(ctx context.Context, cfg Config) (*Session, error) {
	stdin := cfg.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	resizeRef := cfg.ResizeRef
	if resizeRef == nil {
		resizeRef = os.Stdout
	}

	inputMode, err := enterTerminalInputMode(stdin)
	if err != nil {
		return nil, fmt.Errorf("prepare terminal input: %w", err)
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Workdir

	tty, err := pty.Start(cmd)
	if err != nil {
		_ = inputMode.Restore()
		label := cfg.Label
		if label == "" {
			label = cfg.Command
		}
		return nil, fmt.Errorf("start %s: %w", label, err)
	}

	session := &Session{
		cmd:       cmd,
		pty:       tty,
		stdout:    cfg.Stdout,
		stdin:     stdin,
		resizeRef: resizeRef,
		monitor:   cfg.Monitor,
		inputMode: inputMode,
		done:      make(chan struct{}),
		resizeSig: make(chan os.Signal, 1),
	}

	session.startOutputPump()
	session.startInputPump()
	session.startWaiter()
	session.startResizeForwarder()

	return session, nil
}

func (s *Session) Write(p []byte) (int, error) {
	if s == nil {
		return 0, os.ErrClosed
	}
	return s.pty.Write(p)
}

func (s *Session) Close(exitSequence string, timeout time.Duration) error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return s.ExitErr()
	default:
	}

	_, _ = io.WriteString(s, exitSequence)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-s.done:
		return s.ExitErr()
	case <-timer.C:
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-s.done
		return s.ExitErr()
	}
}

func (s *Session) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

func (s *Session) ExitErr() error {
	if s == nil {
		return nil
	}
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	return s.waitErr
}

func (s *Session) startOutputPump() {
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := s.pty.Read(buffer)
			if n > 0 {
				chunk := append([]byte(nil), buffer[:n]...)
				if s.monitor != nil {
					s.monitor.Consume(chunk)
				}
				if s.stdout != nil {
					_, _ = s.stdout.Write(chunk)
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

func (s *Session) startInputPump() {
	if !s.inputMode.Interactive() || s.stdin == nil {
		return
	}

	go func() {
		buffer := make([]byte, 256)
		for {
			select {
			case <-s.done:
				return
			default:
			}

			ready, err := waitForTerminalInput(s.stdin, stdinPollInterval)
			if err != nil {
				return
			}
			if !ready {
				continue
			}

			n, err := s.stdin.Read(buffer)
			if n > 0 {
				if _, werr := s.pty.Write(buffer[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

func (s *Session) startWaiter() {
	go func() {
		err := s.cmd.Wait()
		_ = s.pty.Close()
		_ = s.inputMode.Restore()

		s.waitMu.Lock()
		s.waitErr = err
		s.waitMu.Unlock()

		s.doneOnce.Do(func() {
			close(s.done)
		})
	}()
}
