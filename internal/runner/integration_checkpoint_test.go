package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/automation"
	"vibedrive/internal/config"
	"vibedrive/internal/diagnostics"
	"vibedrive/internal/plan"
	"vibedrive/internal/tasknotes"
)

func TestIntegrationCheckpointQualityRecoveryEndToEnd(t *testing.T) {
	t.Run("rust shaped project succeeds with artifacts present", func(t *testing.T) {
		result := runCheckpointProject(t, checkpointProjectOptions{
			Shape:         "rust",
			TaskID:        "rust-iteration",
			VerifyCommand: "test -f src/generated.rs",
		})
		if result.Err != nil {
			t.Fatalf("Run returned error: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
		}
		assertCheckpointTaskStatus(t, result.Workspace, "rust-iteration", plan.StatusDone)
		assertFinalizeStayedQuiet(t, result)
		assertNoBrokenPipe(t, result)
		assertBuildArtifactsNotCommitted(t, result.Workspace)
	})

	t.Run("non rust shaped project succeeds", func(t *testing.T) {
		result := runCheckpointProject(t, checkpointProjectOptions{
			Shape:         "non-rust",
			TaskID:        "ecsnet-iteration",
			VerifyCommand: "test -f src/generated.js",
		})
		if result.Err != nil {
			t.Fatalf("Run returned error: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
		}
		assertCheckpointTaskStatus(t, result.Workspace, "ecsnet-iteration", plan.StatusDone)
		assertFinalizeStayedQuiet(t, result)
		assertNoBrokenPipe(t, result)
		assertBuildArtifactsNotCommitted(t, result.Workspace)
	})

	t.Run("rust shaped project intentional failure captures diagnostics", func(t *testing.T) {
		result := runCheckpointProject(t, checkpointProjectOptions{
			Shape:         "rust",
			TaskID:        "rust-failure",
			VerifyCommand: "echo intentional rust verify failure >&2; exit 42",
		})
		if result.Err == nil {
			t.Fatal("expected intentional verify failure")
		}
		assertNoBrokenPipe(t, result)
		assertCheckpointTaskStatus(t, result.Workspace, "rust-failure", plan.StatusInProgress)
		assertCheckpointExecDiagnostics(t, result.Workspace, "rust-failure", "finalize-task", "intentional rust verify failure")
	})

	t.Run("non rust shaped project intentional failure captures diagnostics", func(t *testing.T) {
		result := runCheckpointProject(t, checkpointProjectOptions{
			Shape:         "non-rust",
			TaskID:        "ecsnet-failure",
			VerifyCommand: "echo intentional ecsnet verify failure >&2; exit 43",
		})
		if result.Err == nil {
			t.Fatal("expected intentional verify failure")
		}
		assertNoBrokenPipe(t, result)
		assertCheckpointTaskStatus(t, result.Workspace, "ecsnet-failure", plan.StatusInProgress)
		assertCheckpointExecDiagnostics(t, result.Workspace, "ecsnet-failure", "finalize-task", "intentional ecsnet verify failure")
	})
}

func TestIntegrationCheckpointQualityRecoveryFinalizeHelper(t *testing.T) {
	if os.Getenv("VIBEDRIVE_FINALIZE_HELPER") != "1" {
		return
	}

	args := argsAfterDoubleDash(os.Args)
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "missing helper args")
		os.Exit(2)
	}
	if err := runFinalizeHelper(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type checkpointProjectOptions struct {
	Shape         string
	TaskID        string
	VerifyCommand string
}

type checkpointProjectResult struct {
	Workspace string
	Stdout    string
	Stderr    string
	Err       error
}

func runCheckpointProject(t *testing.T, opts checkpointProjectOptions) checkpointProjectResult {
	t.Helper()

	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")
	writeCheckpointSeedProject(t, seed, opts.Shape)
	initRunnerGitRepo(t, seed)
	runRunnerCmd(t, root, "git", "clone", seed, checkout)
	writeCheckpointBuildArtifacts(t, checkout, opts.Shape)

	planPath := filepath.Join(checkout, "vibedrive-plan.yaml")
	writeCheckpointPlan(t, planPath, opts.TaskID, opts.VerifyCommand)
	fakeCodex, fakeClaude := writeCheckpointFakeAgents(t, filepath.Join(root, "bin"))

	cfg := checkpointRunnerConfig(checkout, planPath, fakeCodex, fakeClaude)
	r, err := New(cfg, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stdout := r.stdout.(*bytes.Buffer)
	stderr := r.stderr.(*bytes.Buffer)
	r.executablePath = os.Args[0]

	err = r.Run(context.Background())
	return checkpointProjectResult{
		Workspace: checkout,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Err:       err,
	}
}

func checkpointRunnerConfig(workspace, planPath, fakeCodex, fakeClaude string) *config.Config {
	return &config.Config{
		Path:                 filepath.Join(workspace, "vibedrive.yaml"),
		Workspace:            workspace,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Claude: config.ClaudeConfig{
			Command:         fakeClaude,
			Args:            []string{"--test-agent"},
			Transport:       config.ClaudeTransportPrint,
			StartupTimeout:  "2s",
			SessionStrategy: config.SessionStrategySessionID,
		},
		Codex: config.CodexConfig{
			Command:        fakeCodex,
			Version:        "codex-test 1.0",
			Args:           []string{"--test-agent"},
			Transport:      config.CodexTransportExec,
			StartupTimeout: "2s",
		},
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "execute-task",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorCoder,
						Prompt:          "WRITE_RESULT={{ .TaskResultPath }}\nTASK={{ .Task.ID }}\n",
						RequiredOutputs: []string{"{{ .TaskResultPath }}"},
					},
					{
						Name:            "peer-review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorReviewer,
						Prompt:          "WRITE_REVIEW={{ .ReviewPath }}\n",
						RequiredOutputs: []string{"{{ .ReviewPath }}"},
					},
					{
						Name:            "address-peer-review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorCoder,
						Prompt:          "ADDRESS_RESULT={{ .TaskResultPath }}\nREVIEW={{ .ReviewPath }}\n",
						RequiredOutputs: []string{"{{ .TaskResultPath }}"},
					},
					{
						Name: "finalize-task",
						Type: config.StepTypeExec,
						Command: []string{
							"{{ .ExecutablePath }}",
							"-test.run=TestIntegrationCheckpointQualityRecoveryFinalizeHelper",
							"--",
							"task",
							"finalize",
							"--workspace",
							"{{ .Workspace }}",
							"--plan",
							"{{ .PlanFile }}",
							"--task",
							"{{ .Task.ID }}",
							"--result",
							"{{ .TaskResultPath }}",
							"--message",
							"{{ .Task.Title }}",
						},
						Env: map[string]string{
							"VIBEDRIVE_FINALIZE_HELPER": "1",
						},
						Timeout: "10s",
					},
				},
			},
		},
	}
}

