# Dev Manager - Project Meta Document

## Project Overview

OpenPoet is a web application that orchestrates Claude Code sessions across multiple projects, providing a centralized interface for managing development workflows, skills, and AI-assisted coding sessions.

## Core Features

- **Multi-Project Management**: Manage both local and remote (SSH) projects
- **Claude Code Integration**: Start and manage AI-assisted terminal sessions
- **Skills System**: Create and sync instruction templates for Claude Code
- **MCP Server Configuration**: Configure and inject MCP servers into sessions
- **Task Management**: Track todos, progress, and priorities per project
- **Mobile-Optimized UI**: Full-featured mobile terminal with voice input

## Architecture

### Tech Stack

- **Backend**: Go 1.24+
- **Frontend**: Vanilla JavaScript, CSS (no framework)
- **Database**: SQLite
- **Terminal**: xterm.js + WebSocket PTY
- **AI Integration**: Claude API (Anthropic)

### Key Components

- `cmd/openpoet/main.go`: Application entry point
- `internal/database/`: SQLite models and migrations
- `internal/handlers/`: HTTP/WebSocket handlers (API, AI, terminal)
- `internal/session/`: Session manager for Claude Code processes
- `internal/mcp/`: MCP server integration
- `internal/llm/`: Claude API client and prompt management
- `web/templates/`: HTML templates
- `web/static/`: CSS, JavaScript, service worker

### Deployment

- Production runs on port 8081 (`.run/openpoet -port 8081`)
- Deploy: `./.scripts/deploy.sh [--pull|--rollback]`, always behind the mandatory deploy gate (`ops/deploy-gate/gate.sh`, see `docs/deploy-gate.md`)
- Build command: `make build`
- Service Worker provides offline capability and PWA features

## Current Status

- ✅ Core features implemented and stable
- ✅ Mobile UX optimized (terminal, voice input, touch gestures)
- ✅ AI Assistant chat panel with skills management
- ✅ MCP server integration complete
- ✅ Task management system operational
- ✅ OpenTelemetry instrumentation added
- ✅ Document viewer component refactored
- 🚧 Theme system (light/dark) in development

## Recent Changes (2026-02-10)

- Added OpenTelemetry instrumentation (`internal/handlers/otel.go`)
- Implemented theme system UI (`web/static/css/themes.css`, `web/static/js/theme.js`)
- Refactored mobile terminal submit logic (3-step sequence with Ctrl+U)
- Added LLM pricing calculator (`internal/llm/pricing.go`)
- Removed static file handler (migrated to embedded assets)

## Known Issues / Technical Debt

- Mobile terminal submit requires 3-step sequence (Ctrl+U, text, Enter with delay) — fragile, needs investigation
- Service Worker cache management could be improved (manual clear-cache.html workaround)
- No automated tests yet (Playwright framework prepared but not in use)

## Automatic worktree isolation (conflict semantics)

The conflict radar distinguishes two different hazards. Do not collapse them back
into one — that was the bug that made isolation useless.

- **Tree collision** — two live sessions writing the same file in the **same**
  working tree. One silently loses its work, so the gate **denies** (unchanged
  behaviour, `internal/coordinator/gate.go`).
- **Lane divergence** — the same logical file in **different** trees (main
  checkout vs a `.openpoet/worktrees/<lane>` worktree, or two lanes). Each side
  owns its own checkout, nothing can be lost, and git arbitrates when the lane
  is integrated, so this is **never denied**: it emits a `conflict.divergence`
  event (integration risk) and opens no incident.

Claims expire (`claimTTL`, 30 min from the last write to that path) — a claim is
evidence of live contention, not a permanent lock.

Sessions choose their tree with `isolation` on `sessions.create` /
`POST /api/sessions`: `never` (default, main checkout), `auto` (main checkout
while free, an isolated lane once busy), `always` (its own lane unconditionally).
Lanes are **provisioned on demand** — nothing needs pre-provisioning — and the
create response carries `workspace_id`/`work_dir` so the caller can find the
lane later. OpenPoet does not merge lanes: the lane is an ordinary git branch
(`openpoet/<name>`), integrated with plain git by whoever owns the work, then
released with `workspaces.remove` or `workspaces.discard`.

