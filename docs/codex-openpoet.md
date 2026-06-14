# Codex In OpenPoet

OpenPoet can run local and remote SSH projects with the OpenAI Codex CLI backend.

The default runtime is `codex app-server`, rendered through the OpenPoet
terminal. This keeps terminal-based prompting while routing Codex approvals,
questions, and MCP elicitations through OpenPoet's app modals.

## Requirements

- For local projects, install and authenticate the Codex CLI on the same machine running OpenPoet.
- For remote projects, install and authenticate the Codex CLI on the remote SSH host.
- Keep `codex` on `PATH`, or set a project-specific Codex binary path in the project backend settings.

## Project Settings

Select `OpenAI Codex` as the project backend to show Codex-specific settings:

- `Codex Binary`: optional path to the `codex` executable.
- `CODEX_HOME`: optional Codex home directory override.
- `Model`: optional model override. Empty uses the Codex CLI default.
- `Reasoning Effort`: optional reasoning effort override.
- `Service Tier`: optional service tier, such as `flex` or `fast`.
- `Approval Policy`: Codex approval policy. The OpenPoet default is `on-request`.
- `Sandbox Mode`: Codex sandbox mode. The OpenPoet default is `workspace-write`.
- `Runtime`: `OpenPoet Terminal + Modals` uses `codex app-server`; `Native Codex TUI`
  runs the interactive Codex CLI in a PTY and keeps Codex prompts in the terminal.

OpenPoet stores these values in the project `backend_config` JSON blob.
Use `backend_config.runtime = "tui"` only when native Codex TUI menus and
pickers are more important than OpenPoet modal handling.

## Runtime Behavior

Current Codex support includes:

- Local session start through `codex app-server`.
- Remote SSH session start through `codex app-server`, including Windows OpenSSH hosts.
- OpenPoet terminal input/output for Codex turns.
- OpenPoet modals for tool approvals, user questions, and MCP elicitations.
- Terminal output rendering through OpenPoet's WebSocket path.
- Project memory sync between `CLAUDE.md`, `AGENTS.md`, and the OpenPoet memory doc.
- Project skills synced under `.agents/skills`.
- Project MCP config synced under `.codex/config.toml`.
- Remote OpenPoet MCP injection through the SSH reverse tunnel.

## Current Limits

- The structured JSONL event browser is still Claude-only.
- Codex token accounting is hidden in the UI until OpenPoet has a reliable Codex usage source.
- Native Codex TUI mode is available as an explicit runtime option, but its
  approval and permission flows remain inside the terminal.
