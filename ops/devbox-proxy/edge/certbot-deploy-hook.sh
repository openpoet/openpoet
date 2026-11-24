#!/bin/sh
# Certbot deploy hook (renewal-hooks/deploy/): runs inside the certbot
# container after a certificate was renewed. The container cannot reach
# Docker, so it only asks the host to reload nginx by rewriting a request
# file on the shared volume; nginx-reload.path on the host does the reload.
#
# Only the lineages listed (one per line) in nginx-reload.lineages, next to
# the renewal-hooks directory, request a reload.
set -eu
dir="${CERTBOT_CONFIG_DIR:-/etc/letsencrypt}"
list="$dir/renewal-hooks/nginx-reload.lineages"
lineage="$(basename "${RENEWED_LINEAGE:-}")"
[ -n "$lineage" ] || { echo "nginx-reload hook: RENEWED_LINEAGE not set" >&2; exit 0; }
if ! grep -qxF "$lineage" "$list" 2>/dev/null; then
	echo "nginx-reload hook: $lineage not listed, no reload requested"
	exit 0
fi
tmp="$dir/.nginx-reload.request.$$"
printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$lineage" > "$tmp"
mv -f "$tmp" "$dir/nginx-reload.request"
echo "nginx-reload hook: reload requested for $lineage"
