# Changelog

All notable changes to OpenPoet are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Automation API v1** (`/api/automation/v1`): a versioned command API with
  scoped client credentials, explicit approvals, idempotency keys, a typed
  capability registry shared with the UI, a transactional event outbox with
  durable delivery and acknowledgements, Server-Sent Events, snapshots, durable
  work runs and plans, and daily session reports. The contract is published as
  OpenAPI 3.1 in [`api/automation/v1/`](api/automation/v1/).
- Automation capabilities for sessions: read a session's conversation as clean
  text (`sessions.messages`, also exposed as the MCP tool
  `openpoet_session_messages`), switch a live Claude Code session's permission
  mode, close finished sessions, answer pending session questions, reopen
  sessions in error, and wait for a turn with its excerpt.
- An `approvals:waived` scope so a trusted automation client can self-approve
  explicit capabilities.
- Per-session isolated git worktrees (on demand or automatic), including
  moving an already running session into its own worktree.
- File-conflict radar and a synchronous pre-tool-use gate that warn about or
  block concurrent edits of the same files by different sessions.
- **OpenAI Codex backend** running through `codex app-server`, with
  approvals, questions and MCP elicitations surfaced as OpenPoet modals, a
  structured transcript, live rendering of tool calls, and remote SSH support.
- **OpenCode backend** support.
- An isolated OpenAI OAuth provider bridge for Claude Code, for local and
  remote projects (`make provider-helper`).
- Explicit model and reasoning effort for every session, global session
  defaults, a model-aware effort picker, and reporting of both the requested
  and the effective model. Support for the current Claude model lineup.
- Runtime controls for active sessions, and resuming sessions whose turn was
  cut by a restart.
- Git operations for remote SSH projects.
- Windows ConPTY backend for SSH sessions on remote Windows hosts.
- Project scaffolding, Docker Compose controls and remote directory browsing.
- Project selector sorted by recent use, with search and tag filters.
- Structured view: live PTY streaming, optimistic echo, TodoWrite task lists,
  inline AskUserQuestion answers and a working indicator.
- Per-OS command variants for global MCP servers.
- "New Task" option in the task menu.
- A publish guard (gitleaks plus checks for private IPs, internal hosts, home
  paths, OAuth links and forbidden files) run by versioned pre-commit and
  pre-push hooks; see [docs/publish-guard.md](docs/publish-guard.md).
- A mandatory deploy/release gate requiring a clean tree and a commit on `main`
  and `origin/main`; see [docs/deploy-gate.md](docs/deploy-gate.md).
- A dry-run-by-default safe rollout workflow (`ops/safe-rollout`).

### Changed

- The UI and the Automation API now share the same Application Services for
  every mutation, so both paths enforce the same validation and invariants.
- Hardened SQLite concurrency (busy timeout, a WAL read pool, per-session
  input locking) and SSH connection handling.
- Per-session hook and MCP tokens instead of shared credentials.
- Playwright MCP configuration is stored encrypted.
- Upgraded the Claude Agent SDK, go-openai and xterm.js.
- The API contract files moved from `docs/automation/` to `api/automation/`.

### Removed

- Native Windows builds. On Windows, run OpenPoet inside WSL 2. Remote Windows
  hosts over SSH remain supported.
- The experimental multi-session "missions" and coordinator features.

### Fixed

- Long `send_input` text is delivered intact and its delivery is verified;
  automation clients timing out no longer kill or confuse sessions.
- Long voice recordings are no longer lost (chunked upload with local
  persistence), dictations keep their tail, and transcriptions go to the
  field where they were recorded.
- Terminal rendering artifacts on resize, alternate buffer handling, and
  multi-client sizing.
- The MCP HTTP stream is no longer cut every 10 minutes, and expired MCP HTTP
  sessions recover their session ID.
- AskUserQuestion answers reach the agent again, including index-keyed
  answers.
- Codex: failures of `app-server` are surfaced, remote transcripts are
  restored on reopen, message deltas stay merged across tool calls, and ESC
  interrupts the turn.
- Remote sessions start with default SSH keys, a stalled remote start no longer
  freezes OpenPoet, and remote transcript reads are bounded.
- Transcripts with lines over 1 MiB, transcripts under `CLAUDE_CONFIG_DIR`,
  and following a transcript after resume.
- Encrypted credentials with an invalid nonce now return an error instead of
  panicking.
- `localStorage` quota errors no longer break tab persistence.
- Neutral default for `VAPID_EMAIL`.

## [0.1.15] and earlier

See the [GitHub releases](https://github.com/openpoet/openpoet/releases).

[Unreleased]: https://github.com/openpoet/openpoet/compare/v0.1.15...HEAD
[0.1.15]: https://github.com/openpoet/openpoet/releases/tag/v0.1.15
