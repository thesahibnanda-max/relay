# Contributing to Relay

Thanks for considering a contribution. This document covers how the repo is organized, how to build
and test it, and what a pull request needs before it can be merged.

## Before you start

* **Bugs and small fixes:** just open a PR.
* **New features or anything that changes behavior:** please open an issue first describing the
  problem and your proposed approach. Relay has explicit design constraints (see
  [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and the "zero footprint" section of
  [README.md](README.md)) that a change needs to respect, and it's much cheaper to align on
  direction before writing code than after.
* **Antigravity CLI (`agy`) and native Windows support are marked 🧪 Experimental** (see
  [NOTICE.md](NOTICE.md)). Reports and fixes for either are especially welcome.

## Development setup

Requires Go (see [go.mod](go.mod) for the exact version) and, for website work, Node.

```sh
git clone https://github.com/thesahibnanda-max/relay.git
cd relay
make build        # ./bin/relay
make check        # gofmt + go vet (incl. other platforms) + full test suite under the race detector
```

Other useful targets (see `make help` for the full list):

```sh
make short        # quick tests only: skips the binary end-to-end, chaos and soak tests
make fuzz         # every fuzz target, briefly
make cross        # cross-compile every supported platform into ./dist
make fmt          # gofmt -w .
```

### Live tests against the real tools

CI drives fakes (`testdata/fakeagent`, `testdata/fakeagy`). Changes to how Relay reads a tool's screen,
transcript or database, or types into it, also need a run against the real tool, which spends model
quota. For `agy`, with an authenticated `agy` on your `PATH` and a working directory its folder-trust
prompt has already been accepted for:

```sh
RELAY_AGY_CWD=~/some/trusted/dir go test -tags e2e_real -run RealAgy -v -timeout 60m -count=1 ./internal/cli
```

It uses your real `~/.gemini` (with an isolated `RELAY_HOME`), fails if agy's config files are not left
byte-identical, and restores them either way. `RELAY_AGY_DIR=/dir` runs a specific agy version found
there (a copy of its binary named `agy`). Real Claude/Codex/Copilot tests live in `internal/agent`
(`-tags e2e_real -run Real`).

### Website (`ui/`)

The marketing site is plain HTML/CSS/JS under `ui/site/`; Node is only used for local preview and
tests.

```sh
cd ui
npm ci
npm run dev       # http://localhost:4173
npm test          # Playwright: layout, demo image, install.sh/install.ps1, accessibility (axe)
```

## Before opening a pull request

* **Never commit directly to `main`.** Work on a branch and open a PR; `main` only accepts PRs
  whose checks have passed.
* **Run `make check`** (and `cd ui && npm test` for website changes) before pushing. CI runs the
  same checks on Linux, macOS and Windows, plus a cross-build and fuzz smoke test - a red CI run
  on your PR will need to be fixed before it can be merged.
* **Add tests** for new behavior and bug fixes. This codebase leans heavily on real, end-to-end
  tests (a scripted fake terminal tool driving the actual `relay` binary) rather than mocks - see
  `internal/cli/e2e_test.go` for the pattern, and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for
  why.
* **Match the surrounding code's style.** Comments explain *why*, not *what* - a non-obvious
  constraint, a workaround for a specific bug, something that would surprise a reader - not a
  restatement of the code. Prefer reusing an existing pattern in the codebase over inventing a new
  one; if you're not sure one exists, search for it or ask in your issue/PR.
* **Keep commit messages to one line**, in the conventional style already used throughout the
  history: `feat: ...`, `fix: ...`, `docs: ...`, `refactor: ...`, `test: ...`, `chore: ...`. No
  commit bodies or bullet lists needed.
* **Respect the zero-footprint rule.** Relay must never write to a wrapped tool's own persistent
  config or run that tool's own "add"/"enable" commands - see the "Zero footprint" section of
  [README.md](README.md) and `internal/doctor`'s footprint check, which an automated test enforces.
  The one accepted, carefully-scoped exception (`agy`, since it has no per-launch registration
  mechanism at all) is documented there too - read it before assuming a second exception is fine.

## Reporting a bug

Open a [GitHub issue](https://github.com/thesahibnanda-max/relay/issues/new) with:

* What you ran (the exact `relay ...` command) and what you expected vs. what happened.
* Your OS and which tool(s) you were wrapping (`relay doctor`'s output is a good start).
* Anything from `~/.relay/log/relayd.log` that looks relevant, with secrets redacted.

## Security

Please don't open a public issue for a security vulnerability. See
[docs/SECURITY.md](docs/SECURITY.md) for the threat model and how to report one privately.

## License

By contributing, you agree that your contribution is licensed under the project's
[BSD 3-Clause License](LICENSE).
