# Codex Screen Fixtures

These fixtures are tmux pane captures from real `codex` TUI runs when possible.
Each `screens/*.json` file stores the pane title, working directory, capture
source, and expected detector outputs. The adjacent `screens/*.txt` file is the
raw `tmux capture-pane -p -J` text replayed by the tests.

To capture a new fixture:

1. Start Codex in a fixed-size tmux pane, for example:
   `tmux new-session -d -s codex-fixture -x 120 -y 40 -c "$PWD" 'codex --dangerously-bypass-approvals-and-sandbox'`
2. Capture the title with:
   `tmux display-message -p -t codex-fixture '#{pane_title}'`
3. Capture the visible pane with:
   `tmux capture-pane -p -J -t codex-fixture -S -80`
4. Save the pane text as `screens/<name>.txt`, add a matching
   `screens/<name>.json`, then run `go test ./pkg/agentcli/codex`.

If the fixture comes from a Vibedrive failure bundle, use
`.vibedrive/debug/<run>/<task>/<step>/tmux/pane.txt` as the screen text and
take the final title from `tmux/title-history.jsonl` or `tmux/metadata.json`.
If only task notes remain, a reconstructed TUI-style stuck fixture is acceptable
when the source field explains what real failure evidence it came from.

For prompt-submission captures, `tmux send-keys -l '<prompt>'` followed by
`tmux send-keys C-m` most closely matches what Codex accepted during fixture
capture.