A session that is ALREADY running can be moved into a lane with the
`sessions.isolate` capability (destructive tier). A runner cannot change working
directory in place, so the move is stop → repoint the row → reopen, with two
consequences enforced as typed refusals rather than hidden: the session's
**conversation does not carry over** (Claude Code indexes its transcript by the
encoded cwd, so `--resume` cannot cross the change — the caller supplies a
`briefing`), and a session holding **uncommitted work** in the shared tree is
refused (`session_has_uncommitted_work`), because a lane branches from HEAD and
would leave that work behind. It is cheapest right after a gate denial, when the
losing session has usually written nothing yet.

E2E harness: `ops/lanes/e2e.sh` (real server + real git repos + fresh DB on port
8793; $0 — synthetic sessions via the `OPENPOET_TEST_MODE` endpoint).

## Guidelines & Constraints

- **Port 8081 is PRODUCTION** — never use for testing or debugging
- **NEVER deploy without explicit user approval** — The deploy action (killing production process on port 8081, rebuilding, and restarting) must ONLY be executed when the user has given direct, express approval to deploy. Claude Code sessions must NEVER autonomously decide to deploy. If a task description mentions "deploy", the session must still ask the user for confirmation before executing. This applies to all contexts: task completion, commit workflows, and any other scenario.
- **DEPLOY GATE — nunca há deploy sem tudo commitado** — Todo deploy/release/restart da produção (`.scripts/deploy.sh`, `--pull`, `--rollback`, `ops/safe-rollout`, `release.yml`) passa pelo gate obrigatório `ops/deploy-gate/gate.sh`: working tree limpo (`git status --porcelain` vazio, untracked inclusive) e o commit implantado na `main` **e** em `origin/main`. Se o gate reprovar, o deploy FALHA listando os arquivos: commite (e faça push de) tudo, em commits coerentes, e tente de novo. Nunca contorne o gate com passos manuais (`cp`, `kill`, `systemctl`) nem com stash. O único bypass é `./.scripts/deploy.sh --emergency-bypass "<motivo>"`, que é explícito e registrado em `.run/deploy-gate-bypass.log`. Agentes só o usam com ordem expressa do usuário para aquele deploy. Detalhes: `docs/deploy-gate.md`. Conferir sem deploy: `make deploy-gate`.
- **OPEN SOURCE — nada comprometedor entra em commit nem chega ao remoto** (`docs/publish-guard.md`) — (1) O OpenPoet é público: nunca commitar nem empurrar segredos, tokens, chaves, senhas, IPs e hosts internos, dados pessoais (caminhos com o usuário da máquina, nomes de clientes e de projetos de clientes, e-mails privados), imagens e capturas de debug, docs internos (relatórios de incidente, avaliações, IDs internos de sessão/task), dumps, logs, `.db`, `.env`, binários. Fixtures usam `example.com`, `192.0.2.x`, `/home/dev`. (2) Antes de cada commit, revise o diff com esse critério (o hook de pre-commit roda `ops/publish-guard/guard.sh staged`). (3) Antes de cada push, revise todos os commits que ainda não estão no remoto e rode `make publish-guard` (gitleaks + IPs/paths/denylist local/arquivos proibidos; o hook de pre-push repete). Se achar algo, reescreva o histórico **local** antes de empurrar (backup em bundle antes). (4) O que está no `.gitignore` continua no `.gitignore` (nunca `git add -f`). (5) Segredo que chegou ao remoto exige rotação; reescrever não basta. Nunca `--no-verify`, nunca `--force` puro: `--force-with-lease` só com aprovação do usuário e aviso sobre forks/clones. Skills: `commit`, `push`. Hooks: `make setup`; scanner: `make tools`.
- Database migrations must be additive (never alter existing migrations)
- All dependencies must use MIT-compatible licenses
- Mobile terminal submit logic must follow 3-step sequence (see CLAUDE.md)
- Mobile-responsive features must work on both touch (long-press) and mouse (hover) devices
- All icon-only buttons MUST have a `title` attribute — the global long-press tooltip framework (`web/static/js/longpress-tooltip.js`) uses it to show tooltips on mobile via document-level event delegation. No per-button JS wiring is needed.
