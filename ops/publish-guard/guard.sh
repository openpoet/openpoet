#!/usr/bin/env bash
#
# guard.sh - publication guard for OpenPoet (an open-source repository).
#
# Nothing compromising may enter a commit or reach the remote: secrets,
# tokens, internal IPs/hosts, personal data, debug images/captures, internal
# docs, dumps, logs, .env files, binaries. This guard enforces what can be
# checked mechanically; the human/agent review of the diff is still required
# (see docs/publish-guard.md).
#
# Modes:
#   guard.sh staged                  staged changes (pre-commit hook)
#   guard.sh range <git-log-args>    commits in a range, e.g. origin/main..main
#   guard.sh pre-push <remote>       reads git's pre-push stdin; checks every
#                                    commit not yet on the remote
#   guard.sh unpushed [remote]       commits on HEAD not on <remote> (origin)
#   guard.sh history                 the whole history (all refs)
#
# Checks:
#   1. gitleaks (secret scanner) over the same changes. Missing gitleaks
#      fails closed: install with `make tools`.
#   2. added lines (and, for ranges, commit messages) must not contain:
#      private IPv4 addresses (10/8, 172.16/12, 192.168/16), this machine's
#      home directory or user paths, or any pattern in the LOCAL denylist
#      (client names, internal domains), one extended regex per line, read
#      from $(git rev-parse --git-common-dir)/info/publish-denylist and
#      ~/.config/openpoet/publish-denylist. The denylist is never versioned.
#      A line containing the marker `publish-guard:allow` is skipped (visible
#      in review; use only for deliberate fixtures).
#   3. added/modified files must not be .env files, keys/certificates,
#      databases, logs, dumps, HAR captures, token files; images/videos/PDFs
#      only under docs/images/ or web/static/; no binary file over 512 KiB and
#      no file over 5 MiB.
#
# There is no bypass in this script. `git commit/push --no-verify` skips the
# hooks, and agents must never use it without an explicit order from the user.
#
# Exit codes: 0 clean, 1 findings, 2 usage/tooling error.
#

set -uo pipefail

MODE="${1:-}"; shift || true
REPO="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "publish-guard: not a git repository" >&2; exit 2; }
cd "$REPO" || exit 2

FINDINGS=()
add() { FINDINGS+=("$1"); }

# ── patterns ──────────────────────────────────────────────────────────
PRIVATE_IP='(^|[^0-9.])(10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3})([^0-9]|$)'
LITERALS=()
if [ -n "${HOME:-}" ] && [ "$HOME" != "/" ] && [ "${#HOME}" -ge 6 ]; then LITERALS+=("$HOME"); fi
ME="$(id -un 2>/dev/null || true)"
if [ "${#ME}" -ge 3 ]; then LITERALS+=("/home/$ME" "/Users/$ME" "\\Users\\$ME"); fi
DENYLIST=()
for f in "$(git rev-parse --git-common-dir)/info/publish-denylist" "${HOME:-/nonexistent}/.config/openpoet/publish-denylist"; do
    [ -f "$f" ] || continue
    while IFS= read -r line || [ -n "$line" ]; do
        line="${line%%$'\r'}"
        case "$line" in ''|'#'*) continue ;; esac
        DENYLIST+=("$line")
    done < "$f"
done

# check_text LABEL TEXT: content checks over added lines / messages
check_text() {
    local label="$1" text="$2" hit l
    [ -n "$text" ] || return 0
    text="$(printf '%s\n' "$text" | grep -v 'publish-guard:allow' || true)"
    [ -n "$text" ] || return 0
    hit="$(printf '%s\n' "$text" | grep -a -n -E -- "$PRIVATE_IP" | head -3 | cut -c1-160)"
    [ -n "$hit" ] && add "$label: private IP address: $(printf '%s' "$hit" | tr '\n' ' ')"
    for l in "${LITERALS[@]}"; do
        hit="$(printf '%s\n' "$text" | grep -a -n -F -- "$l" | head -3 | cut -c1-160)"
        [ -n "$hit" ] && add "$label: this machine's user/home path ($l): $(printf '%s' "$hit" | tr '\n' ' ')"
    done
    for l in "${DENYLIST[@]}"; do
        hit="$(printf '%s\n' "$text" | grep -a -n -i -E -- "$l" | head -3 | cut -c1-160)"
        [ -n "$hit" ] && add "$label: matches the local denylist (/$l/): $(printf '%s' "$hit" | tr '\n' ' ')"
    done
    return 0
}

