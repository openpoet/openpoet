# HTML artifacts

An artifact is an interactive HTML document that an agent publishes into
OpenPoet: one self-contained page (inline CSS and JavaScript) stored in the
OpenPoet database, never in the project directory. The user opens it in a
sandboxed viewer, annotates the parts that should change, and the agent reads
those notes, publishes a new version and resolves them.

Use it for plans, reports and documentation. Docs (`openpoet_create_document`)
remain the place for short plain-text answers.

## What is stored

| Table | Holds |
| --- | --- |
| `artifacts` | Title, summary, status (`active` / `archived`), current version, and the links to the project, session, task and chat conversation that produced it |
| `artifact_versions` | The HTML of every version, with its size, SHA-256, change note and author. Versions are never rewritten or deleted |
| `artifact_annotations` | The user's notes: the version they were made on, where they point (a text quote or an element), the note, and its resolution |

Rules the server enforces on every publish:

- The content must be an HTML document, valid UTF-8, at most **2 MiB** per
  version.
- Credential-looking text (`password=…`, `api_key: …`, private keys) is masked
  as `[REDACTED]`, as in Docs. The publish result says when that happened,
  because masking inside a script can break it.
- An update names the version it was based on (`base_version`). If the artifact
  has moved on, the update is refused with a conflict that names the current
  version; nothing is overwritten.
- Publishing HTML identical to the current version stores nothing.
- A missing project or task link is derived from the publishing session.

## Sandbox

The HTML is written by an agent, so it is treated as hostile. It is never
inserted into the OpenPoet page.

1. **Sandboxed iframe.** The viewer loads the artifact in
   `<iframe sandbox="allow-scripts">`. Scripts run, in an opaque origin: no
   cookies, no storage, no access to the OpenPoet page, no popups, no form
   submission, no top-level navigation, no downloads.
2. **Content Security Policy on the response.** `GET /api/artifacts/{id}/render`
   is the only route that returns an artifact as `text/html`, and it always
   sends:

   ```
   sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline';
   style-src 'unsafe-inline'; img-src data: blob:; font-src data:;
   media-src data: blob:; connect-src 'none'; form-action 'none';
   base-uri 'none'; frame-src 'none'; child-src 'none'; worker-src 'none';
   object-src 'none'; webrtc 'block'; frame-ancestors 'self'
   ```

   `connect-src 'none'` blocks `fetch`, XHR and WebSockets, including to the
   OpenPoet API; no script, style, font or image can be loaded from any host.
   The `sandbox` directive keeps the artifact sandboxed when the URL is opened
   directly in a tab. Errors on this route carry the same headers.
3. **No credential on the route.** The route is under `/api/`, where the
   UI-cookie middleware never plants the UI secret, and the response sets no
   cookie. It also sends `X-Content-Type-Options: nosniff`,
   `Referrer-Policy: no-referrer`, `Cache-Control: no-store`,
   `Cross-Origin-Resource-Policy: same-origin` and a `Permissions-Policy` that
   denies camera, microphone, geolocation and clipboard.

What remains possible, because CSP does not govern it:

- An artifact can navigate its own frame to another URL, which can disclose the
  artifact's own content to that URL.
- WebRTC peer connections are outside the CSP fetch directives. The response
  sends `webrtc 'block'`, which only browsers that implement that directive
  enforce; this has not been verified to stop a connection.

In both cases the only data available to the artifact is its own content, which
the agent wrote. Nothing from OpenPoet is ever placed in the frame: annotation
text, in particular, stays in the page.

