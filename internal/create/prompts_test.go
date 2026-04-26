package create

import (
	"strings"
	"testing"
)

func TestSevenPromptDefinitionsExist(t *testing.T) {
	prompts := map[string]string{
		"ProductDefinitionAuthor":  ProductDefinitionAuthor,
		"ProductDefinitionCritic":  ProductDefinitionCritic,
		"UXReviewAuthor":           UXReviewAuthor,
		"UXReviewCritic":           UXReviewCritic,
		"TechnicalReviewAuthor":    TechnicalReviewAuthor,
		"TechnicalReviewCritic":    TechnicalReviewCritic,
		"AuthorFollowUpFromCritic": AuthorFollowUpFromCritic,
	}

	if len(prompts) != 7 {
		t.Fatalf("expected seven prompt definitions, got %d", len(prompts))
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
		"ask questions until",
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

func TestTechnicalReviewAuthorPromptCoversTechnicalDimensionsAndPlanBoundary(t *testing.T) {
	assertContainsAll(t, "TechnicalReviewAuthor", TechnicalReviewAuthor,
		"architecture",
		"data model and api contracts",
		"integration points",
		"implementation risks",
		"edge cases",
		"test strategy",
		"rollout/migration notes",
		"known unknowns",
		"rough implementation approach",
		"do not produce a detailed task-by-task plan",
		"detailed planning is reserved for vibedrive init",
	)
}

func TestCriticPromptsReadDesignAndDoNotEdit(t *testing.T) {
	criticPrompts := map[string]struct {
		prompt string
		stage  string
	}{
		"ProductDefinitionCritic": {prompt: ProductDefinitionCritic, stage: "product definition"},
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

func TestAuthorFollowUpPromptOwnsDesignDocument(t *testing.T) {
	assertContainsAll(t, "AuthorFollowUpFromCritic", AuthorFollowUpFromCritic,
		"fresh author",
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
