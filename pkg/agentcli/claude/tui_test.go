package claude

import "testing"

func TestClassifyTitle(t *testing.T) {
	tests := []struct {
		title string
		want  string
		ok    bool
	}{
		{title: "✳ Claude Code", want: "idle", ok: true},
		{title: "⠂ Claude Code", want: "busy", ok: true},
		{title: "✳ Implement THREAT_MODEL.md code changes", want: "idle", ok: true},
		{title: "⠂ Implement THREAT_MODEL.md code changes", want: "busy", ok: true},
		{title: "", want: "", ok: false},
	}

	for _, test := range tests {
		got, ok := classifyTitle(test.title)
		if ok != test.ok {
			t.Fatalf("title %q: expected ok=%v, got %v", test.title, test.ok, ok)
		}
		if got != test.want {
			t.Fatalf("title %q: expected %q, got %q", test.title, test.want, got)
		}
	}
}

func TestScreenStateTreatsClaudeTokenStatusLineAsBusy(t *testing.T) {
	tests := []struct {
		name   string
		screen string
	}{
		{
			name: "task title with token counter",
			screen: `● Write(ECSNet/include/ecsnet/net/session/session_codes.hpp)
  ⎿  Wrote 191 lines to ECSNet/include/ecsnet/net/session/session_codes.hpp

✶ Implement session wire schemas in include/ecsnet/net/session/**… (5m 12s · ↓ 19.3k tokens)

────────────────────────────────────────────────────────────────────────────────
❯ 
────────────────────────────────────────────────────────────────────────────────
  [001-ecsnet-wire-serialization-and-actions-95d0eff10f24]
  ⏵⏵ bypass permissions on (shift+tab to cycle)
`,
		},
		{
			name: "churning with changing token counter",
			screen: `✻ Churning… (22s · ↑ 809 tokens · thought for 3s)

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  [001-ecsnet-wire-serialization-and-actions-95d0eff10f24]
  ⏵⏵ bypass permissions on (shift+tab to cycle)
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, ok := ScreenState(tt.screen)
			if !ok || state != "busy" {
				t.Fatalf("expected Claude token status line to classify as busy, got state=%q ok=%v", state, ok)
			}
			if ReadyScreen(tt.screen) {
				t.Fatal("expected active token status line to suppress ready-screen detection")
			}
		})
	}
}
