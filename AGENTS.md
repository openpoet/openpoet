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

## Guidelines & Constraints

- **Port 8081 is PRODUCTION** — never use for testing or debugging
- **NEVER deploy without explicit user approval** — The deploy action (killing production process on port 8081, rebuilding, and restarting) must ONLY be executed when the user has given direct, express approval to deploy. Claude Code sessions must NEVER autonomously decide to deploy. If a task description mentions "deploy", the session must still ask the user for confirmation before executing. This applies to all contexts: task completion, commit workflows, and any other scenario.
- Database migrations must be additive (never alter existing migrations)
- All dependencies must use MIT-compatible licenses
- Mobile terminal submit logic must follow 3-step sequence (see CLAUDE.md)
- Mobile-responsive features must work on both touch (long-press) and mouse (hover) devices
- All icon-only buttons MUST have a `title` attribute — the global long-press tooltip framework (`web/static/js/longpress-tooltip.js`) uses it to show tooltips on mobile via document-level event delegation. No per-button JS wiring is needed.
