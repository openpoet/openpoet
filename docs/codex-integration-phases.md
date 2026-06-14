# Codex Integration Phases

This tracker keeps the Codex integration work explicit between sessions.

## Current Phase

Phase 8 implemented: remote Codex parity; remote host environment validation pending.

## Phase 8: Remote Codex Parity

Status: implemented; remote host environment validation pending.

Scope:
- Allow `OpenAI Codex` on remote SSH projects, including Windows OpenSSH hosts.
- Keep `codex app-server` as the default runtime for remote projects so
  OpenPoet terminal rendering, approvals, questions, and MCP elicitations keep
  using app modals.
- Keep native Codex TUI available for remote projects when explicitly selected.
- Sync Codex project configuration remotely: `.agents/skills`,
  `.codex/config.toml`, and `AGENTS.md`/`CLAUDE.md` memory docs.

Completed in source:
- Removed the API and UI rejection for remote Codex projects.
- Added a remote `codex app-server` runner over SSH stdio with OpenPoet MCP
  rewritten through the existing SSH reverse tunnel.
- Made the generic remote terminal runner backend-aware so native Codex TUI
  starts `codex` instead of hard-coded `claude`.
- Added remote Codex config sync for skills, MCP config, and memory docs.

Verification:
- `node --check web/static/js/app.js`
- `go test ./internal/session ./internal/configsync ./internal/handlers ./internal/database ./cmd/openpoet`
- `make build`
- Runtime-tested on an isolated server using a copied DB on port 64627 with
  project `Remote Windows Project (Copy)` configured for Codex app-server:
  - OpenPoet accepted the remote Codex project update.
  - Remote Codex config sync completed over Windows OpenSSH.
  - SSH reverse tunnel started and OpenPoet MCP was rewritten to the tunnel URL.
  - Session startup reached the Windows remote launcher.
  - The remote host returned `codex` not found in the OpenSSH session PATH, so
    the remaining validation requires installing/authenticating Codex CLI on
    that Windows host or setting the project Codex binary path to the absolute
    `codex` executable path.

## Phase 7: OpenPoet Modal Parity For Codex

Status: complete and validated.

Scope:
- Make `codex app-server` the default Codex runtime again so approvals,
  questions, and MCP elicitations can be routed through OpenPoet modals.
- Keep the native Codex TUI available as an explicit project runtime option.
- Preserve terminal-based Codex interaction in OpenPoet while restoring modal
  responses for interactive requests.

Completed in source:
- Changed the default Codex runtime from native TUI to `app-server`.
- Added a Codex runtime selector to project settings.
- Added backend metadata to hook events and updated Codex-facing modal labels.

Verification:
- `node --check web/static/js/app.js`
- `node --check web/static/js/hooks.js`
- `go test ./internal/session`
- `go test ./internal/handlers`
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`
- `make build`
- Runtime UI smoke on development port 63466:
  - Project modal shows Codex runtime selector with `OpenPoet Terminal + Modals` selected by default.
  - Simulated Codex AskUser event renders `Codex has a question`.
  - Simulated Codex Bash approval renders the nested `action.command` preview.
  - Browser console had no JavaScript errors.

## Phase 1: Local Codex Session Runtime

Status: complete and smoke-tested.

Scope:
- Add `codex` as a project backend.
- Start local Codex sessions through `codex app-server`.
- Persist the Codex provider thread id on OpenPoet sessions.
- Send terminal input to Codex turns and render Codex output in the OpenPoet terminal.

Verification:
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`
- Created a Codex-backed session for project 20 through `POST /api/sessions`.
- Sent `Reply with exactly: OpenPoet Codex smoke test OK`.
- Confirmed terminal output included `Codex: OpenPoet Codex smoke test OK`.

Known limitation:
- The deploy script builds and copies the patched binary, but in this tool environment its detached child may not survive after the script exits. The deployed `.run/openpoet` binary can be run attached to keep the service available.

## Phase 2: Project Knowledge And Config Parity

Status: complete.

Superseded by Phase 8: remote Codex is now in scope.

Scope:
- Keep Codex `AGENTS.md` aligned with existing Claude project memory.
- Keep repo skills available under `.agents/skills`.
- Keep Codex project MCP config written under `.codex/config.toml`.
- Make sync behavior auditable and repeatable.

