package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vibedrive/internal/codexargs"

	"gopkg.in/yaml.v3"
)

const (
	StepTypeClaude = "claude"
	StepTypeCodex  = "codex"
	StepTypeAgent  = "agent"
	StepTypeExec   = "exec"

	ClaudeTransportPrint = "print"
	ClaudeTransportTUI   = "tui"
	CodexTransportExec   = "exec"
	CodexTransportTUI    = "tui"

	SessionStrategySessionID = "session_id"
	SessionStrategyContinue  = "continue"

	defaultClaudeEffort         = "max"
	defaultClaudePermissionMode = "bypassPermissions"
	defaultStartupTimeout       = "30s"
	defaultCodexReasoningEffort = "xhigh"
	defaultCodexBypassFlag      = "--dangerously-bypass-approvals-and-sandbox"

	AgentClaude = "claude"
	AgentCodex  = "codex"

	StepActorCoder    = "coder"
	StepActorReviewer = "reviewer"
)

type Config struct {
	Path                 string              `yaml:"-"`
	BaseDir              string              `yaml:"-"`
	DryRun               bool                `yaml:"dry_run"`
	Workspace            string              `yaml:"workspace"`
	PlanFile             string              `yaml:"plan_file"`
	MaxIterations        int                 `yaml:"max_iterations"`
	MaxStalledIterations int                 `yaml:"max_stalled_iterations"`
	DefaultWorkflow      string              `yaml:"default_workflow"`
	Coder                string              `yaml:"-"`
	Reviewer             string              `yaml:"-"`
	Claude               ClaudeConfig        `yaml:"claude"`
	Codex                CodexConfig         `yaml:"codex"`
	Steps                []Step              `yaml:"steps"`
	Workflows            map[string]Workflow `yaml:"workflows"`
}

type ClaudeConfig struct {
	Command         string   `yaml:"command"`
	Args            []string `yaml:"args"`
	Transport       string   `yaml:"transport"`
	StartupTimeout  string   `yaml:"startup_timeout"`
	SessionStrategy string   `yaml:"session_strategy"`
}

type CodexConfig struct {
	Command        string   `yaml:"command"`
	Args           []string `yaml:"args"`
	Transport      string   `yaml:"transport"`
	StartupTimeout string   `yaml:"startup_timeout"`
}

type Step struct {
	Name            string            `yaml:"name"`
	Type            string            `yaml:"type"`
	Actor           string            `yaml:"actor"`
	Prompt          string            `yaml:"prompt"`
	Command         []string          `yaml:"command"`
	WorkingDir      string            `yaml:"working_dir"`
	Env             map[string]string `yaml:"env"`
	RequiredOutputs []string          `yaml:"required_outputs"`
	FreshSession    bool              `yaml:"fresh_session"`
	Timeout         string            `yaml:"timeout"`
	ContinueOnError bool              `yaml:"continue_on_error"`
	Disabled        bool              `yaml:"disabled"`
}

type Workflow struct {
	Steps []Step `yaml:"steps"`
}

