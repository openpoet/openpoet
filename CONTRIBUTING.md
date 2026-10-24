# Contributing to OpenPoet

Thanks for your interest in OpenPoet! Bug reports, fixes, documentation and
new features are all welcome. This guide covers how to get a working
development setup from a clean clone and the conventions the project follows.

By participating you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Security issues must **not** be reported in public issues; see
[SECURITY.md](SECURITY.md).

## Development setup

### Requirements

- **Go**: the version declared in [`go.mod`](go.mod) (currently Go 1.25).
- **git** and **make**.
- **Node.js 20+** and npm, only for the browser/DOM tests (`make test-web`).
- **gitleaks**, used by the publish guard hooks (installed by `make tools`).
- Optional: the [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
  CLI (or another supported agent CLI such as Codex or OpenCode) to actually
  start sessions from OpenPoet.

### From a clean clone

```bash
git clone https://github.com/openpoet/openpoet.git
cd openpoet

make setup      # enable the versioned git hooks (pre-commit + pre-push)
make tools      # install gitleaks (and golangci-lint) into your GOPATH/bin
make build      # builds ./build/openpoet
./build/openpoet
# open http://localhost:8080
```

`make dev` runs the server straight from source (`go run ./cmd/openpoet`).
Useful environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `OPENPOET_BIND` | `localhost` | Address to listen on (see [SECURITY.md](SECURITY.md) before changing it) |
| `OPENPOET_PORT` | `8080` | Port to listen on |
| `OPENPOET_DB` | `~/.openpoet/openpoet.db` | SQLite database path; point it at a throwaway file while developing |
| `OPENPOET_ENCRYPT_KEY` | derived from host and user | Key used to encrypt stored credentials |

Use a separate database for development so you never touch the data of an
OpenPoet instance you rely on, for example
`OPENPOET_DB=/tmp/openpoet-dev.db make dev`.

### Running the tests

```bash
make test       # go test -v ./...
make test-web   # Playwright browser/DOM tests under tests/
```

`make test-web` installs the dev-only npm dependency (`playwright`, pinned in
`package.json`) and Chromium on first use. The tests run in three groups:
`unit` (Node only), `browser` (Playwright against in-process pages) and
`server` (Playwright against a running instance). The `server` group is
skipped unless something answers at `http://localhost:8080`; point it
elsewhere with `OPENPOET_E2E_URL`.

`go test ./...` also runs the shell test suites of the publish guard and the
deploy gate (`ops/publish-guard`, `ops/deploy-gate`).

Before opening a pull request, please run at least:

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

The pre-commit hook runs `gofmt`, `go vet` and the publish guard for you.

## The publish guard (why your commit may be refused)

OpenPoet is a public repository, and everything that reaches GitHub has to be
treated as published forever. The hooks installed by `make setup` run
`ops/publish-guard/guard.sh`, which refuses commits and pushes containing:

- secrets detected by gitleaks;
- private IP addresses and internal hostnames;
- personal home paths (`/home/<you>`, `/Users/<you>`, `C:\Users\<you>`);
- OAuth links carrying codes or tokens;
- forbidden files: `.env`, keys, databases, logs, dumps, large binaries,
  files generated from a local agent setup (`CLAUDE.md`, `AGENTS.md`,
  `.claude/`, ...), and images outside `docs/images/` and `web/static/`.

Test fixtures should use neutral values: `example.com`, `192.0.2.x`
(TEST-NET), `/home/dev`, `C:\Users\dev`. Run `make publish-guard` before
pushing. Please do not bypass the hooks with `--no-verify`. The details,
including how to keep a private denylist of names that must never be
committed, are in [docs/publish-guard.md](docs/publish-guard.md).

## Coding conventions

- **Go:** formatted with `gofmt`, clean under `go vet`. Keep packages under
  `internal/`; the entry point is `cmd/openpoet`.
- **Frontend:** vanilla JavaScript and CSS, no framework and no build step.
  Static assets live in `web/static/` and are embedded in the binary.
- **Database migrations are additive.** Never edit or reorder an existing
  migration in `internal/database/migrations.go`; add a new one. Released
  binaries and existing user databases depend on the migration history.
- **Dependencies must use MIT-compatible licenses** (MIT, BSD, Apache-2.0,
  ISC, ...). Update [THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md) when
  you add one.
- **Icon-only buttons need a `title` attribute.** The global long-press
  tooltip (`web/static/js/longpress-tooltip.js`) reads it to show tooltips on
  touch devices, so no per-button JavaScript is needed.
- **Mobile and desktop both matter.** Interactions must work on touch
  (tap, long-press) and with a mouse (click, hover). Check narrow viewports.
- **Automation API parity.** User-facing mutations go through the shared
  Application Services so the UI and the Automation API behave the same.
  `TestUIActionManifestCoversBackendAndFrontendMutations` fails when a new
  route or `onclick="app.*"` handler is not classified in
  [`api/automation/ui-action-manifest.v1.json`](api/automation/ui-action-manifest.v1.json);
  see [docs/automation/ui-action-matrix.md](docs/automation/ui-action-matrix.md).
- **Tests:** add or update tests with every behavior change. Go tests sit
  next to the code; browser tests live in `tests/`.

## Commits and pull requests

- Write commit messages in English, in the
  [Conventional Commits](https://www.conventionalcommits.org/) style
  (`feat(sessions): ...`, `fix(terminal): ...`, `docs: ...`). Explain *why*
  in the body when it is not obvious.
- Keep pull requests focused on one topic. Split unrelated changes.
- Fill in the pull request template, link the issue it addresses, and add an
  entry under `[Unreleased]` in [CHANGELOG.md](CHANGELOG.md) for user-visible
  changes.
- Releases are cut from tags on `main`; the release workflow refuses tags that
  do not point at a commit on `main` (see [docs/deploy-gate.md](docs/deploy-gate.md)).

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
