package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type claudeScreenFixture struct {
	Name       string                   `json:"name"`
	Branch     string                   `json:"branch"`
	Source     string                   `json:"source"`
	Workdir    string                   `json:"workdir"`
	Title      string                   `json:"title"`
	ScreenFile string                   `json:"screen_file"`
	Want       claudeDetectorEvaluation `json:"want"`
}

type claudeDetectorEvaluation struct {
	ClassifyClaudeTmuxTitle stateResult        `json:"classifyClaudeTmuxTitle"`
	ClassifyTitle           stateResult        `json:"classifyTitle"`
	ClaudeTrustPrompt       bool               `json:"claudeTrustPrompt"`
	ClaudeReadyScreen       bool               `json:"claudeReadyScreen"`
	ClaudeScreenState       stateResult        `json:"claudeScreenState"`
	TitleMonitorSnapshot    snapshotEvaluation `json:"titleMonitorSnapshot"`
}

type stateResult struct {
	State string `json:"state"`
	OK    bool   `json:"ok"`
}

type snapshotEvaluation struct {
	IdleTransitions int `json:"idleTransitions"`
	BusyTransitions int `json:"busyTransitions"`
	TrustPrompts    int `json:"trustPrompts"`
}

func TestClaudeScreenGoldenFixtures(t *testing.T) {
	fixtures := loadClaudeScreenFixtures(t)
	seenBranches := map[string]bool{}

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			seenBranches[fixture.Branch] = true
			screen := readClaudeFixtureScreen(t, fixture.ScreenFile)
			got := evaluateClaudeDetectors(fixture, screen)
			assertClaudeDetectorEvaluation(t, fixture, got)
		})
	}

	for _, branch := range []string{
		"idle title",
		"busy title",
		"ready screen",
		"trust prompt",
		"stuck/ambiguous state",
	} {
		if !seenBranches[branch] {
			t.Fatalf("missing Claude detector fixture for branch %q", branch)
		}
	}
}

func loadClaudeScreenFixtures(t *testing.T) []claudeScreenFixture {
	t.Helper()

	const fixtureDir = "testdata/screens"
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read Claude screen fixtures: %v", err)
	}

	var fixtures []claudeScreenFixture
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(fixtureDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		var fixture claudeScreenFixture
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("parse fixture %s: %v", path, err)
		}
		if fixture.Name == "" || fixture.Branch == "" || fixture.Workdir == "" || fixture.ScreenFile == "" {
			t.Fatalf("fixture %s is missing required metadata: %#v", path, fixture)
		}
		fixtures = append(fixtures, fixture)
	}
	if len(fixtures) == 0 {
		t.Fatal("no Claude screen fixtures found")
	}
	return fixtures
}

func readClaudeFixtureScreen(t *testing.T, name string) string {
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

func evaluateClaudeDetectors(fixture claudeScreenFixture, screen string) claudeDetectorEvaluation {
	tmuxState, tmuxOK := classifyClaudeTmuxTitle(fixture.Title)
	titleState, titleOK := classifyTitle(fixture.Title)
	screenState, screenOK := ScreenState(screen)

	monitor := &titleMonitor{}
	monitor.Consume(append(claudeTitleChunk(fixture.Title), []byte(screen)...))
	snapshot := monitor.snapshot()

	return claudeDetectorEvaluation{
		ClassifyClaudeTmuxTitle: stateResult{
			State: tmuxState,
			OK:    tmuxOK,
		},
		ClassifyTitle: stateResult{
			State: titleState,
			OK:    titleOK,
		},
		ClaudeTrustPrompt: claudeTrustPrompt(screen),
		ClaudeReadyScreen: ReadyScreen(screen),
		ClaudeScreenState: stateResult{
			State: screenState,
			OK:    screenOK,
		},
		TitleMonitorSnapshot: snapshotEvaluation{
			IdleTransitions: snapshot.idleTransitions,
			BusyTransitions: snapshot.busyTransitions,
			TrustPrompts:    snapshot.trustPrompts,
		},
	}
}

func assertClaudeDetectorEvaluation(t *testing.T, fixture claudeScreenFixture, got claudeDetectorEvaluation) {
	t.Helper()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{name: "classifyClaudeTmuxTitle", got: got.ClassifyClaudeTmuxTitle, want: fixture.Want.ClassifyClaudeTmuxTitle},
		{name: "classifyTitle", got: got.ClassifyTitle, want: fixture.Want.ClassifyTitle},
		{name: "claudeTrustPrompt", got: got.ClaudeTrustPrompt, want: fixture.Want.ClaudeTrustPrompt},
		{name: "claudeReadyScreen", got: got.ClaudeReadyScreen, want: fixture.Want.ClaudeReadyScreen},
		{name: "claudeScreenState", got: got.ClaudeScreenState, want: fixture.Want.ClaudeScreenState},
		{name: "titleMonitorSnapshot", got: got.TitleMonitorSnapshot, want: fixture.Want.TitleMonitorSnapshot},
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

func claudeTitleChunk(title string) []byte {
	return []byte("\x1b]0;" + title + "\x07")
}