func Load(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	cfg.Path = absPath
	cfg.BaseDir = filepath.Dir(absPath)

	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	if !filepath.IsAbs(cfg.Workspace) {
		cfg.Workspace = filepath.Join(cfg.BaseDir, cfg.Workspace)
	}
	cfg.Workspace = filepath.Clean(cfg.Workspace)

	if cfg.PlanFile == "" {
		cfg.PlanFile = "vibedrive-plan.yaml"
	}
	if !filepath.IsAbs(cfg.PlanFile) {
		cfg.PlanFile = filepath.Join(cfg.Workspace, cfg.PlanFile)
	}
	cfg.PlanFile = filepath.Clean(cfg.PlanFile)

	if cfg.Claude.Command == "" {
		cfg.Claude.Command = "claude"
	}
	if cfg.Claude.Transport == "" {
		cfg.Claude.Transport = ClaudeTransportTUI
	}
	if cfg.Claude.StartupTimeout == "" {
		cfg.Claude.StartupTimeout = defaultStartupTimeout
	}
	if cfg.Claude.SessionStrategy == "" {
		cfg.Claude.SessionStrategy = SessionStrategySessionID
	}
	cfg.Claude.Args = ensureDefaultClaudeArgs(cfg.Claude.Args)

	if cfg.Codex.Command == "" {
		cfg.Codex.Command = "codex"
	}
	if cfg.Codex.Transport == "" {
		cfg.Codex.Transport = defaultCodexTransport(cfg.Codex.Args)
	}
	if cfg.Codex.StartupTimeout == "" {
		cfg.Codex.StartupTimeout = defaultStartupTimeout
	}
	cfg.Codex.Args = ensureDefaultCodexArgs(cfg.Codex.Args, cfg.Codex.Transport)

	for i := range cfg.Steps {
		if cfg.Steps[i].Type == "" {
			cfg.Steps[i].Type = StepTypeClaude
		}
	}
	for name, workflow := range cfg.Workflows {
		for i := range workflow.Steps {
			if workflow.Steps[i].Type == "" {
				workflow.Steps[i].Type = StepTypeClaude
			}
		}
		cfg.Workflows[name] = workflow
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.MaxIterations < 0 {
		return fmt.Errorf("max_iterations must be >= 0")
	}
	if c.MaxStalledIterations < 1 {
		return fmt.Errorf("max_stalled_iterations must be >= 1")
	}
	if c.Claude.Command == "" {
		return fmt.Errorf("claude.command is required")
	}
	if strings.TrimSpace(c.Coder) != "" && !isValidAgent(c.CoderAgent()) {
		return fmt.Errorf("coder %q is not supported", c.Coder)
	}
	if strings.TrimSpace(c.Reviewer) != "" && !isValidAgent(c.ReviewerAgent()) {
		return fmt.Errorf("reviewer %q is not supported", c.Reviewer)
	}

	switch normalize(c.Claude.Transport) {
	case ClaudeTransportPrint, ClaudeTransportTUI:
	default:
		return fmt.Errorf("unsupported claude.transport %q", c.Claude.Transport)
	}

	if strings.TrimSpace(c.Claude.StartupTimeout) == "" {
		return fmt.Errorf("claude.startup_timeout is required")
	}

	switch normalize(c.Claude.SessionStrategy) {
	case "", SessionStrategySessionID, SessionStrategyContinue:
	default:
		return fmt.Errorf("unsupported claude.session_strategy %q", c.Claude.SessionStrategy)
	}

	codexTransport := normalize(c.Codex.Transport)
	if codexTransport == "" {
		codexTransport = defaultCodexTransport(c.Codex.Args)
	}

	switch codexTransport {
	case CodexTransportExec, CodexTransportTUI:
	default:
		return fmt.Errorf("unsupported codex.transport %q", c.Codex.Transport)
	}

	if err := validateCodexArgs(codexTransport, c.Codex.Args); err != nil {
		return err
	}

	if len(c.Steps) == 0 && len(c.Workflows) == 0 {
		return fmt.Errorf("at least one step or workflow is required")
	}

	for i, step := range c.Steps {
		if err := validateStep(fmt.Sprintf("steps[%d]", i), step); err != nil {
			return err
		}
	}

	if c.usesAgentSteps() {
		if strings.TrimSpace(c.Coder) == "" {
			return fmt.Errorf("coder is required when using agent steps")
		}
		if strings.TrimSpace(c.Reviewer) == "" {
			return fmt.Errorf("reviewer is required when using agent steps")
		}
	}

	for name, workflow := range c.Workflows {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("workflow names must not be empty")
		}
		if len(workflow.Steps) == 0 {
			return fmt.Errorf("workflows.%s.steps must not be empty", name)
		}
		for i, step := range workflow.Steps {
			if err := validateStep(fmt.Sprintf("workflows.%s.steps[%d]", name, i), step); err != nil {
				return err
			}
		}
	}

	if c.PlanFile != "" {
		if strings.TrimSpace(c.DefaultWorkflow) != "" {
			if _, ok := c.Workflows[c.DefaultWorkflow]; !ok && len(c.Steps) == 0 {
				return fmt.Errorf("default_workflow %q does not exist", c.DefaultWorkflow)
			}
		}
	}

	return nil
}

func defaultConfig() Config {
	return Config{
		Workspace:            ".",
		PlanFile:             "vibedrive-plan.yaml",
		MaxStalledIterations: 2,
		Coder:                AgentCodex,
		Reviewer:             AgentClaude,
		Claude: ClaudeConfig{
			Command:         "claude",
			Args:            []string{"--effort", defaultClaudeEffort},
			Transport:       ClaudeTransportTUI,
			StartupTimeout:  defaultStartupTimeout,
			SessionStrategy: SessionStrategySessionID,
		},
		Codex: CodexConfig{
			Command:        "codex",
			StartupTimeout: defaultStartupTimeout,
			Args:           defaultCodexArgs(CodexTransportTUI),
		},
	}
}