Completed in source:
- Treat `CLAUDE.md`, `AGENTS.md`, and the OpenPoet DB memory doc as one project-memory source. The newest source wins and both repo files are updated so Claude and Codex do not drift.

Verification:
- `go test ./internal/configsync`
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`

Deployment:
- Deployed with the Phase 3 runtime changes in build `ab220aa`.

## Phase 3: Approval, Tools, And Hooks Parity

Status: complete and smoke-tested.

Scope:
- Exercise Codex tool approval requests through OpenPoet hooks.
- Confirm `allow`, `deny`, and session-scoped allow behavior.
- Confirm OpenPoet MCP injection is usable from Codex sessions.

Completed in source:
- Extracted Codex approval decision mapping into testable helpers.
- Mapped OpenPoet `allow`, `allowAlways`, and `deny` hook results to Codex `accept`, `acceptForSession`, and `decline`.
- Tightened local `allowAlways` caching so file-change requests only bypass hooks when Codex provides a stable `grantRoot`; one-off file-change approvals no longer share a broad empty cache key.
- Added AskUser answer conversion tests for Codex's `{answers: {id: {answers: [...]}}}` response shape.
- Added `mcpServer/elicitation/request` handling so Codex MCP tool-call approvals route through OpenPoet's permission hook flow and return Codex `accept`, `decline`, or `cancel`.

Verification:
- `go test ./internal/session`
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`
- Runtime-tested on an isolated dev server using a copied DB on port 53787.
- Triggered a real Codex Bash approval request and denied it through `POST /api/hooks/permission/{sessionId}/respond`; Codex received the rejection and the outside-workspace test file was not created.
- Triggered a real OpenPoet MCP `openpoet_create_document` call from Codex; approved the MCP elicitation through the same hook response API; Codex completed the call and returned document id `b6699f3d`.
- Runtime-tested session-scoped `allowAlways` behavior on an isolated server using a copied DB on port 54725.
- Temporary Codex session `bf60dc8d-4947-4d34-9f09-81b258ee67ea` ran the same outside-workspace Bash command twice.
- First command produced a real `Bash` permission request; responding `allowAlways` returned `acceptForSession` to Codex.
- Second identical command completed with `OpenPoetAllowAlwaysOK` and no second pending hook request.
- The temporary session was deleted, the isolated server was stopped, and the outside-workspace probe file was absent after the run.

Next task:
- Move to Phase 4 resume and long-running session behavior.

## Phase 4: Resume And Long-Running Session Behavior

Status: complete and smoke-tested.

Scope:
- Verify provider-thread resume after OpenPoet restart.
- Verify stop/delete cleanup for Codex child processes.
- Verify interrupted turns and failed turns recover cleanly.

Completed in source:
- Preserve Codex provider thread ids through restart restore and manual reopen.
- Clear stale `activeTurnID` on Ctrl-C so post-interrupt prompts start a fresh turn.
- Track interrupted Codex turn ids and suppress late deltas/items from those turns. This prevents command output that arrives after a local interrupt from being displayed in the OpenPoet terminal.
- Recognize Codex app-server's current `commandExecution` item type, so command metadata displays correctly in the terminal.
- Best-effort signal the Codex-reported local command process id during Ctrl-C, with the interrupted-turn output fence as the reliable fallback when Codex has already released the wrapper process.

