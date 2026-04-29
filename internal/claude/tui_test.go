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
