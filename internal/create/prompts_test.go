package create

import (
	"strings"
	"testing"
)

func TestNinePromptDefinitionsExist(t *testing.T) {
	prompts := map[string]string{
		"ProductDefinitionAuthor":  ProductDefinitionAuthor,
		"ProductDefinitionCritic":  ProductDefinitionCritic,
		"FeatureRefactorAuthor":    FeatureRefactorAuthor,
		"FeatureRefactorCritic":    FeatureRefactorCritic,
		"UXReviewAuthor":           UXReviewAuthor,
		"UXReviewCritic":           UXReviewCritic,
		"TechnicalReviewAuthor":    TechnicalReviewAuthor,
		"TechnicalReviewCritic":    TechnicalReviewCritic,
		"AuthorFollowUpFromCritic": AuthorFollowUpFromCritic,
	}

	if len(prompts) != 9 {
		t.Fatalf("expected nine prompt definitions, got %d", len(prompts))
	}
	for name, prompt := range prompts {
		if strings.TrimSpace(prompt) == "" {
			t.Fatalf("expected %s prompt to be non-empty", name)
		}
	}
}

func TestAuthorPromptsInspectWorkspaceDesignAndWriteDesign(t *testing.T) {
	authorPrompts := map[string]string{
		"ProductDefinitionAuthor": ProductDefinitionAuthor,
		"FeatureRefactorAuthor":   FeatureRefactorAuthor,
		"UXReviewAuthor":          UXReviewAuthor,
		"TechnicalReviewAuthor":   TechnicalReviewAuthor,
	}

	for name, prompt := range authorPrompts {
		assertContainsAll(t, name, prompt,
			"inspect the workspace",
			"root design.md",
			"if present",
			"before editing",
			"write or update the root design.md",
			"before stopping",
		)
	}
}

func TestProductDefinitionAuthorPromptCoversInterviewAndPushback(t *testing.T) {
	assertContainsAll(t, "ProductDefinitionAuthor", ProductDefinitionAuthor,
		"agent tui",
		"ask questions one at a time until",
		"requirements are ready for the next stage",
		"push back",
		"product problems",
		"contradictions",
		"over-broad scope",
		"unclear users",
		"missing workflows",
		"weak success criteria",
		"do not stop after a fixed checklist",
		"important questions remain",
	)
}

func TestFeatureRefactorAuthorPromptCoversExistingProjectChanges(t *testing.T) {
	assertContainsAll(t, "FeatureRefactorAuthor", FeatureRefactorAuthor,
		"existing project",
		"new feature",
		"refactoring existing behavior",
		"current behavior that must be preserved",
		"compatibility constraints",
		"rollout or migration needs",
		"refactor boundary",
		"current codebase",
		"relevant files",
		"integration points",
		"risky coupling",
		"too broad",
		"measurable acceptance criteria",
	)
}

func TestUXReviewAuthorPromptCoversDesignDimensions(t *testing.T) {
	assertContainsAll(t, "UXReviewAuthor", UXReviewAuthor,
		"without forcing all",
		"user journeys and workflows",
		"interaction design",
		"visual style",
		"layout and responsive behavior",
		"accessibility",
		"empty/loading/error states",
		"content and terminology",
		"scope tradeoffs from a product/design perspective",
	)
}

func TestAuthorPromptsRequireAgentSelfVerification(t *testing.T) {
	authorPrompts := map[string]string{
		"ProductDefinitionAuthor": ProductDefinitionAuthor,
		"FeatureRefactorAuthor":   FeatureRefactorAuthor,
		"UXReviewAuthor":          UXReviewAuthor,
		"TechnicalReviewAuthor":   TechnicalReviewAuthor,
	}

	for name, prompt := range authorPrompts {
		assertContainsAll(t, name, prompt,
			"verify",
			"without manual help",
			"instrumentation",
		)
	}

	assertContainsAll(t, "UXReviewAuthor", UXReviewAuthor,
		"scripted screenshots",
		"accessibility checks",
	)
	assertContainsAll(t, "FeatureRefactorAuthor", FeatureRefactorAuthor,
		"migration checks",
		"fixtures",
	)
	assertContainsAll(t, "TechnicalReviewAuthor", TechnicalReviewAuthor,
		"screenshot capture",
		"fixtures",
	)
}

func TestTechnicalReviewAuthorPromptCoversTechnicalDimensionsAndPlanBoundary(t *testing.T) {
	assertContainsAll(t, "TechnicalReviewAuthor", TechnicalReviewAuthor,
		"architecture",
		"data model and api contracts",
		"integration points",
		"implementation risks",
		"edge cases",
		"test strategy",
		"verification instrumentation",
		"rollout/migration notes",
		"known unknowns",
		"rough implementation approach",
		"do not produce a detailed task-by-task plan",
		"detailed planning is reserved for later",
	)
}

func TestCriticPromptsReadDesignAndDoNotEdit(t *testing.T) {
	criticPrompts := map[string]struct {
		prompt string
		stage  string
	}{
		"ProductDefinitionCritic": {prompt: ProductDefinitionCritic, stage: "product definition"},
		"FeatureRefactorCritic":   {prompt: FeatureRefactorCritic, stage: "feature or refactoring"},
		"UXReviewCritic":          {prompt: UXReviewCritic, stage: "ux review"},
		"TechnicalReviewCritic":   {prompt: TechnicalReviewCritic, stage: "technical review"},
	}

	for name, prompt := range criticPrompts {
		assertContainsAll(t, name, prompt.prompt,
			"read the workspace",
			"design.md",
			"critical second opinion",
			prompt.stage,
			"do not edit design.md",
		)
	}
}

func TestCriticPromptsReviewAgentSelfVerification(t *testing.T) {
	assertContainsAll(t, "ProductDefinitionCritic", ProductDefinitionCritic,
		"verify the intended outcome without manual help",
	)
	assertContainsAll(t, "FeatureRefactorCritic", FeatureRefactorCritic,
		"current behavior",
		"compatibility",
		"clear change boundary",
		"verify the intended outcome without manual help",
	)
	assertContainsAll(t, "UXReviewCritic", UXReviewCritic,
		"agent-verifiable ui evidence",
		"scripted screenshots",
	)
	assertContainsAll(t, "TechnicalReviewCritic", TechnicalReviewCritic,
		"verification instrumentation",
		"screenshot capture",
		"verify their own work without manual help",
	)
}

func TestAuthorFollowUpPromptOwnsDesignDocument(t *testing.T) {
	assertContainsAll(t, "AuthorFollowUpFromCritic", AuthorFollowUpFromCritic,
		"design.md author instance",
		"read the critic feedback",
		"decide what to do with the feedback",
		"update design.md",
		"keeping ownership of the document",
	)
}

func assertContainsAll(t *testing.T, name, prompt string, wants ...string) {
	t.Helper()

	normalized := strings.ToLower(prompt)
	for _, want := range wants {
		if !strings.Contains(normalized, strings.ToLower(want)) {
			t.Fatalf("expected %s to contain %q, got %q", name, want, prompt)
		}
	}
}
