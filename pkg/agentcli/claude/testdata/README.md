# Claude Screen Fixtures

These fixtures are tmux pane captures from real `claude` TUI runs. Each
`screens/*.json` file stores the pane title, working directory, capture source,
and expected detector outputs. The adjacent `screens/*.txt` file is the raw
`tmux capture-pane -p -J` text replayed by the tests.

To capture a new fixture:

1. Start Claude in a fixed-size tmux pane, for example:
   `tmux new-session -d -s claude-fixture -x 120 -y 40 -c "$PWD" 'claude --permission-mode bypassPermissions --effort max'`
2. Capture the title with:
   `tmux display-message -p -t claude-fixture '#{pane_title}'`
3. Capture the visible pane with:
   `tmux capture-pane -p -J -t claude-fixture -S -80`
4. Save the pane text as `screens/<name>.txt`, add a matching
   `screens/<name>.json`, then run `go test ./pkg/agentcli/claude`.

For prompt-submission captures, `tmux send-keys -l '<prompt>'` followed by
`tmux send-keys C-m` most closely matches what Claude accepted during fixture
capture.
