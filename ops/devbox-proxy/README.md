# devbox-proxy: publishing devbox ports

An agent running in a devbox container (an SSH project on the OpenPoet host)
publishes a port of its own container with the MCP tool
`openpoet_devbox_publish_port {app, port, mode?}` and gets back
`https://<app>-<suffix>.<domain>/` (the form is configurable), reachable only
from the private network.
`openpoet_devbox_list_ports` and `openpoet_devbox_unpublish_port {app}`
list and remove publications. The code is in `internal/devboxpublish`.

```
agent (devbox) --MCP over the session's SSH tunnel, opst1 bearer--> OpenPoet (host)
OpenPoet: devbox-forward add op-<app> <container> <port> <pool port>   (loopback relay)
          routes/op-<app>.caddy + caddy reload through the admin socket
edge nginx (TLS, wildcard) --> devbox-proxy <BIND_ADDR>:28081 --Host--> 127.0.0.1:<pool port> --> container:<port>
```

## Trust model

- Only a session with a valid per-session bearer may call the tools. The
  container is **derived** from that session's project (SSH host must be one of
  `OPENPOET_DEVBOX_SSH_HOSTS`, SSH port must be the published port 22 of
  exactly one running container). The agent never names a container.
- The container must be listed in `OPENPOET_DEVBOX_CONTAINERS` with its app
  prefix: an app is `<prefix>` or `<prefix>-<rest>`, so devboxes cannot take
  each other's names. Names in `OPENPOET_DEVBOX_RESERVED` (hand-managed
  routes) are never created or removed by the tools.
- Relays listen on 127.0.0.1 only and accept only 127.0.0.1; the proxy accepts
  only the edge origin and drops unknown Hosts. Host ports come from a pool;
  the agent never picks one.
- The proxy's admin API is a unix socket in a tmpfs **inside** the proxy
  container: no TCP port. OpenPoet reaches it with `docker exec`.
- A publication lasts until it is removed or its container no longer exists
  (checked every 10 minutes; a Docker error removes nothing).

## Files

| File | Role |
|---|---|
| `Caddyfile.tmpl` | Proxy config: origin check, `import /etc/caddy/routes/*.caddy`, final abort, admin on the socket |
| `docker-compose.yml.tmpl` | Caddy on the host network, `routes/` read-only, tmpfs for the admin socket |
| `render.sh` | Renders both into the proxy's directory (`--check` shows the diff) |
| `edge-nginx.conf.tmpl`, `render-edge.sh` | Edge nginx server blocks for the subzone |
| `terraform/` | The wildcard DNS record (one resource) |
| `render_test.sh` | Tests, including `caddy adapt` of the rendered file when the image is present |

Route files in `routes/`: OpenPoet writes `op-<app>.caddy`; routes managed by
hand are `manual-<name>.caddy`, in the same two-line form:

```
@m_web host web-dev9.example.com
reverse_proxy @m_web 127.0.0.1:28080
```

## OpenPoet configuration (`.run/openpoet.env`)

