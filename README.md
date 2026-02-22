<p align="center">
  <img src="mascot.svg" alt="OpenPoet Mascot" width="200" />
</p>

# OpenPoet

Orchestrate Claude Code sessions across multiple projects from a single web interface.

**[openpoet.ai](https://openpoet.ai)**

## Install

### Homebrew (macOS and Linux)

```bash
brew install openpoet/tap/openpoet
```

### winget (Windows)

```powershell
winget install openpoet
```

### Shell script (macOS and Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/openpoet/openpoet/main/install.sh | sh
```

Install a specific version:

```bash
OPENPOET_VERSION=1.0.0 curl -fsSL https://raw.githubusercontent.com/openpoet/openpoet/main/install.sh | sh
```

Install to a custom directory:

```bash
OPENPOET_INSTALL=$HOME/.local/bin curl -fsSL https://raw.githubusercontent.com/openpoet/openpoet/main/install.sh | sh
```

### Manual download

Download the binary for your platform from the [Releases page](https://github.com/openpoet/openpoet/releases).

| Platform | Architecture | Archive |
|---|---|---|
| macOS | Apple Silicon (M1+) | `openpoet_VERSION_darwin_arm64.tar.gz` |
| macOS | Intel | `openpoet_VERSION_darwin_amd64.tar.gz` |
| Linux | x86_64 | `openpoet_VERSION_linux_amd64.tar.gz` |
| Linux | ARM64 | `openpoet_VERSION_linux_arm64.tar.gz` |
| Windows | x86_64 | `openpoet_VERSION_windows_amd64.zip` |
| Windows | ARM64 | `openpoet_VERSION_windows_arm64.zip` |

## Quick Start

```bash
openpoet
# Open http://localhost:8080 in your browser
```

### CLI Options

```
openpoet [flags]
openpoet version       Print version
openpoet mcp-serve     Run as MCP server
openpoet benchmark     Run benchmarks
```

| Flag | Default | Description |
|---|---|---|
| `-port` | `8080` | Port to listen on |
| `-bind` | `0.0.0.0` | Address to bind to |
| `-db` | `openpoet.db` | SQLite database path |
| `-openai-key` | | OpenAI API key for voice transcription |
| `-mcp-http` | `false` | Enable MCP HTTP endpoint at /mcp |

### Requirements

- **Node.js 18+** is required for the Claude Agent SDK provider. The sidecar scripts are embedded in the binary and auto-extracted on first run. If Node.js is not installed, OpenPoet will show a helpful message and continue working with other provider types.

## Features

- **Multi-Project Management** — Manage both local and remote (SSH) projects
- **Claude Code Integration** — Start and manage AI-assisted terminal sessions
- **Skills System** — Create and sync instruction templates for Claude Code
- **MCP Server Configuration** — Configure and inject MCP servers into sessions
- **Task Management** — Track todos, progress, and priorities per project
- **Mobile-Optimized UI** — Full-featured mobile terminal with voice input
- **PWA Support** — Install as a progressive web app with offline capability

## Development

```bash
# Build
make build

# Run in development mode
make dev

# Run tests
make test
```

## License

MIT