func normalize(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func NormalizeAgent(value string) string {
	return normalizeAgent(value)
}

func normalizeAgent(value string) string {
	return normalize(value)
}

func ResolveAgent(value, fallback, role string) (string, error) {
	resolved := NormalizeAgent(value)
	if resolved == "" {
		resolved = NormalizeAgent(fallback)
	}
	if isValidAgent(resolved) {
		return resolved, nil
	}

	role = strings.TrimSpace(role)
	if role == "" {
		return "", fmt.Errorf("agent %q is not supported; expected claude or codex", value)
	}
	return "", fmt.Errorf("%s %q is not supported; expected claude or codex", role, value)
}

func isValidAgent(value string) bool {
	switch normalizeAgent(value) {
	case AgentClaude, AgentCodex:
		return true
	default:
		return false
	}
}

func (c *Config) CoderAgent() string {
	return normalizeAgent(c.Coder)
}

func (c *Config) ReviewerAgent() string {
	return normalizeAgent(c.Reviewer)
}

func ensureDefaultClaudeArgs(args []string) []string {
	withDefault := append([]string{}, args...)
	if !hasClaudeEffortArg(withDefault) {
		withDefault = append(withDefault, "--effort", defaultClaudeEffort)
	}
	if !hasClaudePermissionArg(withDefault) {
		withDefault = append(withDefault, "--permission-mode", defaultClaudePermissionMode)
	}
	return withDefault
}

func ensureDefaultCodexArgs(args []string, transport string) []string {
	if len(args) == 0 {
		return defaultCodexArgs(transport)
	}

	withDefault := ensureCodexYOLOArgs(args)
	if hasCodexReasoningArg(withDefault) {
		return withDefault
	}
	withDefault = append(withDefault, "-c", defaultCodexReasoningConfig())
	return withDefault
}

func defaultCodexArgs(transport string) []string {
	args := []string{defaultCodexBypassFlag}
	if normalize(transport) == CodexTransportExec {
		args = append(args, "exec")
	}
	return append(args, "-c", defaultCodexReasoningConfig())
}

func defaultCodexTransport(args []string) string {
	if codexargs.IsInteractiveSubcommand(codexargs.Subcommand(args)) {
		return CodexTransportTUI
	}
	return CodexTransportExec
}

func ensureCodexYOLOArgs(args []string) []string {
	stripped := stripCodexPermissionArgs(args)
	withBypass := []string{defaultCodexBypassFlag}
	return append(withBypass, stripped...)
}

func defaultCodexReasoningConfig() string {
	return fmt.Sprintf(`model_reasoning_effort=%q`, defaultCodexReasoningEffort)
}

func hasClaudeEffortArg(args []string) bool {
	for _, arg := range args {
		if arg == "--effort" || strings.HasPrefix(arg, "--effort=") {
			return true
		}
	}
	return false
}

func hasClaudePermissionArg(args []string) bool {
	for _, arg := range args {
		switch {
		case arg == "--permission-mode",
			strings.HasPrefix(arg, "--permission-mode="),
			arg == "--dangerously-skip-permissions",
			arg == "--allow-dangerously-skip-permissions":
			return true
		}
	}
	return false
}

func hasCodexReasoningArg(args []string) bool {
	for i, arg := range args {
		if hasCodexReasoningConfig(arg) {
			return true
		}
		if (arg == "-c" || arg == "--config") && i+1 < len(args) && hasCodexReasoningConfig(args[i+1]) {
			return true
		}
	}
	return false
}

func hasCodexReasoningConfig(arg string) bool {
	return strings.Contains(arg, "model_reasoning_effort=") || strings.Contains(arg, "reasoning_effort=")
}

func stripCodexPermissionArgs(args []string) []string {
	stripped := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == defaultCodexBypassFlag, arg == "--full-auto":
			continue
		case arg == "-a" || arg == "--ask-for-approval" || arg == "-s" || arg == "--sandbox":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "--ask-for-approval="), strings.HasPrefix(arg, "--sandbox="):
			continue
		default:
			stripped = append(stripped, arg)
		}
	}

	return stripped
}

func validateCodexArgs(transport string, args []string) error {
	subcommand := codexargs.Subcommand(args)
	switch normalize(transport) {
	case CodexTransportTUI:
		if codexargs.IsInteractiveSubcommand(subcommand) {
			return nil
		}
		return fmt.Errorf("codex.transport %q does not support subcommand %q", transport, subcommand)
	case CodexTransportExec:
		if codexargs.IsNonInteractiveSubcommand(subcommand) {
			return nil
		}
		return fmt.Errorf("codex.transport %q requires a non-interactive subcommand such as exec or review", transport)
	default:
		return fmt.Errorf("unsupported codex.transport %q", transport)
	}
}

func validateStep(path string, step Step) error {
	if step.Name == "" {
		return fmt.Errorf("%s.name is required", path)
	}

	switch normalize(step.Type) {
	case StepTypeClaude:
		if strings.TrimSpace(step.Prompt) == "" {
			return fmt.Errorf("%s.prompt is required for claude steps", path)
		}
	case StepTypeCodex:
		if strings.TrimSpace(step.Prompt) == "" {
			return fmt.Errorf("%s.prompt is required for codex steps", path)
		}
	case StepTypeAgent:
		if strings.TrimSpace(step.Prompt) == "" {
			return fmt.Errorf("%s.prompt is required for agent steps", path)
		}
		switch normalize(step.Actor) {
		case StepActorCoder, StepActorReviewer:
		default:
			return fmt.Errorf("%s.actor %q is not supported", path, step.Actor)
		}
	case StepTypeExec:
		if len(step.Command) == 0 {
			return fmt.Errorf("%s.command is required for exec steps", path)
		}
	default:
		return fmt.Errorf("%s.type %q is not supported", path, step.Type)
	}

	return nil
}

func (c *Config) usesAgentSteps() bool {
	for _, step := range c.Steps {
		if normalize(step.Type) == StepTypeAgent {
			return true
		}
	}
	for _, workflow := range c.Workflows {
		for _, step := range workflow.Steps {
			if normalize(step.Type) == StepTypeAgent {
				return true
			}
		}
	}
	return false
}