func writeCheckpointSeedProject(t *testing.T, dir, shape string) {
	t.Helper()

	switch shape {
	case "rust":
		writeCheckpointFile(t, filepath.Join(dir, ".gitignore"), "/target/\n")
		writeCheckpointFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname = \"meetspace-shape\"\nversion = \"0.1.0\"\nedition = \"2021\"\n")
		writeCheckpointFile(t, filepath.Join(dir, "src", "lib.rs"), "pub fn ready() -> bool { true }\n")
	case "non-rust":
		writeCheckpointFile(t, filepath.Join(dir, ".gitignore"), "/node_modules/\n/__pycache__/\n")
		writeCheckpointFile(t, filepath.Join(dir, "package.json"), "{\"name\":\"ecsnet-shape\",\"version\":\"0.1.0\"}\n")
		writeCheckpointFile(t, filepath.Join(dir, "src", "index.js"), "export const ready = true;\n")
	default:
		t.Fatalf("unsupported checkpoint project shape %q", shape)
	}
}

func writeCheckpointBuildArtifacts(t *testing.T, workspace, shape string) {
	t.Helper()

	switch shape {
	case "rust":
		for i := range 300 {
			writeCheckpointFile(t, filepath.Join(workspace, "target", "debug", fmt.Sprintf("artifact-%03d.o", i)), "artifact\n")
		}
		writeCheckpointFile(t, filepath.Join(workspace, ".vibedrive", "cargo-target", "artifact.txt"), "artifact\n")
		writeCheckpointFile(t, filepath.Join(workspace, "crates", "foo", "target", "artifact.txt"), "artifact\n")
	case "non-rust":
		for i := range 50 {
			writeCheckpointFile(t, filepath.Join(workspace, "node_modules", "cache", fmt.Sprintf("artifact-%03d.js", i)), "artifact\n")
		}
		writeCheckpointFile(t, filepath.Join(workspace, ".vibedrive", "node-cache", "artifact.txt"), "artifact\n")
		writeCheckpointFile(t, filepath.Join(workspace, "services", "web", "node_modules", "artifact.js"), "artifact\n")
		writeCheckpointFile(t, filepath.Join(workspace, "services", "api", "__pycache__", "artifact.pyc"), "artifact\n")
	}
}