# check_file PATH BLOB: file-level checks for an added/modified path
check_file() {
    local path="$1" blob="$2" base size
    base="$(basename "$path")"
    case "$base" in
        .env.example|.env.sample|.env.template) ;;
        .env|.env.*|*.env) add "$path: .env file" ;;
    esac
    case "$base" in
        *.pem|*.key|*.p12|*.pfx|*.jks|*.keystore|id_rsa*|id_ed25519*|id_ecdsa*|*.ppk) add "$path: key/certificate file" ;;
        *.db|*.sqlite|*.sqlite3|*.db-wal|*.db-shm|*.db-journal) add "$path: database file" ;;
        *.log|nohup*.out) add "$path: log file" ;;
        *.har|*.dump|*.hprof|core|core.[0-9]*|*.pcap) add "$path: dump/capture file" ;;
        *.token|*token.txt|*.secret|*.secrets) add "$path: token/secret file" ;;
        *.bak|*.sql|*.sql.gz) add "$path: backup/SQL dump" ;;
    esac
    case "$base" in
        *.png|*.jpg|*.jpeg|*.gif|*.webp|*.bmp|*.tif|*.tiff|*.heic|*.mp4|*.mov|*.webm|*.pdf)
            case "$path" in
                docs/images/*|web/static/*) ;;
                *) add "$path: image/video/PDF outside docs/images/ and web/static/ (debug captures never go in)" ;;
            esac ;;
    esac
    [ -n "$blob" ] || return 0
    size="$(git cat-file -s "$blob" 2>/dev/null || echo 0)"
    if [ "$size" -gt 5242880 ]; then
        add "$path: file larger than 5 MiB ($size bytes)"
    elif [ "$size" -gt 524288 ] && [ "$(git cat-file blob "$blob" | head -c 8192 | tr -d -c '\000' | wc -c)" -gt 0 ]; then
        add "$path: binary file larger than 512 KiB ($size bytes)"
    fi
    return 0
}

# added lines of a diff, grouped per file, fed to check_text
check_diff() {
    local label="$1"; shift
    local current="" buf="" line
    while IFS= read -r line; do
        case "$line" in
            '+++ b/'*) [ -n "$current" ] && check_text "$label $current" "$buf"; current="${line#+++ b/}"; buf="" ;;
            '+++ '*) [ -n "$current" ] && check_text "$label $current" "$buf"; current=""; buf="" ;;
            '+'*) [ -n "$current" ] && buf+="${line#+}"$'\n' ;;
        esac
    done < <("$@")
    [ -n "$current" ] && check_text "$label $current" "$buf"
    return 0
}

need_gitleaks() {
    if ! command -v gitleaks >/dev/null 2>&1; then
        echo "publish-guard: gitleaks is not installed — refusing (fail closed). Install: make tools" >&2
        exit 2
    fi
}

run_gitleaks() {
    local out rc=0
    out="$(gitleaks "$@" --redact --no-banner --exit-code 1 2>&1)" || rc=$?
    if [ "$rc" -eq 1 ]; then
        add "gitleaks found secrets:"$'\n'"$(printf '%s\n' "$out" | grep -E 'Finding:|Secret:|RuleID:|File:|Line:|Commit:' | sed 's/^/      /')"
    elif [ "$rc" -ne 0 ]; then
        echo "publish-guard: gitleaks failed (exit $rc): $out" >&2; exit 2
    fi
}

check_commits() {
    # $@: git rev-list arguments
    local c
    for c in $(git rev-list --reverse "$@"); do
        local short="${c:0:10}"
        check_text "commit $short message" "$(git log -1 --format=%B "$c")"
        check_diff "commit $short" git show --format= --unified=0 --no-color --no-ext-diff "$c"
        while IFS=$'\t' read -r st path rest; do
            [ -n "$path" ] || continue
            case "$st" in R*|C*) path="$rest" ;; D*) continue ;; esac
            check_file "$path" "$(git rev-parse --verify --quiet "$c:$path" 2>/dev/null || true)"
        done < <(git show --format= --name-status --no-color "$c" 2>/dev/null)
    done
}

report() {
    local what="$1"
    if [ "${#FINDINGS[@]}" -eq 0 ]; then
        echo "publish-guard ($what): OK"
        exit 0
    fi
    {
        echo "=================================================================="
        echo "PUBLISH GUARD FAILED ($what) — OpenPoet is open source."
        echo "Nothing compromising may be committed or pushed:"
        local f; for f in "${FINDINGS[@]}"; do echo "  ✗ $f"; done
        echo "Fix the content (or rewrite local history before pushing)."
        echo "See docs/publish-guard.md. Never --no-verify without an explicit order."
        echo "=================================================================="
    } >&2
    exit 1
}

case "$MODE" in
    staged)
        need_gitleaks
        run_gitleaks git --pre-commit --staged .
        check_diff "staged" git diff --cached --unified=0 --no-color --no-ext-diff
        while IFS=$'\t' read -r st path rest; do
            [ -n "$path" ] || continue
            case "$st" in R*|C*) path="$rest" ;; D*) continue ;; esac
            check_file "$path" "$(git rev-parse --verify --quiet ":$path" 2>/dev/null || true)"
        done < <(git diff --cached --name-status --no-color)
        report "staged changes"
        ;;
    range)
        [ $# -gt 0 ] || { echo "usage: guard.sh range <git-log-args>" >&2; exit 2; }
        need_gitleaks
        [ -n "$(git rev-list "$@" 2>/dev/null | head -1)" ] || report "range $* (empty)"
        run_gitleaks git --log-opts="$*" .
        check_commits "$@"
        report "range $*"
        ;;
    unpushed)
        remote="${1:-origin}"
        git fetch --quiet "$remote" 2>/dev/null || { echo "publish-guard: could not fetch $remote (fail closed)" >&2; exit 2; }
        exec "$0" range HEAD --not --remotes="$remote"
        ;;
    pre-push)
        remote="${1:-origin}"
        zero="0000000000000000000000000000000000000000"
        need_gitleaks
        ranges=()
        while read -r lref lsha rref rsha; do
            [ -n "${lsha:-}" ] || continue
            [ "$lsha" = "$zero" ] && continue               # deleting a remote ref
            if [ "$rsha" = "$zero" ] || ! git cat-file -e "$rsha^{commit}" 2>/dev/null; then
                ranges+=("$lsha --not --remotes=$remote")
            else
                ranges+=("$rsha..$lsha")
            fi
        done
        [ "${#ranges[@]}" -gt 0 ] || report "pre-push (nothing to push)"
        for r in "${ranges[@]}"; do
            # shellcheck disable=SC2086
            [ -n "$(git rev-list $r | head -1)" ] || continue
            run_gitleaks git --log-opts="$r" .
            # shellcheck disable=SC2086
            check_commits $r
        done
        report "pre-push to $remote"
        ;;
    history)
        need_gitleaks
        run_gitleaks git --log-opts="--all" .
        check_commits --all
        report "full history"
        ;;
    -h|--help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    "") sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
    *) echo "publish-guard: unknown mode: $MODE" >&2; exit 2 ;;
esac
