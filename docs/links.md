# Links

OpenPoet runs on a machine and a port; the address people reach it at from
another device is something only the owner knows. An agent that builds a URL on
its own ends up with `localhost` and a port. So links are built in one place,
on one configured address.

## Public base URL

Set it in **Config → Settings → Public Address**, or with the
`OPENPOET_PUBLIC_BASE_URL` environment variable (which wins over the setting):

```
https://openpoet.example.com
```

- It must be an `http` or `https` address, without credentials, query string or
  fragment. A path prefix is allowed (`https://example.com/openpoet`). It is
  stored without the trailing slash.
- Its host name is accepted by the Host guard, so it does not have to be
  repeated in `OPENPOET_ALLOWED_HOSTS`.
- It only says where the instance is reached. It does not publish anything:
  whoever opens a link still needs to reach that address (the same network, a
  VPN, or a paired device through the tunnel).

Without it, OpenPoet has no link that works outside the machine it runs on, and
says so instead of handing out `localhost`.

## Link paths

| Path | Opens |
| --- | --- |
| `/app/artifact/<id>` | The artifact in the artifact viewer (`?v=<n>` for a version) |
| `/app/doc/<id>` | The Doc in the document viewer |
| `/app/session/<id>` | The session's terminal |
| `/app/task/<project>/<task>` | The task |
| `/app/project/<id>` | The project |

The app keeps its state in the query string, so a link path is rewritten on
load into the canonical form (`/?view=projects&doc=artifact%3A<id>`) and then
opened. An `/app/...` path that is not a link goes to the home page. A link to
an artifact that does not exist opens the viewer with a message saying so.

Through the tunnel, a device that is not paired is sent to the pairing page
with the link it was opening (`/pair?next=<path>`), and lands on it once the
pairing is approved. `next` is only ever a path on the same site.

## Getting a link

| Where | How |
| --- | --- |
| REST | `GET /api/links?kind=artifact\|document\|session\|task\|project&id=<id>[&version=<n>]` |
| Agent tool | `openpoet_get_link` (`kind`, `id`, `version`) over MCP and the CLI; `get_link` in the chat assistant |

The answer carries `path` (always), `url` (the path on the public base URL;
absent when none is configured), `local_url` (the path on the address the
request came in on) and `public`. Asking for something that does not exist is
an error: no link is invented.

`openpoet_create_artifact`, `openpoet_update_artifact` and
`openpoet_create_document` also answer with the shareable link when a public
base URL is configured. The REST publish routes return it as `url`, next to the
in-app `link`.

A session can ask for its own link with `kind: "session"` and no `id`.

Tool policies written as an allowlist name every tool they grant. On upgrade,
allowlists that already grant `read_document` or `read_artifact` get
`get_link`. Policies that allow everything are not touched.
