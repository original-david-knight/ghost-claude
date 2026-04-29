package codexargs

import "testing"

func TestSubcommandIgnoresGlobalFlags(t *testing.T) {
	got := Subcommand([]string{"--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="xhigh"`, "exec"})
	if got != "exec" {
		t.Fatalf("Subcommand = %q, want exec", got)
	}
}

func TestSubcommandKinds(t *testing.T) {
	if !IsInteractiveSubcommand("") || !IsInteractiveSubcommand("resume") || IsInteractiveSubcommand("exec") {
		t.Fatal("unexpected interactive subcommand classification")
	}
	if !IsNonInteractiveSubcommand("exec") || !IsNonInteractiveSubcommand("review") || IsNonInteractiveSubcommand("resume") {
		t.Fatal("unexpected non-interactive subcommand classification")
	}
}
