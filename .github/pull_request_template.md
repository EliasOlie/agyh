## What and why

<!-- What does this change and which problem does it solve? Link the issue: Closes #... -->

## How it was tested

<!-- Commands you ran, OS, agents involved (Claude Code / Codex / Antigravity). -->

## Checklist

- [ ] `gofmt -l .` prints nothing, `go vet ./...` and `go test -race ./...` pass
- [ ] Tests added/updated for behaviour changes
- [ ] README updated if a command, flag or config key changed
- [ ] Does not weaken the safety model (read-only agent data, loopback bridge, no shell for chat input, fatal commands blocked)
- [ ] No real transcripts, tokens or personal data in code, tests or screenshots