func writeCheckpointPlan(t *testing.T, path, taskID, verifyCommand string) {
	t.Helper()

	content := fmt.Sprintf(`project:
  name: checkpoint-e2e
tasks:
  - id: %s
    title: Checkpoint E2E %s
    workflow: implement
    status: todo
    verify_commands:
      - %s
`, taskID, taskID, verifyCommand)
	writeCheckpointFile(t, path, content)
}

func writeCheckpointFakeAgents(t *testing.T, binDir string) (string, string) {
	t.Helper()

	codexPath := filepath.Join(binDir, "fake-codex")
	claudePath := filepath.Join(binDir, "fake-claude")
	writeCheckpointFileMode(t, codexPath, `#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then
  echo "codex-test 1.0"
  exit 0
fi
if [ "${1:-}" != "exec" ]; then
  echo "expected codex exec, got: $*" >&2
  exit 64
fi
shift
prompt=$(cat)
result_path=$(printf '%s\n' "$prompt" | sed -n 's/^WRITE_RESULT=//p' | head -n 1)
if [ -z "$result_path" ]; then
  result_path=$(printf '%s\n' "$prompt" | sed -n 's/^ADDRESS_RESULT=//p' | head -n 1)
fi
if [ -n "$result_path" ]; then
  if [ -f Cargo.toml ]; then
    mkdir -p src
    printf 'pub const GENERATED: &str = "meetspace";\n' > src/generated.rs
  elif [ -f package.json ]; then
    mkdir -p src
    printf 'export const generated = "ecsnet";\n' > src/generated.js
  fi
  mkdir -p "$(dirname "$result_path")"
  printf '{"status":"done","notes":"checkpoint fake codex completed"}\n' > "$result_path"
fi
echo "codex exec completed"
`, 0o755)
	writeCheckpointFileMode(t, claudePath, `#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then
  echo "claude-test 1.0"
  exit 0
fi
prompt=$(cat)
review_path=$(printf '%s\n' "$prompt" | sed -n 's/^WRITE_REVIEW=//p' | head -n 1)
if [ -n "$review_path" ]; then
  mkdir -p "$(dirname "$review_path")"
  printf '{"decision":"approved","summary":"checkpoint fake review approved","findings":[]}\n' > "$review_path"
fi
echo "claude print completed"
`, 0o755)
	return codexPath, claudePath
}

func writeCheckpointFile(t *testing.T, path, content string) {
	t.Helper()
	writeCheckpointFileMode(t, path, content, 0o644)
}

func writeCheckpointFileMode(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) returned error: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("WriteFile(%q) returned error: %v", path, err)
	}
}

