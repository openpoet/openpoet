# OpenAI OAuth provider for Claude Code

OpenPoet can run a local or remote Claude Code project against an OpenAI ChatGPT
subscription by using the community `claude-code-proxy` compatibility bridge.
This is not a native Anthropic or OpenAI integration: the bridge translates the
Anthropic Messages protocol used by Claude Code into OpenAI Codex responses.

## Install the pinned bridge

```sh
make provider-helper
```

The installer downloads `claude-code-proxy` v0.1.21 for the current macOS or
Linux architecture, validates a SHA-256 digest pinned in the repository, and
places the executable at `build/claude-code-proxy`. For another installation
layout, place it next to the `openpoet` binary or set
`OPENPOET_CLAUDE_CODE_PROXY_BIN` to an explicit executable path.

Source and release: <https://github.com/raine/claude-code-proxy/releases/tag/v0.1.21>

## Configure OpenPoet

1. Open **Settings → AI Providers** and create an **OpenAI ChatGPT OAuth
   (Claude Code)** profile.
2. Save the profile and choose **Connect**.
3. Open the displayed OpenAI device URL and enter the code manually. OpenPoet
   never opens a browser or reads an existing Codex login.
4. Edit a project, keep **Claude Code** as its backend, select **OpenAI
   ChatGPT OAuth**, choose the connected profile, and set the main and optional
   small model IDs.

For remote projects, OpenPoet opens a reverse SSH listener on the remote
loopback interface and forwards it to the local provider bridge for the
lifetime of the session. The remote Claude Code process receives the tunnel
URL and a random session-scoped bearer credential. The OpenAI OAuth bundle,
helper configuration, and refresh tokens remain on the OpenPoet host.

## Credential isolation

- The OAuth helper receives a newly created private `HOME`, `CCP_CONFIG_DIR`,
  `XDG_CONFIG_HOME`, and `XDG_STATE_HOME`. It cannot discover `~/.codex` or the
  native Codex CLI login through its environment.
- OpenPoet imports the completed OAuth bundle into the encrypted `ai_configs`
  secret columns. API responses expose only `OAuth connected` and connection
  status.
- When a project starts, the credential is decrypted into a process-specific
  temporary directory (`0700`) with an `auth.json` file (`0600`). The bridge
  listens only on `127.0.0.1`; refreshed credentials are encrypted back into
  OpenPoet, and the temporary directory is removed on normal bridge exit or
  OpenPoet shutdown.
- Local Claude Code receives only the loopback URL and a dummy Anthropic token.
  Local sessions do not inherit the OpenPoet parent process environment, and
  common Anthropic/OpenAI credential variables are explicitly cleared for this
  provider.
- Remote Claude Code receives a remote-loopback tunnel URL plus an ephemeral
  bearer credential generated for that session. The tunnel authenticates each
  request before forwarding it over the existing SSH connection and disappears
  when the session stops. No OpenAI or Codex credential is copied to the remote
  host.
- Disconnecting a profile stops its bridge and clears the encrypted OAuth
  bundle. It does not modify the native Codex CLI credential store.

For production, set a strong persistent `OPENPOET_ENCRYPT_KEY`; changing that
key later makes previously encrypted provider credentials unreadable.

## Runtime mapping

For an OpenAI-backed Claude Code session, OpenPoet injects the bridge or tunnel
URL via `ANTHROPIC_BASE_URL`, maps the configured model to Claude's main/default
model variables, maps the optional small model to fast/subagent variables, and
disables nonessential Anthropic traffic. The persisted session harness is
`claude_code/openai`, so GPT model IDs are validated separately from native
Claude model IDs.

ChatGPT subscriptions and OpenAI API billing are separate. This integration
uses the manually authorized ChatGPT OAuth profile; it does not convert a
ChatGPT subscription into an API key or reuse `OPENAI_API_KEY`.

Official background:

- OpenAI authentication: <https://learn.chatgpt.com/docs/auth#openai-authentication>
- ChatGPT versus API billing: <https://help.openai.com/en/articles/9039756-billing-settings-in-chatgpt-vs-platform>
- Anthropic LLM gateway variables: <https://code.claude.com/docs/en/llm-gateway>

Because the protocol bridge is community-maintained, upstream Claude Code or
OpenAI protocol changes can require a bridge update and a new pinned checksum.
