#!/usr/bin/env bash
# render.sh: write the devbox-proxy Caddyfile and docker-compose.yml into a
# directory from the templates next to this script. Idempotent; it never
# touches a running container and never removes route files.
#
#   BIND_ADDR=<host addr> ORIGIN_CIDR=<edge addr>/32 [LISTEN_PORT=28081] \
#   [CONTAINER=devbox-proxy] [IMAGE=caddy:2.10-alpine] render.sh <dir> [--check]
#
# --check: render to a temporary directory and show the diff against <dir>
# (exit 1 when they differ), without writing anything.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { echo "render.sh: $*" >&2; exit 2; }

[ $# -ge 1 ] || die "usage: render.sh <dir> [--check]"
dir="$1"; check="${2:-}"
[ -z "$check" ] || [ "$check" = "--check" ] || die "unknown option $check"

: "${BIND_ADDR:?BIND_ADDR is required (address the proxy listens on)}"
: "${ORIGIN_CIDR:?ORIGIN_CIDR is required (the only origin accepted, e.g. the edge address/32)}"
LISTEN_PORT="${LISTEN_PORT:-28081}"
CONTAINER="${CONTAINER:-devbox-proxy}"
IMAGE="${IMAGE:-caddy:2.10-alpine}"

[[ $BIND_ADDR =~ ^[0-9a-fA-F.:]+$ ]] || die "BIND_ADDR must be an IP address"
[[ $BIND_ADDR != 0.0.0.0 && $BIND_ADDR != :: ]] || die "BIND_ADDR must not be a wildcard"
[[ $ORIGIN_CIDR =~ ^[0-9a-fA-F.:]+/[0-9]{1,3}$ ]] || die "ORIGIN_CIDR must be an address/prefix"
[[ $LISTEN_PORT =~ ^[0-9]{1,5}$ ]] && (( LISTEN_PORT >= 1 && LISTEN_PORT <= 65535 )) || die "LISTEN_PORT invalid"
[[ $CONTAINER =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || die "CONTAINER invalid"
[[ $IMAGE =~ ^[a-zA-Z0-9][a-zA-Z0-9_./:@-]*$ ]] || die "IMAGE invalid"

render() {  # render <template> <out>
	sed -e "s|@@BIND_ADDR@@|$BIND_ADDR|g" -e "s|@@ORIGIN_CIDR@@|$ORIGIN_CIDR|g" \
	    -e "s|@@LISTEN_PORT@@|$LISTEN_PORT|g" -e "s|@@CONTAINER@@|$CONTAINER|g" \
	    -e "s|@@IMAGE@@|$IMAGE|g" "$1" > "$2"
	if grep -q '@@[A-Z_]*@@' "$2"; then die "unrendered placeholder in $2"; fi
}

if [ "$check" = "--check" ]; then
	tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
	render "$here/Caddyfile.tmpl" "$tmp/Caddyfile"
	render "$here/docker-compose.yml.tmpl" "$tmp/docker-compose.yml"
	rc=0
	for f in Caddyfile docker-compose.yml; do
		diff -u "$dir/$f" "$tmp/$f" || rc=1
	done
	[ -d "$dir/routes" ] || { echo "missing $dir/routes"; rc=1; }
	exit $rc
fi

mkdir -p "$dir/routes"
chmod 755 "$dir/routes"
for f in Caddyfile docker-compose.yml; do
	tmpf="$(mktemp "$dir/.$f.XXXXXX")"
	render "$here/$f.tmpl" "$tmpf"
	chmod 644 "$tmpf"
	mv -f "$tmpf" "$dir/$f"
done
echo "rendered $dir/Caddyfile, $dir/docker-compose.yml and $dir/routes/"