| Variable | Example | Meaning |
|---|---|---|
| `OPENPOET_DEVBOX_PUBLISH` | `1` | Turns the feature on |
| `OPENPOET_DEVBOX_HOST_TEMPLATE` | `{app}-dev9.example.com` | Published host; `{app}.dev9.example.com` works too |
| `OPENPOET_DEVBOX_SSH_HOSTS` | `192.0.2.9` | SSH hosts that mean "a container on this machine" |
| `OPENPOET_DEVBOX_CONTAINERS` | `alpha-devbox:alpha,beta-devbox:beta` | Allowed containers and their app prefix |
| `OPENPOET_DEVBOX_RESERVED` | `alpha,alpha-legacy` | Hand-managed app names |
| `OPENPOET_DEVBOX_PORT_RANGE` | `28100-28199` | Loopback pool for the relays |
| `OPENPOET_DEVBOX_ROUTES_DIR` | `/srv/devbox-proxy/routes` | The proxy's `routes/` on the host |
| `OPENPOET_DEVBOX_PROXY_CONTAINER` | `devbox-proxy` | Default `devbox-proxy` |
| `OPENPOET_DEVBOX_FORWARD_BIN` | `/home/dev/.local/bin/devbox-forward` | Default `devbox-forward` on `PATH` |
| `OPENPOET_DEVBOX_DNS` | `static` | `static`: a wildcard record covers every host (nothing to do). `command`: run `OPENPOET_DEVBOX_DNS_COMMAND ensure <host>` and `remove <host>` |
| `OPENPOET_DEVBOX_DNS_COMMAND` | `/usr/local/bin/devbox-dns` | Absolute path; it owns the credentials and the scope of what it may change |
| `OPENPOET_DEVBOX_RESOLVER` | `192.0.2.9:53` | Resolver of the private network. When set, a publish returns the URL only once this resolver answers for the host |
| `OPENPOET_DEVBOX_RESOLVER_CONTAINER` | `dns-cache` | Container sent SIGHUP before each check (dnsmasq drops its cache, including negative answers cached before the record existed) |
| `OPENPOET_DEVBOX_DNS_WAIT_SECONDS` | `60` | How long a publish waits for the name (1-600); on timeout the publication is undone |

A bad value disables the feature and is logged at startup
(`[devbox-publish] disabled: ...`); it never stops OpenPoet. The tools must
also be allowed by the session tool policy (Config → MCP) when it is an
allowlist.

## Activation (once)

1. Render next to the current files and compare:
   `BIND_ADDR=<host> ORIGIN_CIDR=<edge>/32 ./render.sh <proxy dir> --check`.
2. Put each existing hand-made route in `routes/manual-<name>.caddy`, then
   render for real: `... ./render.sh <proxy dir>`.
3. `docker compose up -d` in the proxy directory (one restart: the admin
   socket only exists after it). Check the existing URLs answer.
4. Set the variables above and restart OpenPoet.

Rollback: restore the previous `Caddyfile`/`docker-compose.yml` and
`docker compose up -d`. Relays and route files are left as they are.

## DNS

`internal/devboxpublish/dns.go` isolates the naming scheme behind two
interfaces: `DNS` (`Ensure`/`Remove` a host's record) and `Resolver`
(`Flush` the private network's resolver cache, ask whether a host
`Resolves`). Publishing is relay → route → `DNS.Ensure` → flush and poll the
resolver until it answers → URL; any failure undoes the earlier steps.
Removing is route → relay → `DNS.Remove` → flush → record. A new naming scheme
is a new `DNS` implementation (or a `command`), with no other change.

## Edge

Everything under one subzone goes to the proxy, so publishing changes nothing
on the edge: the URL of an app is `https://<app>.<subzone>/` (one DNS label,
which is what a wildcard certificate covers), e.g. with
`OPENPOET_DEVBOX_HOST_TEMPLATE={app}.dev9.example.com`.

| Piece | How |
|---|---|
| DNS | One wildcard `A` record `*.<subzone>` → edge, **only** through `terraform/` (one resource; values and state outside the repository) |
| TLS | One wildcard certificate `*.<subzone>`, issued and renewed by the edge's existing ACME client (DNS-01) under its own certificate name |
| nginx | `render-edge.sh` → one file with the two server blocks: `<label>.<subzone>` → `PROXY_ADDR`, `Host` preserved, private network only |

With `OPENPOET_DEVBOX_DNS=static` and `OPENPOET_DEVBOX_RESOLVER` set, a
publish creates no record: it only checks that the private network's resolver
answers for the new name before returning the URL.

### DNS: wildcard record

```
cd ops/devbox-proxy/terraform
export CLOUDFLARE_API_TOKEN=...        # DNS edit on the zone
terraform init
terraform plan  -var-file=<private>/terraform.tfvars -state=<private>/terraform.tfstate -out=<private>/plan.bin
terraform apply -state=<private>/terraform.tfstate <private>/plan.bin
```

`terraform.tfvars.example` shows the three values. The configuration has a
single resource and reads the zone by name: it never imports or changes other
records of the zone. Removing the wildcard is `terraform destroy` with the
same arguments.
