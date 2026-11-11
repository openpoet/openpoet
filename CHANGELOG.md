# Changelog

All notable changes to OpenPoet are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **HTML artifacts**: agents publish interactive, self-contained HTML documents
  into the OpenPoet database instead of writing files into the project. Every
  publish is a version and old versions stay readable. Artifacts are linked to
  the project, session and task that produced them and show up in the session
  and task document lists and in a new Artifacts section of the project view.
  See [`docs/artifacts.md`](docs/artifacts.md).
- A sandboxed artifact viewer: the page runs in an iframe that allows scripts
  and nothing else, served with a Content Security Policy that blocks network
  requests and remote resources, so agent-generated HTML cannot reach cookies,
  the session or the OpenPoet API.
- Annotations on artifacts: select text or click an element to leave a note.
  Agents list the open notes and resolve them, and a note made on one version
  is placed again on the next.
- Agent tools for artifacts over MCP, the chat assistant and the CLI
  (`openpoet_create_artifact`, `openpoet_update_artifact`,
  `openpoet_read_artifact`, `openpoet_list_artifacts`,
  `openpoet_list_artifact_annotations`, `openpoet_resolve_artifact_annotation`),
  and the matching Automation API capabilities under `artifacts.*`.
- A built-in `html-artifact` skill with the design guide for artifacts.
- **Questions in artifacts**: an artifact can ask the reader to decide or
  answer something. A version declares its questions in a JSON data block
  (single choice, multiple choice or free text) and marks the page with
  `data-openpoet-question` / `data-openpoet-option`; the reader taps a choice
  and confirms the answers in a new Answers tab of the viewer. Each answer is
  stored as a structured annotation, replaceable and withdrawable, and reaches
  the agent through `openpoet_list_artifact_annotations`, which also names the
  questions still unanswered. See [`docs/artifacts.md`](docs/artifacts.md).
- **Public base URL**: a setting (Config → Settings → Public Address, or
  `OPENPOET_PUBLIC_BASE_URL`) with the address the instance is reached at from
  other devices. Links handed out by OpenPoet are built on it, and its host is
  accepted by the Host guard. See [`docs/links.md`](docs/links.md).
- `openpoet_get_link` (MCP, CLI) and `get_link` (chat assistant): the link that
  opens an artifact, a Doc, a session or a task, ready to pass on. Agents no
  longer have a reason to build a URL on `localhost`. Also `GET /api/links`.
  The artifact and Doc publish results name the shareable link.

### Changed

- The built-in `html-artifact` skill teaches how to ask questions, tells
  agents to keep the content of tabs in the DOM, and states the sandbox policy.
  An installed copy that was never edited is updated on start; an edited one is
  left as it is.
- "Send to agent" says it is OpenPoet reporting a button press, counts answers
  and notes separately, and asks the agent to act on each one or leave it open
  saying why.
- A link path opens what it points at. `/app/artifact/<id>` (with `?v=<n>`
  for a version), `/app/doc/<id>`, `/app/session/<id>`, `/app/project/<id>`
  and `/app/task/<project>/<task>` used to load the app on the projects list;
  they are now rewritten into the canonical address and opened, also from a
  notification link. An artifact opened on an older version keeps the version
  in its address.
- Through the tunnel, a device that has to pair first is sent back to the link
  it was opening once the pairing is approved.
- Agents are now steered to publish plans, reports and documentation as HTML
  artifacts. Docs remain for short plain-text answers. The managed instruction
  block in each project is replaced on the next config sync.
- Tool policies written as an allowlist that already grant `create_document` or
  `read_document` are extended with the matching artifact tools on upgrade.
  Policies that allow everything are not changed.

### Security

- An artifact's own script cannot answer for the reader. A choice proposed
  from the sandboxed frame is checked against the questions the server stored
  for that version, is only staged, and is saved when the user confirms it in
  the OpenPoet page. Comments and free-text answers are typed in the page and
  never enter the frame. Sessions cannot write answers.
- Rendered artifacts neuter WebRTC. The `webrtc 'block'` CSP directive is
  ignored by Chromium (a peer connection could still gather candidates against
  an external STUN host), so a guard script runs as the first script of every
  rendered document and removes the `RTCPeerConnection` constructors before any
  agent script, closing an outbound channel the CSP fetch directives do not
  govern. The directive stays as defence in depth.

## [0.2.0] - 2026-10-25

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

[Unreleased]: https://github.com/openpoet/openpoet/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/openpoet/openpoet/compare/v0.1.15...v0.2.0
[0.1.15]: https://github.com/openpoet/openpoet/releases/tag/v0.1.15