Verification:
- `go test ./internal/session`
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`
- Runtime-tested on an isolated server using a copied DB on port 55179.
- Created Codex session `83841ac3-b38b-4eef-93cf-3fd7176bc7f0`, stored marker `OPR4_RESUME_MARKER_55179`, gracefully restarted OpenPoet, auto-restored the same OpenPoet session, confirmed provider thread id stayed `019eba75-7bf7-78a1-9aed-b8de89abaf44`, and Codex recalled the marker after restart.
- Confirmed the pre-restart Codex child process exited after graceful shutdown, and the restored session used a new child process.
- Deleted temp sessions and confirmed session monitors marked them `stopped`.
- Reproduced a Ctrl-C bug where a long-running command printed `INTERRUPT_SHOULD_NOT_COMPLETE` after the user interrupted and continued with a new prompt.
- Fixed and retested with temp session `8fb5b206-963b-40cc-b712-1c7ea696dd3b`: after Ctrl-C and waiting past the original command duration, the late marker was not present in terminal output; a follow-up prompt returned `Codex interrupt recovery OK`.
- Ran a failed-command recovery check in the same session; after `bash -lc 'exit 7'`, Codex returned `Codex failed command recovery OK`.
- Stopped the isolated server after validation.

Deployment:
- Not deployed yet after this Phase 4 slice.

## Phase 5: UI Polish And Remote Strategy

Status: complete.

Scope:
- Add any missing Codex backend settings UI.
- Decide whether remote Codex sessions are in scope or explicitly unsupported for now.
- Document user-facing setup and limitations.

Completed in source:
- Added Codex backend settings to the project modal: binary path, `CODEX_HOME`, model, reasoning effort, service tier, approval policy, and sandbox mode.
- Persist Codex settings through the existing `backend_config` project JSON blob when Codex is selected.
- Preserve Codex backend settings when the project directory picker temporarily re-renders the project form.
- Added UI guidance for remote projects and Codex.
- Added API validation so remote Codex projects are rejected at create/update time, with duplicate-project validation as an additional guard.
- Added focused handler tests for the remote Codex rejection.
- Added user-facing setup documentation in `docs/codex-openpoet.md`.

Verification:
- `node --check web/static/js/app.js`
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`
- `go test ./...` still fails only on the pre-existing unrelated `internal/llm/TestGoSDKModeRoutingByConversationID`: `one-shot options should not exceed interactive options`.

Deployment:
- Not deployed yet after this Phase 5 slice.

Next task:
- Deploy the Phase 4 and Phase 5 Codex integration changes when requested, or define Phase 6.

## Phase 6: Native Codex Slash Command Parity

Status: complete and runtime-tested.

Scope:
- Use the native interactive `codex` TUI as the default OpenPoet Codex runtime.
- Preserve Codex-owned slash commands, Tab completion, menus, and future command additions.
- Keep the app-server runner available only as an explicit internal fallback via
  `backend_config.runtime = "app-server"`.
- Avoid OpenPoet frontend/backend slash-command interception for native Codex sessions.

Completed in source:
- Switched the default Codex runtime from app-server protocol emulation to the
  native interactive `codex` TUI in OpenPoet's PTY.
- Kept `codex app-server` available only through the explicit
  `backend_config.runtime = "app-server"` escape hatch.
- Made Codex CLI args match native TUI behavior, including `codex resume`
  ordering for reopened sessions.
- Added a Codex binary-path override for local PTY runners.
- Disabled OpenPoet's Codex slash-command interception so `/`, Tab, arrows, and
  interactive pickers are owned by the Codex TUI.
- Removed the obsolete OpenPoet-managed Codex slash menu JavaScript and CSS.
- Fixed mobile terminal sync after deploy by removing a stale `lineContent`
  reference and ignoring OSC/DCS terminal responses in the input tracker.

Verification:
- Confirmed native terminal Codex behavior: `/` opens the Codex slash menu and
  `/resume` opens the interactive resume picker.
- Runtime-tested OpenPoet on isolated port 60584 with Codex project
  `Codex TUI Probe`.
- Confirmed OpenPoet Codex startup renders the native `OpenAI Codex (v0.139.0)`
  TUI, not the app-server prompt.
- Confirmed `/` inside OpenPoet shows native commands including `/model` and
  `/permissions`.
- Confirmed Tab completion inside OpenPoet completes `/m` to `/model`.
- Confirmed `/resume` inside OpenPoet opens the native "Resume a previous
  session" picker with search/filter/sort controls.
- Confirmed `/permissions` inside OpenPoet opens the native model permissions
  menu with approval/sandbox choices.
- `node --check web/static/js/terminal.js`
- `node --check web/static/js/app.js`
- `go test ./internal/session ./internal/configsync ./internal/database ./internal/handlers ./cmd/openpoet`
- `make build`
- `go test ./...` still fails only on the unrelated `internal/llm/TestGoSDKModeRoutingByConversationID`: `one-shot options should not exceed interactive options`.

Deployment:
- Deployed to production port 8081 on 2026-06-12 with build
  `ab220aa-1781265586`.
