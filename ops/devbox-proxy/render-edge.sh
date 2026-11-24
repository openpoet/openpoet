#!/usr/bin/env bash
# render-edge.sh: write the edge nginx server blocks for the devbox subzone.
#
#   SUBZONE=dev9.example.com PROXY_ADDR=<proxy addr>:28081 ALLOW_CIDR=<vpn cidr> \
#   [CERT_NAME=<SUBZONE>] render-edge.sh <out-file>
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { echo "render-edge.sh: $*" >&2; exit 2; }
[ $# -eq 1 ] || die "usage: render-edge.sh <out-file>"
: "${SUBZONE:?SUBZONE is required (e.g. dev9.example.com)}"
: "${PROXY_ADDR:?PROXY_ADDR is required (host:port of devbox-proxy)}"
: "${ALLOW_CIDR:?ALLOW_CIDR is required (the private network)}"
CERT_NAME="${CERT_NAME:-$SUBZONE}"
[[ $SUBZONE =~ ^[a-z0-9]+(-[a-z0-9]+)*(\.[a-z0-9]+(-[a-z0-9]+)*)+$ ]] || die "SUBZONE must be a lowercase domain name"
[[ $PROXY_ADDR =~ ^[0-9.]+:[0-9]{1,5}$ ]] || die "PROXY_ADDR must be IPv4:port"
[[ $ALLOW_CIDR =~ ^[0-9.]+/[0-9]{1,2}$ ]] || die "ALLOW_CIDR must be an IPv4 CIDR"
[[ $CERT_NAME =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || die "CERT_NAME invalid"
subzone_re="${SUBZONE//./\\\\.}"
tmp="$(mktemp "$1.XXXXXX")"
sed -e "s|@@SUBZONE_RE@@|$subzone_re|g" -e "s|@@SUBZONE@@|$SUBZONE|g" -e "s|@@PROXY_ADDR@@|$PROXY_ADDR|g" \
    -e "s|@@ALLOW_CIDR@@|$ALLOW_CIDR|g" -e "s|@@CERT_NAME@@|$CERT_NAME|g" "$here/edge-nginx.conf.tmpl" > "$tmp"
if grep -q '@@[A-Z_]*@@' "$tmp"; then rm -f "$tmp"; die "unrendered placeholder"; fi
chmod 644 "$tmp"; mv -f "$tmp" "$1"
echo "rendered $1"
