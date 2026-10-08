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
- `Model`: the project's model, picked from the list `codex app-server` reports
  (`model/list`). Empty uses the global default (Settings → Session Model &
  Effort), never the Codex account default.
- `Reasoning Effort`: the project's effort; the list follows the chosen model
  (`supportedReasoningEfforts`). Empty uses the global default.
- `Service Tier`: optional service tier, such as `flex` or `fast`.
- `Approval Policy`: Codex approval policy. The OpenPoet default is `on-request`.
- `Sandbox Mode`: Codex sandbox mode. The OpenPoet default is `workspace-write`.
- `Runtime`: `OpenPoet Terminal + Modals` uses `codex app-server`; `Native Codex TUI`
  runs the interactive Codex CLI in a PTY and keeps Codex prompts in the terminal.

OpenPoet stores these values in the project `backend_config` JSON blob.

Every session passes model and effort explicitly: `--model` and
`-c model_reasoning_effort=...` for the TUI, and `model` plus
`config.model_reasoning_effort` on `thread/start`/`thread/resume` for the
app-server runtime (`thread/start` ignores an `effort` param). The model and
effort Codex reports back for the thread are recorded as the session's effective
values; native TUI sessions on this host read them from the rollout's last
`turn_context`. See the user manual, "Modelo e effort sempre explícitos".
Use `backend_config.runtime = "tui"` only when native Codex TUI menus and
pickers are more important than OpenPoet modal handling.

## Runtime Behavior

Current Codex support includes:

- Local session start through `codex app-server`.
- Remote SSH session start through `codex app-server`, including Windows OpenSSH hosts.
- OpenPoet terminal input/output for Codex turns.
- OpenPoet modals for tool approvals, user questions, and MCP elicitations.
- Terminal output rendering through OpenPoet's WebSocket path.
- Failed turns, retryable stream errors, config warnings, deprecation notices,
  guardian warnings, and MCP server startup failures surfaced in the terminal
  and in the structured transcript.
- Live rendering of `fileChange`, `mcpToolCall`, `dynamicToolCall`,
  `functionCallOutput`, `webSearch`, `subAgentActivity`, and `contextCompaction`
  items, plus the matching agent phases (`running_tool`, `searching`,
  `waiting_approval`, `waiting_input`).
- Structured transcript for automation: the user and assistant messages the
  runner persists in `codex_transcript_events` are served by `sessions.messages`
  (list, search, expand), the same as a Claude Code JSONL transcript. Sessions on
  the `tui` runtime record no transcript and still answer
  `session_transcript_unavailable`.
- Turn signals: the runner posts `mode_changed` (executing) to
  `/api/hooks/event` when a turn starts and `Stop` when it ends (`turn_status`
  completed, failed or interrupted, with `last_assistant_message`). That closes
  the session's turn, idles its mode and emits `session.turn_completed` with a
  `last_message` excerpt. An approval or question parked on the permission hook
  emits `session.awaiting_input`.
- Project memory sync between `CLAUDE.md`, `AGENTS.md`, and the OpenPoet memory doc.
- Project skills synced under `.agents/skills`.
- Project MCP config synced under `.codex/config.toml`.
- Remote OpenPoet MCP injection through the SSH reverse tunnel.

## Protocol Baseline

The integration tracks the `codex app-server` v2 protocol as shipped by Codex
CLI 0.154.x. `codex app-server generate-json-schema --out <dir>` prints the
schema the installed binary speaks, which is how to check for drift after a
Codex upgrade. Legacy method and item names (`turn/failed`, `config/warning`,
`file_change`, `execCommandApproval`, `applyPatchApproval`) are still accepted
so older Codex builds keep working.

A failed turn arrives twice — as an `error` notification and again inside
`turn/completed` with `status: "failed"` — so the runner reports only the first
one and unwraps the provider JSON envelope in `TurnError.message`.

Notifications OpenPoet deliberately ignores (they only drive the Codex desktop
UI): `remoteControl/status/changed`, `turn/diff/updated`,
`serverRequest/resolved`, `thread/queue/changed`, `thread/realtime/*`,
`threadSection/*`, and `project/changed`. A session records any notification it
does not handle, so a live run reports protocol drift instead of hiding it.

### Live protocol test

`internal/session/codex_live_test.go` drives the real binary end to end
(initialize, `thread/start`, one turn that writes a file and runs a command,
plus a forced turn failure). It is opt-in because it needs an authenticated
Codex CLI and makes real model requests:

```
OPENPOET_CODEX_LIVE=1 go test ./internal/session -run TestCodexLive -v
```

## Current Limits

- The structured JSONL event browser is still Claude-only.
- Codex token accounting is hidden in the UI until OpenPoet has a reliable Codex usage source.
- Native Codex TUI mode is available as an explicit runtime option, but its
  approval and permission flows remain inside the terminal.
