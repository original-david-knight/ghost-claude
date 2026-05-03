package codex

func Subcommand(args []string) string {
	for _, arg := range args {
		switch arg {
		case "exec", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "completion", "sandbox", "debug", "apply", "resume", "fork", "cloud", "exec-server", "features", "help":
			return arg
		}
	}
	return ""
}

func IsInteractiveSubcommand(subcommand string) bool {
	switch subcommand {
	case "", "resume", "fork":
		return true
	default:
		return false
	}
}

func IsNonInteractiveSubcommand(subcommand string) bool {
	switch subcommand {
	case "exec", "review":
		return true
	default:
		return false
	}
}
