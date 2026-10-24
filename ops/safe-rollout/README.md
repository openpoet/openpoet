# OpenPoet safe rollout

Operational groundwork for preparing and applying releases without compiling
after production has been stopped. The tool is **dry-run by default**. No
mutating command runs without `--execute`; `apply` also requires the exact
token produced by `prepare`.

`prepare` and `apply` go through the mandatory deploy gate
(`ops/deploy-gate/gate.sh`, see `docs/deploy-gate.md`): a clean worktree and
the commit (HEAD for `prepare`, the manifest's `git_sha` for `apply`) present
on `main` and on `origin/main`. There is no bypass here.

This tool does not replace human approval. The project's deploy rules still
apply: a production deploy only happens after direct, explicit authorization
from the user.

## Flow

### 1. Inspect the plan

```bash
go run ./ops/safe-rollout prepare
go run ./ops/safe-rollout preflight
go run ./ops/safe-rollout apply
```

The three commands above write no files, run no tests or builds and do not
touch launchd.

### 2. Prepare a release

The worktree must be clean. Tests run before the artifact is created; the
build receives a `BuildVersion` equal to the release ID.

```bash
go run ./ops/safe-rollout prepare --execute \
  --repo "$PWD" \
  --releases-dir "$HOME/.openpoet-rollout/releases" \
  --relay-url "https://relay.example"
```

Outputs:

- an immutable directory `<releases-dir>/<release-id>/`;
- `openpoet` with its SHA-256 recorded;
- `manifest.json` with mode `0600`;
- a random confirmation token.

Build and tests happen while production is still running. `prepare` refuses a
dirty tree so it never produces a binary that cannot be reproduced.

### 3. Read-only preflight

```bash
go run ./ops/safe-rollout preflight --execute \
  --manifest "$HOME/.openpoet-rollout/releases/<release>/manifest.json" \
  --live-binary "$PWD/.run/openpoet" \
  --db "$PWD/.run/openpoet.db" \
  --plist "$PWD/.run/openpoet.production.plist"
```

Validates the artifact hash, the runtime files, `PRAGMA quick_check` and the
current `/api/version`. It does not change production.

### 4. Authorized apply

Only after explicit authorization from the user:

```bash
go run ./ops/safe-rollout apply --execute \
  --confirm-token '<manifest-token>' \
  --manifest "$HOME/.openpoet-rollout/releases/<release>/manifest.json" \
  --live-binary "$PWD/.run/openpoet" \
  --db "$PWD/.run/openpoet.db" \
  --plist "$PWD/.run/openpoet.production.plist" \
  --backup-dir "$HOME/.openpoet-rollout/backups"
```

Actual order:

1. repeats the preflight;
2. creates a consistent snapshot with `VACUUM INTO` and verifies the backup;
3. runs `launchctl bootout`, which delivers SIGTERM to OpenPoet;
4. waits for `/api/version` to become unavailable, without `kill -9`;
5. keeps the previous binary and swaps in the candidate with an atomic rename;
6. migrates, in a single transaction, legacy MCP servers and custom tools to
   encrypted envelopes using `OPENPOET_ENCRYPT_KEY` (or the fallback the
   runtime already uses);
7. runs `launchctl bootstrap`;
8. requires three consecutive health checks reporting the exact release
   version;
9. repeats `PRAGMA quick_check` on the live database.

If start or health fails after the secrets migration, the previous binary and
the SQLite snapshot are restored before the LaunchAgent is reopened. The
migration output contains counts only.

### Offline preflight of the secrets migration

The dedicated tool requires an explicit path and is dry-run by default. It does
not apply schema migrations and never prints names, commands, env or
ciphertext.

```bash
go run ./cmd/openpoet-secret-migrate --db /path/to/openpoet.db

# Only after a backup and explicit approval:
OPENPOET_ENCRYPT_KEY='same-key-as-the-runtime' \
  go run ./cmd/openpoet-secret-migrate --db /path/to/openpoet.db --execute
```

## Intentional limits

- Two OpenPoet processes never share the SQLite database during apply.
- There is no fallback to `kill -9`; a graceful stop that exceeds the timeout
  aborts.
- Rollback restores the database automatically only when this secrets
  migration ran; other schema migrations still need their own N-1
  compatibility strategy.
- The operator must check that sessions are idle before apply. Auto-restore
  does not guarantee exactly-once for external actions in flight.
- The backup is consistent as of the `VACUUM INTO` instant; if the candidate
  fails after the secrets migration, writes between the snapshot and the
  graceful stop are discarded by the restore. That is why the operator must
  drain activity before apply.
- Releases and backups live outside `.run` and outside the repository by
  default.

## Tests

```bash
go test ./ops/safe-rollout
```

The tests use only temporary directories, a temporary SQLite database, local
HTTP and fake command runners. They never call launchd nor touch the
production port.
