package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type codexScreenFixture struct {
	Name       string                  `json:"name"`
	Branch     string                  `json:"branch"`
	Source     string                  `json:"source"`
	Workdir    string                  `json:"workdir"`
	Title      string                  `json:"title"`
	ScreenFile string                  `json:"screen_file"`
	Want       codexDetectorEvaluation `json:"want"`
}

type codexDetectorEvaluation struct {
	ClassifyTitle    stateResult `json:"classifyTitle"`
	IsBusyTitle      bool        `json:"isBusyTitle"`
	TitleMatchesIdle bool        `json:"titleMatchesIdle"`
	CodexReadyScreen bool        `json:"codexReadyScreen"`
	CodexTrustPrompt bool        `json:"codexTrustPrompt"`
	CodexScreenState stateResult `json:"codexScreenState"`
}

type stateResult struct {
	State string `json:"state"`
	OK    bool   `json:"ok"`
}

func TestCodexScreenGoldenFixtures(t *testing.T) {
	fixtures := loadCodexScreenFixtures(t)
	seenBranches := map[string]bool{}
	screenFileBranches := map[string]string{}

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			seenBranches[fixture.Branch] = true
			if previousBranch, ok := screenFileBranches[fixture.ScreenFile]; ok && previousBranch != fixture.Branch {
				t.Fatalf("screen fixture %q is shared by branches %q and %q; per-branch fixtures must use distinct screen evidence", fixture.ScreenFile, previousBranch, fixture.Branch)
			}
			screenFileBranches[fixture.ScreenFile] = fixture.Branch
			screen := readCodexFixtureScreen(t, fixture.ScreenFile)
			got := evaluateCodexDetectors(fixture, screen)
			assertCodexDetectorEvaluation(t, fixture, got)
		})
	}

	for _, branch := range []string{
		"idle title",
		"busy title",
		"ready screen",
		"trust prompt",
		"working screen",
		"stuck/ambiguous screen",
	} {
		if !seenBranches[branch] {
			t.Fatalf("missing Codex detector fixture for branch %q", branch)
		}
	}
}

func loadCodexScreenFixtures(t *testing.T) []codexScreenFixture {
	t.Helper()

	const fixtureDir = "testdata/screens"
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read Codex screen fixtures: %v", err)
	}

	var fixtures []codexScreenFixture
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(fixtureDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		var fixture codexScreenFixture
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("parse fixture %s: %v", path, err)
		}
		if fixture.Name == "" || fixture.Branch == "" || fixture.Source == "" || fixture.Workdir == "" || fixture.ScreenFile == "" {
			t.Fatalf("fixture %s is missing required metadata: %#v", path, fixture)
		}
		fixtures = append(fixtures, fixture)
	}
	if len(fixtures) == 0 {
		t.Fatal("no Codex screen fixtures found")
	}
	return fixtures
}

func readCodexFixtureScreen(t *testing.T, name string) string {
	t.Helper()

	if filepath.Base(name) != name {
		t.Fatalf("fixture screen_file must be a basename, got %q", name)
	}
	raw, err := os.ReadFile(filepath.Join("testdata/screens", name))
	if err != nil {
		t.Fatalf("read fixture screen %s: %v", name, err)
	}
	return string(raw)
}

func evaluateCodexDetectors(fixture codexScreenFixture, screen string) codexDetectorEvaluation {
	monitor := newTitleMonitor(fixture.Workdir)
	titleState, titleOK := monitor.classifyTitle(fixture.Title)
	screenState, screenOK := codexScreenState(screen)

	return codexDetectorEvaluation{
		ClassifyTitle: stateResult{
			State: titleState,
			OK:    titleOK,
		},
		IsBusyTitle:      isBusyTitle(fixture.Title),
		TitleMatchesIdle: titleMatchesIdle(fixture.Title, monitor.idleTitle),
		CodexReadyScreen: codexReadyScreen(screen),
		CodexTrustPrompt: codexTrustPrompt(screen),
		CodexScreenState: stateResult{
			State: screenState,
			OK:    screenOK,
		},
	}
}

func assertCodexDetectorEvaluation(t *testing.T, fixture codexScreenFixture, got codexDetectorEvaluation) {
	t.Helper()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{name: "classifyTitle", got: got.ClassifyTitle, want: fixture.Want.ClassifyTitle},
		{name: "isBusyTitle", got: got.IsBusyTitle, want: fixture.Want.IsBusyTitle},
		{name: "titleMatchesIdle", got: got.TitleMatchesIdle, want: fixture.Want.TitleMatchesIdle},
		{name: "codexReadyScreen", got: got.CodexReadyScreen, want: fixture.Want.CodexReadyScreen},
		{name: "codexTrustPrompt", got: got.CodexTrustPrompt, want: fixture.Want.CodexTrustPrompt},
		{name: "codexScreenState", got: got.CodexScreenState, want: fixture.Want.CodexScreenState},
	}

	for _, check := range checks {
		if reflect.DeepEqual(check.got, check.want) {
			continue
		}
		t.Errorf("%s fixture %q changed %s classification\nsource: %s\n%s",
			fixture.Branch,
			fixture.Name,
			check.name,
			fixture.Source,
			lineDiff(mustPrettyJSON(check.want), mustPrettyJSON(check.got)),
		)
	}
}

func mustPrettyJSON(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func lineDiff(want, got string) string {
	if want == got {
		return ""
	}

	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	var b strings.Builder
	b.WriteString("--- want\n+++ got\n")
	maxLines := len(wantLines)
	if len(gotLines) > maxLines {
		maxLines = len(gotLines)
	}
	for i := 0; i < maxLines; i++ {
		var wantLine, gotLine string
		wantOK := i < len(wantLines)
		gotOK := i < len(gotLines)
		if wantOK {
			wantLine = wantLines[i]
		}
		if gotOK {
			gotLine = gotLines[i]
		}
		if wantOK && gotOK && wantLine == gotLine {
			fmt.Fprintf(&b, "  %s\n", wantLine)
			continue
		}
		if wantOK {
			fmt.Fprintf(&b, "- %s\n", wantLine)
		}
		if gotOK {
			fmt.Fprintf(&b, "+ %s\n", gotLine)
		}
	}
	return b.String()
}
