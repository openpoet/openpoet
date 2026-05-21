# OpenPoet — Open Source Readiness Audit

**Date:** 2026-02-17 (updated from 2026-02-09)
**Readiness score:** 4/10

> This document contains a comprehensive analysis of the OpenPoet project in preparation for an open source launch on GitHub. It covers documentation, security, internationalization, infrastructure, code quality, and a prioritized action plan.

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Critical Blockers](#2-critical-blockers)
3. [Security](#3-security)
4. [Internationalization (i18n)](#4-internationalization-i18n)
5. [Required Documentation](#5-required-documentation)
6. [Project Infrastructure](#6-project-infrastructure)
7. [Code Quality](#7-code-quality)
8. [License Choice](#8-license-choice)
9. [Action Plan](#9-action-plan)
10. [Final Checklist](#10-final-checklist)

---

## 1. Executive Summary

OpenPoet has a solid and well-organized codebase, with good separation of concerns, adequate encryption for sensitive data, and a comprehensive Makefile. Since the last audit, significant progress has been made: MIT license added, first tests created, frontend modularized into multiple JS files, multi-provider LLM system implemented, and new modules such as OpenTelemetry, benchmarks, and voice. However, essential elements for an open source launch are still missing.

### What is in good shape

- MIT license added (`LICENSE`)
- Clean organization in Go packages (`internal/` with 13 modules)
- Backend 100% in English
- AES-256-GCM encryption for SSH credentials and API keys
- Versioned and transactional database migrations
- Comprehensive Makefile with multi-platform builds, deploy, test-coverage, and sidecar
- Well-configured `.gitignore` (excludes DBs, logs, IDEs, artifacts)
- Frontend JS modularized into 12 separate files
- Multi-provider LLM system (Anthropic Go SDK, Node SDK, Ollama)
- OpenTelemetry instrumented for observability
- Theme system (light/dark) implemented
- Automated deploy script (`scripts/deploy.sh`)

### What needs attention

- No documentation (README, CONTRIBUTING, etc.)
- Minimal test coverage (only 2 test files)
- No CI/CD configured
- Portuguese strings still present in the frontend (7 strings)
- Debug code in production
- Personal domain hardcoded in the code

---

## 2. Critical Blockers

Without resolving these items, the project **should not** be published.

### 2.1 License

| Item | Status |
|------|--------|
| `LICENSE` | **Resolved** — MIT License added |

### 2.2 Missing README

| Item | Status |
|------|--------|
| `README.md` | **Does not exist** |

The README is the project's front door. Without it, visitors do not know what the project does, how to install it, how to use it, or how to contribute. A project without a README on GitHub gets ignored.

### 2.3 Insufficient tests

| Item | Status |
|------|--------|
| `*_test.go` files | **2 found** (was 0) |
| Test coverage | **Minimal** |
| `make test` | Works with `go test -v ./...` |
| `make test-coverage` | Added with HTML report |

Existing test files:
- `internal/llm/provider_test.go`
- `internal/database/session_task_test.go`

Progress compared to the previous audit (was 0 tests), but coverage is still very low. Without comprehensive tests, external contributions remain risky.

### 2.4 No CI/CD

| Item | Status |
|------|--------|
| `.github/workflows/` | **Does not exist** |
| GitHub Actions | **Not configured** |

Without CI, PRs cannot be validated automatically. Maintainers would have to manually build and test each contribution.

### 2.5 Missing contribution guide

| Item | Status |
|------|--------|
| `CONTRIBUTING.md` | **Does not exist** |

International contributors will not know how to set up the environment, what the expected code style is, or how to submit PRs.

---

## 3. Security

### 3.1 Personal domain hardcoded

**File:** `internal/config/config.go`

```go
VAPIDEmail: getEnv("VAPID_EMAIL", "admin@openpoet.minhapalavra.com.br"),
```

**Risk:** Exposes a personal domain in the public code.
**Fix:** Replace with `admin@example.com` or remove the default.

### 3.2 SSH without host key verification

**File:** `internal/session/remote.go`

```go
HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: Add proper host key verification
```

**Risk:** Vulnerable to man-in-the-middle attacks on remote SSH connections.
**Fix:** Implement verification or, at a minimum, document the risk with a warning in the log.

### 3.3 Debug endpoints exposed

**File:** `cmd/openpoet/main.go`

- Endpoint `/api/debug/client-error` accessible without authentication
- Debug middleware logging every static file request with User-Agent
- Verbose `[DEBUG-REQ]` logging on each request

**Risk:** Log pollution, potential information disclosure.
**Fix:** Remove or place behind a `--debug` flag / environment variable.

### 3.4 Fully open CORS

**Risk:** `Access-Control-Allow-Origin: *` allows requests from any origin.
**Fix:** Document that it is intentional (local tool) or restrict it.

### 3.5 Weak default encryption key

**File:** `internal/security/encryption.go` / `internal/config/config.go`

When `OPENPOET_ENCRYPT_KEY` is not set, the key is derived from:

```go
key = fmt.Sprintf("openpoet-%s-%s-default-key", hostname, username)
```

**Risk:** Predictable on known machines.
**Fix:** Clearly document that a strong key should be set. Consider automatically generating one on first boot and saving it to a file.

---

## 4. Internationalization (i18n)

### 4.1 Current state

| Layer | Language | Status |
|-------|----------|--------|
| Backend (Go) | 100% English | OK |
| Frontend (JS) | 99% English | OK |
| HTML Templates | Mixed PT/EN | **Needs fix** |
| CLAUDE.md | Mixed PT/EN | **Needs standardization** |
| manifest.json | English | OK |
| Service Worker | English | OK |

### 4.2 Portuguese strings found (7 total)

All in `web/templates/index.html`:

| Line | String in PT | EN Translation |
|------|-------------|----------------|
| ~289 | `title="Encerrar sessão"` | `title="End session"` |
| ~449 | `"Editar texto"` | `"Edit text"` |
| ~459 | `"Enviar"` (mobile editor button) | `"Send"` |
| ~462 | `placeholder="Digite ou dite seu texto..."` | `placeholder="Type or dictate your text..."` |
| ~639 | `"Enviar"` (AI chat button) | `"Send"` |
| ~642 | `placeholder="Digite ou dite sua mensagem..."` | `placeholder="Type or dictate your message..."` |
| ~766 | `"Enviar"` (voice indicator) | `"Send"` |

### 4.3 i18n system

**Status:** Not implemented. All user-facing strings are hardcoded.

**String distribution (updated estimate):**

| Type | Estimated count | Location |
|------|----------------|----------|
| UI labels (buttons, titles) | ~40 | `index.html` |
| Toast notifications | ~30 | `app.js`, JS modules |
| Error/success messages | ~25 | Various `.js` |
| Placeholders and hints | ~15 | `index.html`, `.js` |
| Backend messages | ~90 | Various `.go` |
| **Total** | **~200+** | |

### 4.4 i18n recommendation

**Immediate phase (pre-launch):**
- Translate the 7 PT strings to EN
- Standardize all UI in English

**Later phase (post-launch):**
- Implement an i18n system with JSON files (`translations/en.json`, `translations/pt-BR.json`)
- For the frontend: a simple system based on `data-i18n` attributes or a library like `i18next`
- For the backend: `golang.org/x/text` or a custom system with JSON
- Add a language switcher in the UI
- Document the translation process in `CONTRIBUTING.md`

---

## 5. Required Documentation

### 5.1 Required documents

| Document | Purpose | Status |
|----------|---------|--------|
| `LICENSE` | Legal permission for use and contribution | **Created** (MIT) |
| `README.md` | Project description, install, usage, screenshots | **Does not exist** |
| `CONTRIBUTING.md` | How to contribute, dev setup, code style, PR process | **Does not exist** |

### 5.2 Strongly recommended documents

| Document | Purpose | Status |
|----------|---------|--------|
| `CODE_OF_CONDUCT.md` | Community behavior standards (Contributor Covenant) | **Does not exist** |
| `SECURITY.md` | How to responsibly report vulnerabilities | **Does not exist** |
| `CHANGELOG.md` | Change history by version | **Does not exist** |

### 5.3 Recommended documents

| Document | Purpose | Status |
|----------|---------|--------|
| `.github/ISSUE_TEMPLATE/bug_report.md` | Template for reporting bugs | **Does not exist** |
| `.github/ISSUE_TEMPLATE/feature_request.md` | Template for suggesting features | **Does not exist** |
| `.github/PULL_REQUEST_TEMPLATE.md` | Checklist for PRs | **Does not exist** |
| `docs/ARCHITECTURE.md` | Architecture overview | **Does not exist** |
| `docs/API.md` | REST/WebSocket API documentation | **Does not exist** |

### 5.4 Suggested content for the README

```
- Project name and tagline
- Badges (build, license, Go version)
- Screenshot or demo GIF
- What it is / what it does
- Main features (including multi-provider LLM, mobile PWA, MCP servers)
- Requirements (Go 1.22+, Node.js for sidecar SDK, etc.)
- Installation (from source, binary release)
- Configuration (environment variables)
- Basic usage
- Architecture summary (13 internal packages, xterm.js, WebSocket)
- How to contribute (link to CONTRIBUTING.md)
- License (MIT)
- Credits
```

---

## 6. Project Infrastructure

### 6.1 Docker

| Item | Status |
|------|--------|
| `Dockerfile` | **Does not exist** |
| `docker-compose.yml` | **Does not exist** |

Docker is nearly mandatory for adoption. OpenPoet has PTY/terminal dependencies and now also the Node.js sidecar, which makes setup even less trivial — a Dockerfile would solve this.

### 6.2 CI/CD (GitHub Actions)

**Recommended workflows:**

| Workflow | Trigger | What it does |
|----------|---------|--------------|
| `build.yml` | Push, PR | `make build` on Linux and macOS |
| `lint.yml` | Push, PR | `make fmt` + `make lint` (golangci-lint) |
| `test.yml` | Push, PR | `make test` |
| `release.yml` | Tag `v*` | Multi-platform build + GitHub Release |

### 6.3 Versioning

| Item | Status |
|------|--------|
| Version tags | **None** |
| Semantic versioning | **Not adopted** |
| Build version | Embedded via `git rev-parse --short HEAD` in the Makefile |

**Recommendation:** Create tag `v1.0.0` for launch. Use [Semantic Versioning](https://semver.org/).

### 6.4 Release binaries

The Makefile already supports `make build-all` for multiple platforms:
- `darwin/amd64` (macOS Intel)
- `darwin/arm64` (macOS Apple Silicon)
- `linux/amd64`
- `linux/arm64`

**Recommendation:** Automate with [GoReleaser](https://goreleaser.com/) to generate GitHub releases automatically from tags.

### 6.5 Deploy

The project now has an automated deploy script (`scripts/deploy.sh`) with Makefile targets:
- `make deploy` — Deploy to production (port 8081)
- `make deploy-status` — Status of the last deploy
- `make deploy-log` — Recent deploy log

---

## 7. Code Quality

### 7.1 Strengths

- **Package organization** — `internal/` structure with 13 well-separated modules:
  - `benchmark` — AI session benchmarking
  - `config` — Configuration via environment variables
  - `configsync` — Skills and hooks synchronization to projects
  - `database` — SQLite models and migrations
  - `files` — File transfer (local and SFTP)
  - `handlers` — HTTP/WebSocket handlers (API, AI, terminal, OpenTelemetry)
  - `llm` — Multi-provider LLM (Anthropic Go SDK, Node SDK sidecar, Ollama)
  - `mcp` — MCP server integration
  - `notifications` — Web Push notifications
  - `security` — AES-256-GCM encryption
  - `session` — Claude Code session management (local and SSH)
  - `voice` — Voice transcription (Whisper)
  - `websocket` — Hub/client pattern for real-time communication
- **Migrations** — Versioned, transactional, idempotent system
- **Encryption** — AES-256-GCM with random nonce for sensitive data
- **WebSocket** — Well-implemented hub/client pattern
- **Makefile** — Comprehensive with targets for build, lint, fmt, clean, vendor, deploy, test-coverage, and sidecar
- **Go dependencies** — Solid and stable choices
- **Modularized frontend** — JS split into 12 files (app.js, ai-chat.js, doc-viewer.js, file-viewer.js, files.js, hooks.js, longpress-tooltip.js, notif-badge.js, notifications.js, terminal.js, theme.js, voice.js)
- **Themes** — Light/dark system implemented (`themes.css`, `theme.js`)
- **OpenTelemetry** — Instrumented for observability (`otel.go`)
- **LLM Pricing** — Cost calculator per provider (`pricing.go`)
- **First tests** — 2 existing test files

### 7.2 Areas of concern

| Item | Detail |
|------|--------|
| **Tests** | Only 2 test files — coverage is still very low |
| **app.js** | ~358KB in a single file — has grown significantly (was ~107KB), but features have been extracted into separate modules |
| **index.html** | ~73KB — monolithic template (was ~57KB) |
| **Debug code** | Debug middleware and endpoints mixed with production code |
| **Error page** | `migrationErrorPageHTML` in Portuguese inside `main.go` |
| **Node.js sidecar** | Additional runtime dependency (Node.js) for the Claude Agent SDK provider |

### 7.3 Go dependencies (direct)

| Dependency | Version | Purpose |
|------------|---------|---------|
| `github.com/go-chi/chi/v5` | v5.1.0 | HTTP routing |
| `github.com/coder/websocket` | v1.8.12 | WebSocket |
| `modernc.org/sqlite` | v1.33.1 | SQLite (pure Go) |
| `github.com/sashabaranov/go-openai` | v1.29.2 | OpenAI-compatible API client |
| `github.com/creack/pty` | v1.1.21 | PTY management |
| `github.com/pkg/sftp` | v1.13.6 | SFTP transfers |
| `golang.org/x/crypto` | v0.28.0 | SSH/crypto |
| `github.com/SherClockHolmes/webpush-go` | v1.3.0 | Web Push notifications |
| `github.com/google/uuid` | v1.6.0 | UUID generation |
| `github.com/jmoiron/sqlx` | v1.4.0 | SQL extensions |

### 7.4 Go dependencies (relevant indirect)

| Dependency | Version | Purpose |
|------------|---------|---------|
| `github.com/severity1/claude-agent-sdk-go` | v0.6.12 | Claude Agent SDK (Go) |
| `github.com/golang-jwt/jwt` | v3.2.2 | JWT tokens |
| `github.com/hashicorp/golang-lru/v2` | v2.0.7 | LRU cache |

### 7.5 Node.js dependencies (sidecar)

| Dependency | Version | Purpose |
|------------|---------|---------|
| `@anthropic-ai/claude-agent-sdk` | ^0.1.0 | Claude Agent SDK (Node.js) |
| `zod` | ^3.24.1 | Schema validation |

### 7.6 Frontend dependencies (vendor)

| Dependency | Version | Purpose |
|------------|---------|---------|
| xterm.js | 5.3.0 | Terminal emulator |
| xterm-addon-fit | 0.8.0 | Terminal auto-resize |
| xterm-addon-web-links | 0.9.0 | Clickable links in the terminal |

All dependencies are stable and well-maintained. No licensing concerns.

---

## 8. License Choice

### 8.1 Decision

**License chosen: MIT** — `LICENSE` file created with copyright by Miquéias Dernier.

### 8.2 Reference

| License | Adoption | Protection | Companies | Contributions |
|---------|----------|------------|-----------|---------------|
| **MIT** (chosen) | Maximum | Minimal — allows proprietary use | Love it | Maximum |
| Apache 2.0 | High | Patent protection | Accept well | High |
| GPL 3.0 | Medium | Forks must be open source | Avoid | Medium |
| AGPL 3.0 | Low | Includes network use (SaaS) | Strongly avoid | Low |

---

## 9. Action Plan

### Phase 1 — Minimum to publish

- [x] Choose and add `LICENSE` file
- [ ] Create `README.md` in English with description, features, install, and screenshots
- [ ] Translate the 7 Portuguese strings to English in `index.html`
- [ ] Translate `migrationErrorPageHTML` in `main.go` to English
- [ ] Remove hardcoded domain `minhapalavra.com.br` from `config.go`
- [ ] Remove or disable debug endpoints and verbose logging middleware
- [ ] Rewrite `CLAUDE.md` in English (or create a bilingual version)
- [ ] Clean up stray files in the root (`hello-world.txt`, `baby-names.txt`, `nohup*.out`, `plan.md`)
- [ ] Ensure `.gitignore` covers all artifacts (`.openpoet/`, `nohup*.out`)
- [ ] Create tag `v1.0.0`

### Phase 2 — Foundation for community

- [ ] Create `CONTRIBUTING.md` in English
- [ ] Create `CODE_OF_CONDUCT.md` (Contributor Covenant)
- [ ] Create `SECURITY.md`
- [ ] Set up GitHub Actions (build + lint + test)
- [ ] Create issue templates (bug report, feature request)
- [ ] Create PR template
- [ ] Create a basic `Dockerfile` (consider Node.js dependency for sidecar)
- [ ] Write tests for the main HTTP handlers
- [ ] Write tests for the migration system
- [ ] Write tests for the LLM providers
- [ ] Add badges to README (build status, license, Go version)

### Phase 3 — Maturity

- [ ] Implement a complete i18n system (frontend + backend)
- [ ] Reach test coverage >50%
- [ ] Create API documentation (`docs/API.md`)
- [ ] Create architecture documentation (`docs/ARCHITECTURE.md`)
- [ ] Set up GoReleaser for automated releases
- [ ] Create `docker-compose.yml` for development
- [ ] Expand `CONTRIBUTING.md` with a detailed development guide
- [ ] Create `ROADMAP.md` with a future vision for the project
- [ ] Set up GitHub Discussions for the community
- [ ] Document the multi-provider LLM system and how to add new providers

---

## 10. Final Checklist

Before running `git push` to the world:

### Security
- [ ] No credentials, API keys, or passwords in the code
- [ ] No personal domains hardcoded
- [ ] Debug code removed or disabled
- [ ] `.gitignore` covers all sensitive files
- [ ] Git history does not contain secrets (review with `git log --all -p | grep -i "password\|secret\|key"`)

### Documentation
- [x] `LICENSE` present
- [ ] `README.md` in English, complete and attractive
- [ ] `CONTRIBUTING.md` with clear instructions
- [ ] Environment variables documented

### Code
- [ ] All UI in English
- [ ] Relevant comments in English
- [ ] `go mod tidy` executed
- [ ] `make lint` passes without errors
- [ ] `make build` works on a clean checkout

### Git
- [ ] `main` branch clean and stable
- [ ] Reasonable commit history (no commits like "fix fix fix")
- [ ] Version tag created (`v1.0.0`)
- [ ] `.github/` configured (Actions, templates)

---

> **Note:** This document was generated as part of the open source readiness audit and updated on 2026-02-17. It should be removed or moved to `docs/` before publication.
