# Contributing to agyh

Thanks for helping! agyh is small on purpose: one static Go binary, no server, no telemetry.

## Before you start

- **Bugs**: open an issue with the bug template.
- **Features**: open an issue first and describe the problem. A short discussion saves you from a PR that can't be merged.
- **Security issues**: never in public issues. See [SECURITY.md](SECURITY.md).

## Development

```bash
git clone https://github.com/EliasOlie/agyh && cd agyh
go build -o agyh ./cmd/agyh
go test -race ./...
```

Requires Go 1.26+. Pure Go (`CGO_ENABLED=0`), so it cross-compiles everywhere.

Before opening a PR, make sure these pass (CI runs the same on Linux and macOS):

```bash
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
```

## Pull requests

- `main` is protected: every change goes through a PR with green CI and a maintainer approval.
- Keep PRs focused; one topic per PR. Squash merge is used, so the PR title becomes the commit message:
  use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`...).
- Add or update tests for behaviour changes, and update the README when a command, flag or config key changes.
- **Never weaken the safety model** without discussing it first: agent data is read-only, the bridge is
  loopback-only, chat input never reaches a shell, and fatal commands stay blocked even in autonomous mode.
- Don't include real session transcripts, tokens or personal data in tests or fixtures.

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
