# Security Policy

## Reporting a vulnerability

Please **do not report security vulnerabilities in public issues, pull
requests or discussions.**

Report them privately through GitHub's private vulnerability reporting:
open the repository's **Security** tab and choose
[**Report a vulnerability**](https://github.com/openpoet/openpoet/security/advisories/new).
This creates a private security advisory visible only to you and the
maintainers.

Please include:

- the affected version (`openpoet version`) and platform;
- a description of the issue and its impact;
- steps to reproduce or a proof of concept;
- any suggested fix or mitigation.

We aim to acknowledge reports within a few days, keep you updated while we
work on a fix, and credit you in the advisory unless you prefer otherwise.
Please give us a reasonable amount of time to release a fix before any public
disclosure.

## Supported versions

OpenPoet is pre-1.0 and moves quickly. Security fixes are released in the
latest version only.

| Version | Supported |
|---|---|
| Latest release | Yes |
| Older releases | No, please upgrade |

## Security model

Understanding what OpenPoet does and does not protect against helps you judge
whether something is a vulnerability.

- **Single-user local tool.** OpenPoet is meant to run on your own machine for
  one person. It has **no login**: anyone who can reach its HTTP port can
  control it, including starting agent sessions and running commands in your
  projects with your user's permissions.
- **Localhost by default.** The server binds to `localhost` unless you change
  it with `OPENPOET_BIND` or the `-bind` flag (see
  [`internal/config/config.go`](internal/config/config.go)).
- **Binding to `0.0.0.0` trusts your whole network.** If you bind to all
  interfaces, every device on the LAN can use OpenPoet as you. Only do this on
  a network you fully trust, or put it behind your own authentication layer.
- **Remote access goes through the paired-device tunnel.** To use OpenPoet from
  a phone or another device outside your network, use the built-in tunnel: a
  device must be paired by approving a short-lived code on the local instance,
  can be revoked at any time, and its traffic is encrypted end to end with a
  key shared during pairing.
- **Credentials are encrypted at rest.** SSH passwords and keys, API keys, MCP
  server environments and other secrets stored in the SQLite database are
  encrypted with AES-256-GCM using a key derived from `OPENPOET_ENCRYPT_KEY`.
  When the variable is not set, OpenPoet reads `openpoet.key` next to the
  database, creating it with a random key (mode 0600) on a fresh install.
  Installs that predate the key file keep their legacy hostname/user-derived
  key in that file and log a warning; rotate it with
  `openpoet-secret-migrate rotate-key` (see its `-h`). Back up the key file
  with the database: without it the stored credentials are unreadable.
- **Only this machine's names are accepted.** Requests whose `Host` header is
  not an IP address, `localhost` or the machine's hostname are refused, which
  blocks DNS-rebinding attacks from web pages. If you reach OpenPoet through a
  reverse proxy or a custom domain, list the names in `OPENPOET_ALLOWED_HOSTS`
  (comma-separated). Cross-origin browser requests to the API, `/mcp` and the
  WebSocket streams are refused, and no CORS headers are sent.
- **Agents act with your permissions.** Claude Code, Codex and other agent
  CLIs started by OpenPoet run as your user. OpenPoet's approval flows reduce
  surprises, but they are not a sandbox.

Examples of issues we want to hear about: ways to reach the API or the
terminal without network access to the bound address, tunnel or pairing
bypasses, credentials leaking in plaintext (logs, API responses, the
Automation API), path traversal outside a project, and cross-site attacks
against a localhost instance (CSRF, DNS rebinding, malicious WebSocket
origins).
