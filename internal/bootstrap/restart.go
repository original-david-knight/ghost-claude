package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vibedrive/internal/config"
	"vibedrive/internal/plan"
	"vibedrive/internal/tasknotes"
)

func (i *Initializer) Restart(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.PlanFile) == "" {
		return fmt.Errorf("plan_file must be set in %s for restart", configPath)
	}

	currentPlan, err := plan.Load(cfg.PlanFile)
	if err != nil {
		return err
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(cfg.Workspace))
	if err != nil {
		return err
	}

	source, err := resolveRestartSources(cfg.Workspace, currentPlan.Project)
	if err != nil {
		return err
	}

	client, err := i.newClient(cfg, i.stdout, i.stderr)
	if err != nil {
		return err
	}

	session, err := i.newSession(config.SessionStrategySessionID)
	if err != nil {
		return err
	}

	defer func() {
		_ = client.Close(session)
	}()

	prompts := []string{
		renderRestartPlanPrompt(cfg, currentPlan, notesFile, source),
		renderRestartReviewPrompt(cfg, currentPlan, notesFile, source),
	}

	for _, prompt := range prompts {
		if err := client.RunPrompt(ctx, session, prompt); err != nil {
			return err
		}
	}

	restartedPlan, err := plan.Load(cfg.PlanFile)
	if err != nil {
		return err
	}
	restartedPlan.ResetProgress()
	if err := restartedPlan.Save(); err != nil {
		return err
	}
	if err := tasknotes.Remove(tasknotes.Path(cfg.Workspace)); err != nil {
		return err
	}

	fmt.Fprintf(i.stdout, "Prepared %s for a fresh restart\n", cfg.PlanFile)
	return nil
}

func renderRestartPlanPrompt(cfg *config.Config, currentPlan *plan.File, notesFile *tasknotes.File, source sourceSpec) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderRestartSourceRefs(cfg.Workspace, source.Files)
	notesSummary := renderRestartNotesSummary(currentPlan, notesFile)

	return strings.TrimSpace(fmt.Sprintf(`
Restart this project from scratch by revising the existing execution plan.

Read %s completely before editing it.

Also read these project source inputs when revising the plan:
%s

Analyze every note captured from the previous run before you change the plan:
%s

Rewrite %s in place so the next run avoids the difficulties surfaced above.

Requirements for the revised plan:
- preserve the hard constraints, success criteria, and source coverage from the existing plan and source inputs
- do not weaken or remove requirements from the source inputs
- incorporate the prior-run notes into better task decomposition, dependency ordering, context_files, acceptance criteria, verify_commands, and checkpoint placement
- add, split, reorder, or remove tasks when the previous run shows the old plan would repeat avoidable difficulty
- keep tasks sized for one focused implementation iteration and one coherent commit when practical
- keep workflow metadata and commit messages useful and specific
- by default, keep testing, verification, and cleanup work attached to the implementation task that introduces the change instead of deferring it to a later cleanup pass
- preserve or add a standalone implement tech-debt task only when the prior-run notes or planned work show unresolved follow-up testing, cleanup, hardening, or rollback-safety work tied to a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface
- describe those triggers from prior-run notes as discovered risks or expected breadth, not as proof that replanning can observe actual changed-file counts or other finalize-time facts unless a note recorded them explicitly
- when a standalone tech-debt task is justified, make the trigger explicit in the task details or acceptance criteria and scope the task to the follow-up work that the risk requires
- do not restore or add standalone tech-debt tasks on a fixed schedule or as generic placeholders when the work can stay inside the implementation task
- do not silently drop manual, machine-specific, or external-dependency work; represent it in tasks/details so the plan remains faithful to the source requirements
- include explicit checkpoint tasks wherever the requirements call for them
- strengthen weak or missing verify_commands whenever a deterministic automated check exists
- for every task, make the last acceptance item instruct the coding agent to leave short notes about what it learned in that phase, including discoveries or plan adjustments that matter if the plan is rerun from a fresh environment
- after incorporating the prior-run learnings, do not copy stale task notes from the previous run into the plan
- reset every task status to todo so the project can restart from a fresh environment
- keep valid YAML and quote any string list item that contains a colon followed by a space

After writing %s, quickly check that the YAML parses and that dependency ordering is coherent.
`, planRef, sourceRefs, notesSummary, planRef, planRef))
}

