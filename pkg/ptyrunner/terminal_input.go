package ptyrunner

type terminalInputMode interface {
	Restore() error
	Interactive() bool
}

type noopTerminalInputMode struct{}

func (noopTerminalInputMode) Restore() error    { return nil }
func (noopTerminalInputMode) Interactive() bool { return false }
