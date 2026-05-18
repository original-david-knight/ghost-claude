package config

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const cliVersionTimeout = 5 * time.Second

var resolveCLIVersion = defaultResolveCLIVersion

// ResolveCLIVersion returns the trimmed output reported by command --version.
func ResolveCLIVersion(command string) (string, error) {
	return ResolveCLIVersionInDir(command, "")
}

// ResolveCLIVersionInDir returns command --version using workdir for relative commands with path separators.
func ResolveCLIVersionInDir(command, workdir string) (string, error) {
	return resolveCLIVersion(command, workdir)
}

func (c *Config) CheckPinnedAgentVersions() error {
	if c == nil {
		return nil
	}

	if err := checkPinnedAgentVersion(AgentCodex, c.Codex.Command, c.Codex.Version, c.Workspace); err != nil {
		return err
	}
	return nil
}

func checkPinnedAgentVersion(agent, command, pinned, workdir string) error {
	pinned = strings.TrimSpace(pinned)
	if pinned == "" {
		return nil
	}

	live, err := ResolveCLIVersionInDir(command, workdir)
	if err != nil {
		return fmt.Errorf("%s.version is pinned to %q but failed to read the live CLI version from %q: %w", agent, pinned, command, err)
	}
	if live != pinned {
		return fmt.Errorf("%s.version %q does not match live %s CLI version %q from %q; update %s.version only after intentionally upgrading the CLI", agent, pinned, agent, live, command, agent)
	}
	return nil
}

func defaultResolveCLIVersion(command, workdir string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("command is required")
	}

	resolved := command
	if !strings.ContainsAny(command, `/\`) {
		var err error
		resolved, err = exec.LookPath(command)
		if err != nil {
			return "", err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cliVersionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, resolved, "--version")
	if strings.TrimSpace(workdir) != "" {
		cmd.Dir = workdir
	}

	output, err := cmd.CombinedOutput()
	reported := strings.TrimSpace(string(output))
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s --version timed out after %s", resolved, cliVersionTimeout)
	}
	if err != nil {
		if reported != "" {
			return "", fmt.Errorf("%s --version failed: %w: %s", resolved, err, reported)
		}
		return "", fmt.Errorf("%s --version failed: %w", resolved, err)
	}
	if reported == "" {
		return "", fmt.Errorf("%s --version returned empty output", resolved)
	}

	return reported, nil
}
