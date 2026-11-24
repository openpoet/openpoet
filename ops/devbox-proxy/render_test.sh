#!/usr/bin/env bash
# Tests for render.sh. With docker and the Caddy image available locally, the
# rendered Caddyfile is also adapted by Caddy itself (no network, no ports).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
export BIND_ADDR=192.0.2.4 ORIGIN_CIDR=192.0.2.1/32

out="$tmp/proxy"
bash "$here/render.sh" "$out" >/dev/null
[ -d "$out/routes" ] || fail "routes dir not created"
grep -q '^	bind 192.0.2.4$' "$out/Caddyfile" || fail "bind not rendered"
grep -q 'not remote_ip 192.0.2.1/32' "$out/Caddyfile" || fail "origin not rendered"
grep -q '^http://:28081 {' "$out/Caddyfile" || fail "default port not rendered"
grep -q 'admin unix//run/caddy-admin/admin.sock' "$out/Caddyfile" || fail "admin socket missing"
grep -q 'admin off' "$out/Caddyfile" && fail "admin must be on the socket, not off"
grep -q -- '- ./routes:/etc/caddy/routes:ro' "$out/docker-compose.yml" || fail "routes mount missing"
grep -q -- '- /run/caddy-admin:mode=0700' "$out/docker-compose.yml" || fail "admin tmpfs missing"
! grep -q '@@' "$out/Caddyfile" "$out/docker-compose.yml" || fail "placeholder left"
# Order inside route: origin abort, import, final abort.
awk '/route \{/{f=1} f&&/abort @not_from_edge/{a=NR} f&&/import \/etc\/caddy\/routes/{i=NR} f&&/^\t\tabort$/{z=NR} END{exit !(a<i && i<z)}' "$out/Caddyfile" || fail "route order"

# Idempotent, and --check agrees.
echo "@m host x" > "$out/routes/manual-x.caddy"
bash "$here/render.sh" "$out" >/dev/null
[ -f "$out/routes/manual-x.caddy" ] || fail "render removed a route file"
bash "$here/render.sh" "$out" --check >/dev/null || fail "--check after render should pass"
LISTEN_PORT=28090 bash "$here/render.sh" "$out" --check >/dev/null && fail "--check should see a different port"
ls -A "$out" | grep -q '^\.' && fail "temporary files left behind"

# Rejected input.
BIND_ADDR=0.0.0.0 bash "$here/render.sh" "$tmp/x" 2>/dev/null && fail "wildcard bind accepted"
BIND_ADDR='1.2.3.4|evil' bash "$here/render.sh" "$tmp/x" 2>/dev/null && fail "bad bind accepted"
ORIGIN_CIDR=192.0.2.1 bash "$here/render.sh" "$tmp/x" 2>/dev/null && fail "origin without prefix accepted"
(unset BIND_ADDR; bash "$here/render.sh" "$tmp/x" 2>/dev/null) && fail "missing BIND_ADDR accepted"

# Edge nginx server blocks.
edge="$tmp/edge.conf"
SUBZONE=dev9.example.com PROXY_ADDR=192.0.2.4:28081 ALLOW_CIDR=192.0.2.0/24 bash "$here/render-edge.sh" "$edge" >/dev/null
[ "$(grep -c 'server_name "~^\[a-z0-9\]+(-\[a-z0-9\]+)\*\\.dev9\\.example\\.com\$";' "$edge")" = 2 ] || fail "edge server_name (one label below the subzone)"
grep -q 'proxy_pass http://192.0.2.4:28081;' "$edge" || fail "edge proxy_pass"
grep -q 'proxy_set_header Host \$host;' "$edge" || fail "edge must preserve Host"
grep -q 'allow 192.0.2.0/24;' "$edge" && grep -q 'deny all;' "$edge" || fail "edge allow/deny"
grep -q '/etc/letsencrypt/live/dev9.example.com/fullchain.pem' "$edge" || fail "edge certificate"
! grep -q '@@' "$edge" || fail "edge placeholder left"
SUBZONE='dev9.example.com;' PROXY_ADDR=192.0.2.4:28081 ALLOW_CIDR=192.0.2.0/24 bash "$here/render-edge.sh" "$tmp/bad.conf" 2>/dev/null && fail "bad subzone accepted"
SUBZONE=dev9.example.com PROXY_ADDR=192.0.2.4 ALLOW_CIDR=192.0.2.0/24 bash "$here/render-edge.sh" "$tmp/bad.conf" 2>/dev/null && fail "proxy without port accepted"

# Certbot deploy hook: only listed lineages request an nginx reload.
le="$tmp/le"; mkdir -p "$le/renewal-hooks"
printf 'a.example.com\nb.example.com\n' > "$le/renewal-hooks/nginx-reload.lineages"
CERTBOT_CONFIG_DIR="$le" RENEWED_LINEAGE="$le/live/c.example.com" sh "$here/edge/certbot-deploy-hook.sh" >/dev/null
[ ! -e "$le/nginx-reload.request" ] || fail "unlisted lineage requested a reload"
CERTBOT_CONFIG_DIR="$le" RENEWED_LINEAGE="$le/live/b.example.com" sh "$here/edge/certbot-deploy-hook.sh" >/dev/null
grep -q ' b.example.com$' "$le/nginx-reload.request" || fail "listed lineage did not request a reload"
CERTBOT_CONFIG_DIR="$le" sh "$here/edge/certbot-deploy-hook.sh" >/dev/null 2>&1 || fail "hook must not fail without RENEWED_LINEAGE"
ls -A "$le" | grep -q '^\.nginx-reload' && fail "hook left a temporary file"

image=caddy:2.10-alpine
if command -v docker >/dev/null && docker image inspect "$image" >/dev/null 2>&1; then
	rm -f "$out/routes/manual-x.caddy"
	printf '@op_a_web host a-web-dev9.example.com\nreverse_proxy @op_a_web 127.0.0.1:28100\n' > "$out/routes/op-a-web.caddy"
	json="$(docker run --rm --network none -v "$out/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$out/routes:/etc/caddy/routes:ro" \
		"$image" caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile 2>/dev/null)" || fail "caddy adapt failed"
	echo "$json" | grep -q '"host":\["a-web-dev9.example.com"\]' || fail "route not imported"
	echo "$json" | grep -q '"listen":"unix//run/caddy-admin/admin.sock"' || fail "admin socket not adapted"
	echo "$json" | grep -q '"listen":\["192.0.2.4:28081"\]' || fail "listen address"
	rm -f "$out/routes/op-a-web.caddy"
	docker run --rm --network none -v "$out/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$out/routes:/etc/caddy/routes:ro" \
		"$image" caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || fail "empty routes dir must adapt"
	echo "caddy adapt: ok"
else
	echo "caddy adapt: skipped (docker or $image not available)"
fi
echo "render_test: ok"
