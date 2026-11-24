#!/usr/bin/env bash
# install-reload-hook.sh: install the certbot deploy hook and the host units
# that reload nginx after a renewal. Run on the edge host with sudo rights.
#
#   COMPOSE_DIR=/srv/nginx LE_DIR=/srv/nginx/certbot/conf RUN_USER=<user in docker group> \
#   install-reload-hook.sh <lineage> [<lineage>...]
#
# Idempotent: rewrites the hook, the lineage list and the units, then enables
# the path unit.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { echo "install-reload-hook.sh: $*" >&2; exit 2; }
[ $# -ge 1 ] || die "usage: install-reload-hook.sh <lineage>..."
: "${COMPOSE_DIR:?}" "${LE_DIR:?}" "${RUN_USER:?}"
for v in "$COMPOSE_DIR" "$LE_DIR"; do [[ $v =~ ^/[A-Za-z0-9_./-]+$ ]] || die "bad path $v"; done
[[ $RUN_USER =~ ^[a-z_][a-z0-9_-]*$ ]] || die "bad user"
for l in "$@"; do [[ $l =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || die "bad lineage $l"; [ -d "$LE_DIR/live/$l" ] || sudo test -d "$LE_DIR/live/$l" || die "no lineage $l"; done

sudo install -d -m 755 "$LE_DIR/renewal-hooks/deploy"
sudo install -m 755 "$here/certbot-deploy-hook.sh" "$LE_DIR/renewal-hooks/deploy/nginx-reload.sh"
printf '%s\n' "$@" | sudo tee "$LE_DIR/renewal-hooks/nginx-reload.lineages" >/dev/null
sudo chmod 644 "$LE_DIR/renewal-hooks/nginx-reload.lineages"
for u in nginx-reload.path nginx-reload.service; do
	sed -e "s|@@LE_DIR@@|$LE_DIR|g" -e "s|@@COMPOSE_DIR@@|$COMPOSE_DIR|g" -e "s|@@RUN_USER@@|$RUN_USER|g" "$here/$u.tmpl" |
		sudo tee "/etc/systemd/system/$u" >/dev/null
done
sudo systemctl daemon-reload
sudo systemctl enable --now nginx-reload.path
echo "installed: hook for $*; nginx-reload.path $(systemctl is-active nginx-reload.path)"
