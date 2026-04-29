//go:build !linux

package ptyrunner

import "os"

func enterTerminalInputMode(_ *os.File) (terminalInputMode, error) {
	return noopTerminalInputMode{}, nil
}