func assertCheckpointTaskStatus(t *testing.T, workspace, taskID, want string) {
	t.Helper()

	file, err := plan.Load(filepath.Join(workspace, "vibedrive-plan.yaml"))
	if err != nil {
		t.Fatalf("Load plan returned error: %v", err)
	}
	task, ok := file.FindTask(taskID)
	if !ok {
		t.Fatalf("task %q not found", taskID)
	}
	if task.Status != want {
		t.Fatalf("task %q status = %q, want %q", taskID, task.Status, want)
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(workspace))
	if err != nil {
		t.Fatalf("Load task notes returned error: %v", err)
	}
	note, ok := notesFile.Find(taskID)
	if !ok {
		t.Fatalf("task notes for %q not found", taskID)
	}
	if note.Status != want {
		t.Fatalf("task notes status = %q, want %q", note.Status, want)
	}
}

func assertFinalizeStayedQuiet(t *testing.T, result checkpointProjectResult) {
	t.Helper()

	output := result.Stdout + result.Stderr
	for _, noisy := range []string{"files changed", "create mode target/", "[master", "[main"} {
		if strings.Contains(output, noisy) {
			t.Fatalf("finalize output was not quiet; found %q in:\n%s", noisy, output)
		}
	}
}

func assertNoBrokenPipe(t *testing.T, result checkpointProjectResult) {
	t.Helper()

	output := result.Stdout + result.Stderr
	if result.Err != nil {
		output += "\n" + result.Err.Error()
	}
	if strings.Contains(strings.ToLower(output), "broken pipe") {
		t.Fatalf("unexpected broken-pipe failure:\n%s", output)
	}
}

func assertBuildArtifactsNotCommitted(t *testing.T, workspace string) {
	t.Helper()

	tree := runRunnerCmd(t, workspace, "git", "-C", workspace, "ls-tree", "-r", "HEAD", "--name-only")
	for _, excluded := range []string{
		"target/",
		".vibedrive/",
		"crates/foo/target/",
		"node_modules/",
		"services/web/node_modules/",
		"services/api/__pycache__/",
	} {
		if strings.Contains(tree, excluded) {
			t.Fatalf("transient build artifact %s was committed:\n%s", excluded, tree)
		}
	}

	var requiredArtifacts []string
	if _, err := os.Stat(filepath.Join(workspace, "Cargo.toml")); err == nil {
		requiredArtifacts = []string{
			".vibedrive/cargo-target/artifact.txt",
			"target/debug/artifact-000.o",
			"crates/foo/target/artifact.txt",
		}
	} else {
		requiredArtifacts = []string{
			".vibedrive/node-cache/artifact.txt",
			"node_modules/cache/artifact-000.js",
			"services/web/node_modules/artifact.js",
			"services/api/__pycache__/artifact.pyc",
		}
	}
	for _, path := range requiredArtifacts {
		if _, err := os.Stat(filepath.Join(workspace, path)); err != nil {
			t.Fatalf("expected transient artifact %s to remain in the working tree: %v", path, err)
		}
	}
}