An agent session cannot write annotations (they are the reviewer's notes, and
agents are told to read them as the user's feedback); it can list, resolve and
reopen them. Titles and change notes are stored as one line of printable text.
The viewer's download hands the browser the HTML as `application/octet-stream`,
so a browser that opens the blob instead of saving it does not run the artifact
in the OpenPoet origin.

`tests/artifact-viewer-smoke.js` publishes an artifact that tries each way out
(cookies, storage, the API, the internet, the parent page, popups, remote
images and scripts) and checks that all of them fail, in the viewer and when
the render URL is opened directly.

## Annotations

The page cannot reach into the sandboxed frame, so the server appends a small
bridge script when it serves an artifact. The bridge is not stored with the
artifact and gets no extra capability; it talks to the page with `postMessage`.

- In annotate mode, selecting text or clicking an element reports an **anchor**
  to the page: a text quote with the characters around it and its offsets, or
  an element selector with its tag and label. While annotate mode is on, clicks
  do not reach the artifact's own handlers.
- The note is typed in the OpenPoet page and stored through the API. **Its text
  is never sent into the frame.** The frame only receives anchors, ids and
  statuses, to draw numbered highlights in a layer of its own (it does not
  modify the artifact's DOM).
- The bridge shares the frame with the artifact's scripts, so the page treats
  every message as untrusted: it checks the sender and the shape, renders
  quotes as text, and opens the note composer only while the user has annotate
  mode on.
- An annotation belongs to the version it was made on. On another version it is
  placed again by its quote and context; when it no longer matches, the panel
  marks it "not in this version" and it stays open.
- **Send to agent** posts a short message to the session that published the
  artifact (through the existing session input route), asking it to read the
  open annotations.

## API

| Method and route | Purpose |
| --- | --- |
| `POST /api/artifacts` | Publish a new artifact (`title`, `html`, `summary`, optional `project_id`, `task_id`, `conversation_id`, `session_id`) |
| `GET /api/artifacts?project_id=&session_id=&task_id=&include_archived=` | List artifacts, without HTML |
| `GET /api/artifacts/{id}` | Metadata, version history and open-annotation count |
| `PUT /api/artifacts/{id}` | Publish a new version (`html`, `base_version`, `change_note`) |
| `PATCH /api/artifacts/{id}` | Change `title`, `summary` or `status` |
| `GET /api/artifacts/{id}/versions/{n\|current}` | One version with its HTML, as JSON |
| `GET /api/artifacts/{id}/render?v=n` | The sandboxed document for the viewer |
| `GET /api/artifacts/{id}/annotations?status=open\|resolved\|all` | List annotations |
| `POST /api/artifacts/{id}/annotations` | Create an annotation (`version`, `anchor`, `body`) |
| `POST /api/artifacts/{id}/annotations/{annotationId}/resolve` | Resolve, with an optional `resolution_note` |
| `POST /api/artifacts/{id}/annotations/{annotationId}/reopen` | Reopen |

A caller authenticated as a session is always attributed to its own session.
Artifacts also appear in the session and task document listings with ids of
the form `artifact:<id>` and type `artifact`.

## Agent tools

Registered next to the Docs tools, so they are available to sessions over MCP,
to the chat assistant, and through `openpoet cli call`.

| Tool | Arguments |
| --- | --- |
| `openpoet_create_artifact` | `title`, `html`, `summary`, `task_id` |
| `openpoet_update_artifact` | `artifact_id`, `html`, `change_note`, `base_version` |
| `openpoet_read_artifact` | `artifact_id`, `version`, `include_html` |
| `openpoet_list_artifacts` | `project_id`, `task_id`, `session_id`, `include_archived` |
| `openpoet_list_artifact_annotations` | `artifact_id`, `status` (default `open`) |
| `openpoet_resolve_artifact_annotation` | `artifact_id`, `annotation_id`, `resolution_note` |

Through the CLI, `html_file` publishes a document from a local file instead of
passing it inline:

```bash
openpoet cli call openpoet_create_artifact '{"title":"Plan","html_file":"/tmp/plan.html"}'
```

Only the CLI honors `html_file`; the MCP server never reads a path named by a
client.

Tool policies written as an allowlist name every tool they grant. On upgrade,
allowlists that already grant `create_document` get the publishing tools, and
those that grant `read_document` get the reading tools. Policies that allow
everything are not touched. Remove the tools in the tool policy settings to opt
a project out.

Automation API capabilities (`artifacts.list`, `artifacts.get`,
`artifacts.get_version`, `artifacts.create`, `artifacts.update`,
`artifacts.update_meta`, `artifacts.annotations.list`, `.create`, `.resolve`,
`.reopen`) use the `documents:read` and `documents:write` scopes. One automation
command is limited to the Automation API's 1 MiB request body, so larger
documents are published through the REST route or the MCP tools.

## How agents are steered to use it

- The managed instruction block that OpenPoet syncs into every harness
  (`.claude/CLAUDE.md`, `AGENTS.md`, Copilot and ACP instructions) tells agents
  to publish plans, reports and documentation as HTML artifacts, and to use a
  Doc for short plain text or when the artifact tools are unavailable.
- The built-in `html-artifact` skill is the design guide: what the sandbox
  enforces, how to structure a page with navigation, visual components, inline
  charts and restrained motion, how to keep it easy to annotate, and the review
  loop. It is registered once; edit or delete it like any other skill.
