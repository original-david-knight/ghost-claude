//go:build !linux

package ptyrunner

import (
	"os"
	"time"
)

func enterTerminalInputMode(_ *os.File) (terminalInputMode, error) {
	return noopTerminalInputMode{}, nil
}

func waitForTerminalInput(_ *os.File, _ time.Duration) (bool, error) {
	return false, nil
}
