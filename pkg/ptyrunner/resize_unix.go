//go:build !windows

package ptyrunner

import (
	"os/signal"
	"syscall"

	"github.com/creack/pty"
)

func (s *Session) startResizeForwarder() {
	if s.resizeRef == nil {
		return
	}
	if _, err := pty.GetsizeFull(s.resizeRef); err != nil {
		return
	}

	signal.Notify(s.resizeSig, syscall.SIGWINCH)
	_ = pty.InheritSize(s.resizeRef, s.pty)

	go func() {
		defer signal.Stop(s.resizeSig)
		for {
			select {
			case <-s.done:
				return
			case <-s.resizeSig:
				_ = pty.InheritSize(s.resizeRef, s.pty)
			}
		}
	}()
}
