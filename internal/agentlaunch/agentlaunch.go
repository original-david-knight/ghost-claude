package agentlaunch

import (
	"context"
	"fmt"
	"io"

	"vibedrive/internal/config"
	"vibedrive/pkg/agentcli/claude"
	codexcli "vibedrive/pkg/agentcli/codex"
)

type Runner interface {
	RunPrompt(ctx context.Context, prompt string) error
	Close() error
}

type claudeClient interface {
	RunPrompt(ctx context.Context, session *claude.Session, prompt string) error
	RunInteractivePrompt(ctx context.Context, session *claude.Session, prompt string) error
	Close(session *claude.Session) error
}

type codexClient interface {
	RunPrompt(ctx context.Context, session *codexcli.Session, prompt string) error
	RunInteractivePrompt(ctx context.Context, session *codexcli.Session, prompt string) error
	Close(session *codexcli.Session) error
}

type claudeRunner struct {
	client  claudeClient
	session *claude.Session
}

func (r *claudeRunner) RunPrompt(ctx context.Context, prompt string) error {
	return r.client.RunPrompt(ctx, r.session, prompt)
}

func (r *claudeRunner) RunInteractivePrompt(ctx context.Context, prompt string) error {
	return r.client.RunInteractivePrompt(ctx, r.session, prompt)
}

func (r *claudeRunner) Close() error {
	return r.client.Close(r.session)
}

type codexRunner struct {
	client  codexClient
	session *codexcli.Session
}

func (r *codexRunner) RunPrompt(ctx context.Context, prompt string) error {
	return r.client.RunPrompt(ctx, r.session, prompt)
}

func (r *codexRunner) RunInteractivePrompt(ctx context.Context, prompt string) error {
	return r.client.RunInteractivePrompt(ctx, r.session, prompt)
}

func (r *codexRunner) Close() error {
	return r.client.Close(r.session)
}

func LaunchAgent(cfg *config.Config, agentType, role string, stdout, stderr io.Writer) (Runner, error) {
	resolvedAgent, err := config.ResolveAgent(agentType, "", role)
	if err != nil {
		return nil, err
	}

	switch resolvedAgent {
	case config.AgentClaude:
		client, err := claude.New(
			cfg.Claude.Command,
			cfg.Claude.Args,
			cfg.Workspace,
			cfg.Claude.Transport,
			cfg.Claude.StartupTimeout,
			stdout,
			stderr,
		)
		if err != nil {
			return nil, err
		}

		session, err := claude.NewSession(config.SessionStrategySessionID)
		if err != nil {
			return nil, err
		}

		return &claudeRunner{client: client, session: session}, nil
	case config.AgentCodex:
		client, err := codexcli.New(
			cfg.Codex.Command,
			cfg.Codex.Args,
			cfg.Workspace,
			cfg.Codex.Transport,
			cfg.Codex.StartupTimeout,
			stdout,
			stderr,
		)
		if err != nil {
			return nil, err
		}

		session, err := codexcli.NewSession()
		if err != nil {
			return nil, err
		}

		return &codexRunner{client: client, session: session}, nil
	default:
		return nil, fmt.Errorf("%s %q is not supported; expected claude or codex", role, agentType)
	}
}
