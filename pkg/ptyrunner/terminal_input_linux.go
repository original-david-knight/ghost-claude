//go:build linux

package ptyrunner

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type linuxTerminalInputMode struct {
	fd    int
	state *unix.Termios
}

func enterTerminalInputMode(file *os.File) (terminalInputMode, error) {
	if file == nil {
		return noopTerminalInputMode{}, nil
	}

	fd := int(file.Fd())
	state, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) {
			return noopTerminalInputMode{}, nil
		}
		return nil, err
	}

	updated := *state
	updated.Iflag &^= unix.ICRNL
	updated.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN
	updated.Cc[unix.VMIN] = 1
	updated.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &updated); err != nil {
		return nil, err
	}

	return &linuxTerminalInputMode{
		fd:    fd,
		state: state,
	}, nil
}

func (m *linuxTerminalInputMode) Restore() error {
	if m == nil || m.state == nil {
		return nil
	}

	return unix.IoctlSetTermios(m.fd, unix.TCSETSF, m.state)
}

func (m *linuxTerminalInputMode) Interactive() bool {
	return m != nil && m.state != nil
}

func waitForTerminalInput(file *os.File, timeout time.Duration) (bool, error) {
	if file == nil {
		return false, nil
	}

	timeoutMillis := int(timeout / time.Millisecond)
	if timeout > 0 && timeoutMillis == 0 {
		timeoutMillis = 1
	}

	pollFDs := []unix.PollFd{{
		Fd:     int32(file.Fd()),
		Events: unix.POLLIN,
	}}
	for {
		n, err := unix.Poll(pollFDs, timeoutMillis)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		return n > 0 && pollFDs[0].Revents&unix.POLLIN != 0, nil
	}
}
