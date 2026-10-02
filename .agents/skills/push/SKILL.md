---
name: push
description: Push OpenPoet commits to the remote (GitHub, public). Use when the user asks to push, fazer push, enviar/subir para o remoto/GitHub, publicar os commits, or open a PR from local commits.
allowed-tools: Bash(git *), Bash(make publish-guard), Bash(make secret-scan), Bash(./ops/publish-guard/guard.sh *)
---

# Push Skill

OpenPoet is **open source** (`openpoet/openpoet` is public). Whatever reaches
the remote must be treated as published forever (forks, clones, caches).
Policy: `docs/publish-guard.md`.

## Rules

1. **Only push with the user's request/approval** for this push.
2. **Nothing compromising reaches the remote:** secrets, tokens, keys,
   passwords, internal IPs/hosts, personal data (machine user/home paths,
   client or client-project names, private e-mails), debug images and
   screenshots, internal docs (incident reports, assessments, internal
   session/task IDs), dumps, logs, `.db`, `.env`, binaries.
3. **What is in `.gitignore` stays in `.gitignore`.** Never `git add -f`
   `.scripts/`, `CLAUDE.md`, `.claude/`, `.run/` or local DBs.
4. **A secret that already reached the remote must be rotated** (revoke and
   replace the credential). Rewriting history is not enough; list it for the
   user.
5. **Never `--no-verify`, never plain `--force`.** If published history must
   change, use `--force-with-lease` only after telling the user and getting
   approval, explaining the impact on forks/clones (they must re-clone or
   hard-reset; old SHAs stay in forks).

## Process

1. `git fetch origin` and list what would be pushed:
   `git log --oneline origin/main..main` (or the branch being pushed).
2. **Review every unpushed commit** — `git log -p origin/main..main` — diff
   and message, against rule 2.
3. **Run the scanner:** `make publish-guard` (gitleaks + IPs/paths/local
   denylist/forbidden files over every unpushed commit). It must say `OK`.
   Missing gitleaks fails closed: `make tools`.
4. **If anything is found, rewrite the LOCAL history before pushing**:
   - first a full backup outside the repo:
     `git bundle create <dir-outside-the-repo>/openpoet-<date>.bundle --all`;
   - `git commit --amend` / `git rebase -i` for recent commits, or
     `git filter-repo` (`--replace-text`, `--invert-paths --path`) for
     older ones; check that `origin/main` is still an ancestor
     (`git merge-base --is-ancestor origin/main main`), i.e. published
     history did not change;
   - run `make publish-guard` again.
   If the finding is already on the remote, stop and report it to the user
   (rotation per rule 4; any published-history rewrite per rule 5).
5. Push: `git push origin main` (the pre-push hook runs the guard again).
   Set the upstream once if missing: `git push -u origin main`.
6. Confirm: `git status -sb` shows `main...origin/main` with nothing ahead.

$ARGUMENTS
