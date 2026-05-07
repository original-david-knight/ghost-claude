package diagnostics

import "time"

const (
	SchemaVersion = "diagnostics.v1"

	PaneLineLimit       = 3000
	PaneByteLimit       = 1024 * 1024
	TitleEventLimit     = 512
	TitleBytesLimit     = 512
	PromptByteLimit     = 256 * 1024
	ParentOutputLimit   = 64 * 1024
	ExecOutputLimit     = 256 * 1024
	JSONMetadataLimit   = 128 * 1024
	FailureMessageLimit = 4096
)

const (
	ArtifactStatusWritten       = "written"
	ArtifactStatusNotApplicable = "not_applicable"
	ArtifactStatusUnavailable   = "unavailable"
	ArtifactStatusFailed        = "failed"
)

const (
	ArtifactManifest       = "manifest"
	ArtifactParentStdout   = "parent_stdout_tail"
	ArtifactParentStderr   = "parent_stderr_tail"
	ArtifactPromptRaw      = "prompt_raw"
	ArtifactPromptNormal   = "prompt_normalized"
	ArtifactPromptMetadata = "prompt_metadata"
	ArtifactTmuxPane       = "tmux_pane"
	ArtifactTmuxTitles     = "tmux_title_history"
	ArtifactTmuxMetadata   = "tmux_metadata"
	ArtifactExecCommand    = "exec_command"
	ArtifactExecStdout     = "exec_stdout_tail"
	ArtifactExecStderr     = "exec_stderr_tail"
	ArtifactExecCombined   = "exec_combined_tail"
)

type Identity struct {
	RunID    string
	TaskID   string
	StepName string
}

type Failure struct {
	Path       string
	Message    string
	CapturedAt time.Time
}

type Transport struct {
	Kind        string `json:"kind"`
	Agent       string `json:"agent"`
	Interactive bool   `json:"interactive"`
}

type ByteArtifact struct {
	Available     bool
	Data          []byte
	OriginalBytes int64
	OriginalLines int
	SHA256        string
}

func Bytes(data []byte) ByteArtifact {
	return ByteArtifact{
		Available: true,
		Data:      append([]byte(nil), data...),
	}
}

func UnavailableBytes() ByteArtifact {
	return ByteArtifact{}
}

type PromptPayload struct {
	Raw               ByteArtifact
	Normalized        ByteArtifact
	NormalizationMode string
	BracketedPaste    bool
	ExtraMetadata     map[string]any
}

type TmuxCapture struct {
	Identity  Identity
	Failure   Failure
	Transport Transport

	Pane              ByteArtifact
	TitleHistory      []TitleEvent
	TitleHistoryKnown bool
	TitleHistoryError string
	Metadata          TmuxMetadata
	Prompt            PromptPayload
	ParentStdout      ByteArtifact
	ParentStderr      ByteArtifact
}

type ExecCapture struct {
	Identity  Identity
	Failure   Failure
	Transport Transport

	Command      ExecCommand
	Prompt       *PromptPayload
	Stdout       ByteArtifact
	Stderr       ByteArtifact
	Combined     ByteArtifact
	ParentStdout ByteArtifact
	ParentStderr ByteArtifact
}

type TitleEvent struct {
	TS              time.Time      `json:"ts"`
	Source          string         `json:"source"`
	Title           string         `json:"title,omitempty"`
	State           string         `json:"state,omitempty"`
	IdleTransitions int            `json:"idle_transitions,omitempty"`
	BusyTransitions int            `json:"busy_transitions,omitempty"`
	TrustPrompts    int            `json:"trust_prompts,omitempty"`
	Extra           map[string]any `json:"extra,omitempty"`
}

type TmuxMetadata struct {
	PaneTarget      string         `json:"pane_target,omitempty"`
	Agent           string         `json:"agent,omitempty"`
	Command         string         `json:"command,omitempty"`
	Args            []string       `json:"args,omitempty"`
	Workdir         string         `json:"workdir,omitempty"`
	StartupTimeout  string         `json:"startup_timeout,omitempty"`
	IdleTransitions int            `json:"idle_transitions,omitempty"`
	BusyTransitions int            `json:"busy_transitions,omitempty"`
	TrustPrompts    int            `json:"trust_prompts,omitempty"`
	FinalState      string         `json:"final_classified_state,omitempty"`
	TitleReadError  string         `json:"title_read_error,omitempty"`
	Extra           map[string]any `json:"extra,omitempty"`
}

type ExecCommand struct {
	Argv       []string          `json:"argv"`
	WorkingDir string            `json:"working_dir"`
	ExitCode   *int              `json:"exit_code,omitempty"`
	Signal     string            `json:"signal"`
	TimedOut   bool              `json:"timed_out"`
	Env        ExecEnvironment   `json:"env"`
	Redaction  RedactionMetadata `json:"redaction"`
	Extra      map[string]any    `json:"extra,omitempty"`
}

type ExecEnvironment struct {
	Step          map[string]string `json:"step,omitempty"`
	InheritedKeys []string          `json:"inherited_keys,omitempty"`
}

type RedactionMetadata struct {
	SensitiveNamePatterns []string `json:"sensitive_name_patterns"`
	ValuesRedacted        bool     `json:"values_redacted"`
}

type Result struct {
	Dir          string
	ManifestPath string
	Manifest     Manifest
}

type Manifest struct {
	SchemaVersion string               `json:"schema_version"`
	CreatedAt     time.Time            `json:"created_at"`
	RunID         string               `json:"run_id"`
	TaskID        string               `json:"task_id"`
	StepName      string               `json:"step_name"`
	PathSegments  ManifestPathSegments `json:"path_segments"`
	Failure       ManifestFailure      `json:"failure"`
	Transport     Transport            `json:"transport"`
	Artifacts     []ArtifactEntry      `json:"artifacts"`
}

type ManifestPathSegments struct {
	Run  string `json:"run"`
	Task string `json:"task"`
	Step string `json:"step"`
}

type ManifestFailure struct {
	Path       string    `json:"path"`
	Message    string    `json:"message"`
	CapturedAt time.Time `json:"captured_at"`
}

type ArtifactEntry struct {
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	MediaType      string `json:"media_type"`
	Status         string `json:"status"`
	Required       bool   `json:"required"`
	Bytes          *int64 `json:"bytes,omitempty"`
	Truncated      bool   `json:"truncated"`
	LimitBytes     int64  `json:"limit_bytes,omitempty"`
	LimitLines     int    `json:"limit_lines,omitempty"`
	LimitEvents    int    `json:"limit_events,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
	OriginalBytes  int64  `json:"original_bytes,omitempty"`
	OriginalLines  int    `json:"original_lines,omitempty"`
	OriginalEvents int    `json:"original_events,omitempty"`
	Error          string `json:"error,omitempty"`
	Reason         string `json:"reason,omitempty"`
}
