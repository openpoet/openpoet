# UI to Automation parity matrix

Canonical source: [`ui-action-manifest.v1.json`](../../api/automation/ui-action-manifest.v1.json).
This matrix covers the HTTP mutations declared in the router, the mutating
frontend calls, and purely local inline actions. Every business route has a
verified binding to the same Application Service used by both the UI and the
Automation API.

## Coverage

| Item | Count |
| --- | ---: |
| Mutating `/api` routes | 121 |
| Inline business actions | 57 |
| `presentation_only` actions | 49 |
| Static business listeners | 19 |
| Static presentation listeners | 27 |
| Explicit telemetry/test exceptions | 7 |

| Status | Count | Meaning |
| --- | ---: | --- |
| `implemented` | 119 | UI and Automation share the Application Service and its invariants |
| `application_service_ready` | 0 | No route remains on an intermediate path |
| `gap` | 0 | No Application Service gaps |
| `internal_only` | 2 | Hook ingestion, not a standalone user action |

## Matrix by domain

| Domain | Total | Implemented | Service ready | Gap | Internal | Risks |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| agents | 3 | 3 | 0 | 0 | 0 | R2, R3 |
| ai | 16 | 16 | 0 | 0 | 0 | R1, R2, R3 |
| ai_configs | 4 | 4 | 0 | 0 | 0 | R4 |
| config | 1 | 1 | 0 | 0 | 0 | R4 |
| documents | 2 | 2 | 0 | 0 | 0 | R2 |
| files | 4 | 4 | 0 | 0 | 0 | R2, R3 |
| git | 3 | 3 | 0 | 0 | 0 | R2, R3 |
| hooks | 4 | 2 | 0 | 0 | 2 | R1, R2, R3 |
| mcp | 8 | 8 | 0 | 0 | 0 | R4 |
| notifications | 6 | 6 | 0 | 0 | 0 | R1, R2, R3 |
| projects | 8 | 8 | 0 | 0 | 0 | R1, R2, R3, R4 |
| proposals | 9 | 9 | 0 | 0 | 0 | R1, R2, R3, R4 |
| sessions | 10 | 10 | 0 | 0 | 0 | R1, R2, R3 |
| settings | 1 | 1 | 0 | 0 | 0 | R4 |
| skills | 10 | 10 | 0 | 0 | 0 | R2, R3 |
| tags | 4 | 4 | 0 | 0 | 0 | R2, R3 |
| task_links | 2 | 2 | 0 | 0 | 0 | R2 |
| tasks | 10 | 10 | 0 | 0 | 0 | R2, R3 |
| token_usage | 1 | 1 | 0 | 0 | 0 | R3 |
| tools | 6 | 6 | 0 | 0 | 0 | R4 |
| tunnel | 5 | 5 | 0 | 0 | 0 | R4 |
| update | 1 | 1 | 0 | 0 | 0 | R4 |
| voice | 1 | 1 | 0 | 0 | 0 | R1 |

## Security

- Every `DELETE` is R3 or R4.
- Credentials, MCP servers with env/commands, AI configs, custom tools, the
  tunnel and updates are R4.
- File writes and commits are R3; arbitrary tool execution is R4.
- `presentation_only` actions get no capability. The list exists only to prove
  that local buttons and toggles were inventoried.
- Telemetry, OTLP and the test-only endpoint live in a machine-readable
  allowlist with a justification; they do not become capabilities.

## Shared Application Services

- `ProjectService`: list/get/create/update/delete/duplicate, encrypted
  credential boundary, validation and decoupled effects.
- `TagService`: tag CRUD and assignment of tags to projects with deduplication.
- `NotificationService`: bounded listing, active notifications, unread count
  and mark read/all on the existing backend.
- `SkillService`: global and per-project CRUD, duplicate/import/restore,
  transactional versioning and per-project skill configuration.
- `AIAgentService`: CRUD with policy/filter validation and protection of the
  default agent.
- `AIConfigService`: CRUD/assignments, encrypted secret, partial update that
  does not erase the API key, and reinit only after commit.
- `MCPService`: global/project MCP servers with ownership, commands/args/env in
  encrypted envelopes and strictly redacted DTOs.
- `CustomToolService`: per-project CRUD with encrypted command and no plaintext
  in responses.
- `ConfigurationService`: settings, MCP API key, global/project sync, shares and
  global/project policies in atomic batches.
- `SessionService`: create/stop/reopen/input/evaluate and image hint, with state
  transitions, explicit env/unsafe flags and post-success effects.
- `SessionEventWatcherService`: watcher start/stop through a direct adapter,
  without a synthetic HTTP request.
- `HookResponseService`: permission responses and task notifications; hook
  ingestion stays `internal_only`.
- `FileMutationService`: writes/uploads in projects and sessions, image paste,
  payload limits and strict confinement to relative paths.
- `GitMutationService`: stage/unstage/commit through typed arguments and
  explicit approval for commits.
- `VoiceTranscriptionService`: transcription with bounded audio, filename and
  language tag before reaching the current provider.
- `TunnelMutationService`: enable/disable, one-time pairing confirmation,
  revoke and permanent delete; results and events never carry credentials,
  tokens or device keys.
- `UpdateMutationService`: apply, blocked for managed installs and while
  sessions are active, with separate authorization for `force`.
- `DocumentService`: memory docs and bounded temporary documents, with secrets
  redacted before persistence and no raw content in creation responses.
- `ProposalService`: memory/task/skill/tool proposals on a durable backend;
  atomic accept/reject, and R3/R4 risk with explicit approval.
- `AIAssistantService`: chat, conversations, initiate, suggestions,
  generate/validate/test and tool execution through ports, with no raw
  transcript/prompt and with bounded/redacted outputs.
- `NotificationDeliveryService`: push subscribe/unsubscribe, preference and test
  through a port; subscription keys never appear in responses.
- `TokenUsageService`: full clear with R3 approval and a post-commit effect.

Operations that handle secrets, commands, tool policies, provider assignments
or external synchronization require an explicit `R4Boundary` with approval,
approver and justification. Credentials, env and commands are never part of the
output DTOs; partial updates preserve an omitted secret. Effects and reinit run
only after persistence/synchronization succeeds.

Content, prompts, JSON arguments and AI/proposal outputs have explicit limits.
Secrets are stripped before they reach a provider, a tool or storage; chat
responses never include the prompt, transcript, proactive context or raw
messages. Deletes, usage clears and tool execution/approval use
`ActionAuthorization` with explicit approval.

The services are registered in a typed registry of 155 capabilities
(106 mutations and 49 reads) and composed on top of the process's real
dependencies. Human-facing handlers fail closed if the bundle is not ready;
there is no mutating fallback to the legacy path. SSE, multipart and WebSocket
remain transport edges only.

## Keeping the matrix honest

The `TestUIActionManifestCoversBackendAndFrontendMutations` test fails when a
new router/frontend mutation or a new `onclick="app.*"` handler is added
without an explicit classification in the manifest.
