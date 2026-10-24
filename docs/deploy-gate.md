# Deploy gate: nothing ships unless it is committed and pushed

Every path that releases or deploys OpenPoet runs the mandatory gate
`ops/deploy-gate/gate.sh` **before acting**. The goal is simple: whatever runs
in production (or is published as a release) must be reproducible from a
commit on `main` that is also on the public remote.

## What the gate requires

The gate passes only when all three conditions hold:

1. **Clean working tree:** `git status --porcelain` is empty. Modified,
   staged, deleted and **untracked** files all fail the gate. Files covered by
   `.gitignore` (build output, local databases, ...) do not count.
2. **Commit on `main`:** the commit being deployed (`HEAD`, or `--commit`) is
   contained in the local `main` branch.
3. **Commit on the remote:** the same commit is contained in `origin/main`,
   after a fresh `git fetch`. If the fetch fails (no network, no credentials),
   the gate **fails**. It never assumes the commit was pushed.

On failure the command exits with code 1, does nothing else, and lists every
reason and every pending file:

```
==================================================================
DEPLOY GATE FAILED (deploy) — nothing was deployed or restarted.
Rule: no deploy without everything committed on main and pushed.
Repository: /home/dev/openpoet
Commit:     59b7d567333c
  ✗ working tree has uncommitted changes (git status --porcelain is not empty)
  ✗ commit 59b7d567333c is not on 'origin/main' (git push origin main first)
Uncommitted files (git status --porcelain):
    ?? ops/deploy-gate/
Fix: commit (and push) everything, or discard what is not meant to ship,
then run the deploy again. See docs/deploy-gate.md.
==================================================================
```

To check without deploying anything: `make deploy-gate`.

## Usage

```
ops/deploy-gate/gate.sh [--repo DIR] [--commit REV] [--context LABEL]
                        [--log FILE] [--emergency-bypass REASON]
```

Exit codes: `0` passed (or bypassed), `1` gate failed, `2` usage error.

## Where the gate runs

| Path | When | Bypass |
|---|---|---|
| GitHub Actions `release.yml` (tags `v*`) | before GoReleaser: the tag must point at a commit on `main` | none |
| `ops/safe-rollout prepare` | before tests and the build | none |
| `ops/safe-rollout apply` | before the preflight and before stopping the running service, against the manifest's `git_sha` | none |
| Local deploy scripts | before building and again right before replacing the running binary | `--emergency-bypass "<reason>"` |

A deploy script that calls the gate should **fail closed**: if
`ops/deploy-gate/gate.sh` is missing, it must refuse to deploy.

### Out of scope

- **The development server:** it exists to run uncommitted code and never
  touches production.
- **Restarting the same binary** (a service manager restart, a reboot): no new
  code reaches production that way, and gating it would keep a crashed service
  down just because the repository had a dirty file.
- **In-app updates (`update.apply`):** they install a GitHub release binary,
  which only exists if it went through the gate in `release.yml`.

## Emergency bypass

There is exactly one bypass, the `--emergency-bypass "<reason>"` flag. It is
explicit and logged. No environment variable disables the gate.

- The reason is mandatory and must be at least 10 characters long.
- All checks still run and are printed, including the list of pending files.
- Every use appends one JSON line (`time`, `context`, `user`, `session`,
  `commit`, `reason`, `failures`, `dirty`) to the bypass log, by default
  `openpoet-deploy-gate-bypass.log` inside the repository's git directory
  (override with `--log`). If the log cannot be written, the bypass is refused.
- **Automated agents never use the bypass on their own.** Only with an explicit
  instruction from the maintainer for that specific deploy, quoting the reason
  they gave.
- After the emergency: commit and push what was deployed.

## Tests

`make test-deploy-gate` runs:

- `ops/deploy-gate/gate_test.sh`: the gate against throwaway git repositories
  with a local bare `origin`. It covers a clean tree, ignored, modified,
  untracked and staged files, an unpushed commit, a commit only on a feature
  branch, an older commit already on `main`, a nonexistent `--commit`, an
  unreachable origin, a bypass without a reason, a reason that is too short, a
  logged bypass, and an environment variable that does not disable the gate.
- `ops/deploy-gate/deploy_sh_test.sh`: checks that a local deploy script keeps
  calling the gate, with the service manager and build tools replaced by stubs.
  It is skipped when no local deploy script exists (for example on a fresh
  clone or in CI).

Both suites also run as part of `go test ./...` through
`ops/deploy-gate/gate_test.go`. The gate cases for safe-rollout live in
`ops/safe-rollout/plan_test.go`.