func assertCheckpointExecDiagnostics(t *testing.T, workspace, taskID, stepName, outputFragment string) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(workspace, ".vibedrive", "debug", "*", taskID, stepName, "manifest.json"))
	if err != nil {
		t.Fatalf("Glob diagnostics manifest returned error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one diagnostics manifest for %s/%s, got %d: %v", taskID, stepName, len(matches), matches)
	}

	var manifest diagnostics.Manifest
	readRunnerJSONFile(t, matches[0], &manifest)
	if manifest.SchemaVersion != diagnostics.SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", manifest.SchemaVersion, diagnostics.SchemaVersion)
	}
	if manifest.TaskID != taskID || manifest.StepName != stepName || strings.TrimSpace(manifest.RunID) == "" {
		t.Fatalf("unexpected manifest identity: %#v", manifest)
	}
	if manifest.Failure.Path != "exec_step_non_zero_exit" {
		t.Fatalf("failure path = %q, want exec_step_non_zero_exit", manifest.Failure.Path)
	}
	if manifest.Transport.Kind != "exec" || manifest.Transport.Agent != "" || manifest.Transport.Interactive {
		t.Fatalf("unexpected transport: %#v", manifest.Transport)
	}

	stepDir := filepath.Dir(matches[0])
	for _, kind := range []string{
		diagnostics.ArtifactExecCommand,
		diagnostics.ArtifactExecStdout,
		diagnostics.ArtifactExecStderr,
		diagnostics.ArtifactExecCombined,
		diagnostics.ArtifactParentStdout,
		diagnostics.ArtifactParentStderr,
		diagnostics.ArtifactManifest,
	} {
		entry := runnerArtifactEntry(t, manifest, kind)
		if entry.Status != diagnostics.ArtifactStatusWritten || !entry.Required {
			t.Fatalf("artifact %s = %#v, want required written", kind, entry)
		}
		if entry.Path == "" || filepath.IsAbs(entry.Path) {
			t.Fatalf("artifact %s has invalid relative path %#v", kind, entry)
		}
		if _, err := os.Stat(filepath.Join(stepDir, entry.Path)); err != nil {
			t.Fatalf("artifact %s file missing: %v", kind, err)
		}
	}
	for _, kind := range []string{
		diagnostics.ArtifactPromptRaw,
		diagnostics.ArtifactPromptNormal,
		diagnostics.ArtifactPromptMetadata,
		diagnostics.ArtifactTmuxPane,
		diagnostics.ArtifactTmuxTitles,
		diagnostics.ArtifactTmuxMetadata,
	} {
		entry := runnerArtifactEntry(t, manifest, kind)
		if entry.Status != diagnostics.ArtifactStatusNotApplicable || entry.Required {
			t.Fatalf("artifact %s = %#v, want not_applicable", kind, entry)
		}
	}

	var command diagnostics.ExecCommand
	readRunnerJSONFile(t, filepath.Join(stepDir, "exec", "command.json"), &command)
	if command.WorkingDir != workspace {
		t.Fatalf("diagnostics working_dir = %q, want %q", command.WorkingDir, workspace)
	}
	if command.ExitCode == nil || *command.ExitCode != 1 {
		t.Fatalf("diagnostics exit_code = %#v, want 1", command.ExitCode)
	}
	combined := mustReadRunnerFile(t, filepath.Join(stepDir, "exec", "combined-tail.txt"))
	if !strings.Contains(combined, outputFragment) {
		t.Fatalf("combined diagnostics did not contain %q:\n%s", outputFragment, combined)
	}
}

func argsAfterDoubleDash(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func runFinalizeHelper(args []string) error {
	if len(args) < 2 || args[0] != "task" || args[1] != "finalize" {
		return fmt.Errorf("expected task finalize helper args, got %q", strings.Join(args, " "))
	}

	var opts automation.FinalizeOptions
	for i := 2; i < len(args); i++ {
		if i+1 >= len(args) {
			return fmt.Errorf("missing value for %s", args[i])
		}
		value := args[i+1]
		switch args[i] {
		case "--workspace":
			opts.Workspace = value
		case "--plan":
			opts.PlanFile = value
		case "--task":
			opts.TaskID = value
		case "--result":
			opts.ResultPath = value
		case "--message":
			opts.CommitMessage = value
		default:
			return fmt.Errorf("unsupported finalize helper flag %q", args[i])
		}
		i++
	}

	if strings.TrimSpace(opts.Workspace) == "" ||
		strings.TrimSpace(opts.PlanFile) == "" ||
		strings.TrimSpace(opts.TaskID) == "" ||
		strings.TrimSpace(opts.ResultPath) == "" {
		return fmt.Errorf("missing required finalize helper option: %#v", opts)
	}
	return automation.Finalize(context.Background(), opts, os.Stdout, os.Stderr)
}