func renderRestartReviewPrompt(cfg *config.Config, currentPlan *plan.File, notesFile *tasknotes.File, source sourceSpec) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderRestartSourceRefs(cfg.Workspace, source.Files)
	notesSummary := renderRestartNotesSummary(currentPlan, notesFile)

	return strings.TrimSpace(fmt.Sprintf(`
Review the restarted execution plan in %s.

Project source inputs:
%s

Previous-run notes that must already be reflected in the new plan:
%s

Perform a critical review of the restarted plan. Focus on:
- prior-run notes that were ignored or only copied into notes instead of being turned into better tasks, dependencies, details, acceptance criteria, or verification
- task sequencing or missing preparatory work that would recreate the same difficulties
- tasks that are too large, vague, or not committable
- missing checkpoints or weak automated verification commands
- tasks that defer routine testing, verification, or cleanup work that should stay attached to implementation
- missing trigger-justified standalone tech-debt tasks when prior-run notes or planned work indicate unresolved follow-up testing, cleanup, hardening, or rollback-safety work for a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface
- wording that claims replanning can observe actual changed-file counts or other finalize-time facts instead of grounding follow-up work in prior notes, expected breadth, or discovered risk
- standalone tech-debt tasks, especially standalone cleanup or test-coverage tasks, that lack an explicit trigger, duplicate routine inline work, or still assume a fixed cadence instead of a risk-based follow-up
- tasks that do not end by capturing phase learnings for future replanning and fresh reruns
- statuses that were not reset to todo
- leftover task notes copied into the plan from the old run
- omitted or weakened requirements from the source inputs

Incorporate any actionable review feedback directly into %s.

Keep the YAML valid. Keep every task status at todo and do not write task notes into the plan.
`, planRef, sourceRefs, notesSummary, planRef))
}

func resolveRestartSources(workspace string, project plan.Project) (sourceSpec, error) {
	files := make([]string, 0, len(project.SourceDocs)+len(project.ConstraintFiles))
	seen := make(map[string]struct{}, len(project.SourceDocs)+len(project.ConstraintFiles))

	appendPath := func(ref string) error {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return nil
		}

		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		path = filepath.Clean(path)

		if _, ok := seen[path]; ok {
			return nil
		}

		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("resolve restart source %q: %w", ref, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restart source %q must be a regular file", ref)
		}

		seen[path] = struct{}{}
		files = append(files, path)
		return nil
	}

	for _, ref := range project.SourceDocs {
		if err := appendPath(ref); err != nil {
			return sourceSpec{}, err
		}
	}
	for _, ref := range project.ConstraintFiles {
		if err := appendPath(ref); err != nil {
			return sourceSpec{}, err
		}
	}

	return sourceSpec{Files: files}, nil
}

func renderRestartSourceRefs(workspace string, files []string) string {
	if len(files) == 0 {
		return "- No source docs are currently recorded in the plan; use the current plan file and any referenced local docs you discover while revising it."
	}
	return renderSourceRefs(workspace, files)
}

func renderRestartNotesSummary(currentPlan *plan.File, notesFile *tasknotes.File) string {
	lines := make([]string, 0, len(currentPlan.Tasks))
	seen := make(map[string]struct{}, len(currentPlan.Tasks))
	for _, task := range currentPlan.Tasks {
		seen[task.ID] = struct{}{}
		note := strings.TrimSpace(task.Notes)
		status := task.Status
		if noteEntry, ok := notesFile.Find(task.ID); ok {
			note = strings.TrimSpace(noteEntry.Notes)
			if strings.TrimSpace(noteEntry.Status) != "" {
				status = noteEntry.Status
			}
		}
		if note == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s [%s]: %s", task.ID, status, note))
	}

	for _, noteEntry := range notesFile.Tasks {
		if _, ok := seen[noteEntry.ID]; ok {
			continue
		}
		note := strings.TrimSpace(noteEntry.Notes)
		if note == "" {
			continue
		}
		status := strings.TrimSpace(noteEntry.Status)
		if status == "" {
			status = "unknown"
		}
		lines = append(lines, fmt.Sprintf("- %s [%s]: %s", noteEntry.ID, status, note))
	}

	if len(lines) == 0 {
		return "- No previous task notes are recorded."
	}

	return strings.Join(lines, "\n")
}
