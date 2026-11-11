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
| `artifact_annotations` | The user's notes: the version they were made on, where they point (a text quote or an element), the note, and its resolution. Also the user's answers to the questions an artifact asks (`kind = 'answer'`) |
| `artifact_questions` | The questions each version declares for the reader |

Rules the server enforces on every publish:

- The content must be an HTML document, valid UTF-8, at most **2 MiB** per
  version.
- The HTML is stored exactly as it was sent, and the publish result carries its
  size in bytes and its SHA-256.
- Credential-looking text (`password=…`, `api_key: …`, private keys) is masked
  as `[REDACTED]`, as in Docs. That is the one case in which the stored HTML
  differs from the input; the publish result says when it happened, because
  masking inside a script can break it.
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

4. **WebRTC is neutered in the document.** Peer connections are outside the CSP
   fetch directives, and `webrtc 'block'` is ignored by Chromium (it was
   confirmed to gather server-reflexive candidates against an external STUN
   host), so a guard script injected as the **first** script of every rendered
   document removes `RTCPeerConnection`, `webkitRTCPeerConnection`,
   `mozRTCPeerConnection` and `RTCDataChannel` before any agent script runs. The
   sandbox already blocks nested frames and workers (`frame-src`, `child-src`,
   `worker-src` are `'none'`), so there is no other context to re-obtain them.
   `webrtc 'block'` stays in the CSP as defence in depth. The guard is injected
   right after a leading `<!doctype>`, so a standards-mode document keeps
   standards mode.

What remains possible, because CSP does not govern it:

- An artifact can navigate its own frame to another URL, which can disclose the
  artifact's own content to that URL.

In this case the only data available to the artifact is its own content, which
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
  statuses, to draw numbered highlights in a layer of its own, outside the
  artifact's DOM.
- The bridge shares the frame with the artifact's scripts, so the page treats
  every message as untrusted: it checks the sender and the shape, renders
  quotes as text, and opens the note composer only while the user has annotate
  mode on.
- An annotation belongs to the version it was made on. On another version it is
  placed again by its quote and context; when it no longer matches, the panel
  marks it "not in this version" and it stays open.
- **Send to agent** posts a short message to the session that published the
  artifact (through the existing session input route), asking it to read the
  open annotations. The message starts with `[OpenPoet]`, says how many answers
  and notes are open, and carries none of their text.

## Questions and answers

An artifact can ask the reader to decide or answer something, and get the
answer back as structured data instead of a free note.

**Declaring.** A version declares its questions in one JSON data block:

```html
<script type="application/json" id="openpoet-questions">
{"questions":[
  {"id":"D1","prompt":"What do we call it?","type":"single",
   "options":[{"id":"a","label":"Routine"},{"id":"b","label":"Automation"}]},
  {"id":"D2","prompt":"Which triggers ship first?","type":"multi","comment":false,
   "options":[{"id":"form","label":"Form"},{"id":"chat","label":"Chat"}]},
  {"id":"NOTES","prompt":"Anything else?","type":"text"}
]}
</script>
```

- `type` is `single` (exactly one option), `multi` (one or more) or `text`
  (free text). Without it, a question with options is `single` and one without
  is `text`. `comment: false` removes the comment box of a choice.
- Ids are 1 to 64 letters, digits, `_`, `-`, `.` or `:`. At most 200 questions
  and 30 options per question; a prompt of up to 500 characters and a label of
  up to 300.
- The server reads the block when the version is published and stores the
  questions with that version. A block that is present but malformed refuses
  the publish (`invalid_artifact_questions`) with a message that says what is
  wrong. The publish result reports how many questions were declared.

**Marking the page.** The element of a question carries
`data-openpoet-question="<id>"`, each choice inside it
`data-openpoet-option="<id>"`, and a control that opens a free-text answer
`data-openpoet-answer`. The elements may be created by script. The bridge sets
`data-openpoet-selected` on the chosen options and
`data-openpoet-answered="pending"` or `"saved"` on the question, with the
matching ARIA state, and `data-openpoet-host` on `<html>`: these are the only
attributes it sets on the artifact's elements. A script can also call
`window.openpoet.answer(question, option)` and `window.openpoet.ask(question)`,
and listen to `openpoet:answers` on `document`.

**Answering.** A tap on a choice marks it. Marked answers wait in the
**Answers** tab of the panel, where the questions of the version are native
controls of the OpenPoet page; a bar over the artifact says how many are
waiting. They are saved together when the user presses **Confirm** there.
Waiting answers survive a reload (they are kept in the page's session storage).
In annotate mode a click annotates instead.

**What is stored.** An answer is an annotation of kind `answer`: the question,
the options chosen (with the labels they had) and a comment, or the text of a
free-text answer. A question of an artifact has one open answer: answering
again replaces it, and it can be withdrawn. An answer the agent resolved stays
as the record; answering that question again opens a new one. Answers are
listed with the notes, count as open annotations, and one whose question a
later version no longer asks is flagged "not in this version".

**Trust.** The bridge shares the frame with the artifact's scripts, so a
message from the frame proves nothing about what the user did.

- A proposal from the frame can only name a question and an option that the
  server stored for that version; anything else is ignored. The server checks
  again when the answer is saved.
- Nothing is stored on a proposal. An answer exists only after the user
  confirms it in the OpenPoet page, which shows the question and the chosen
  option from the server's list, not from the frame.
- Comments and free-text answers are typed in the OpenPoet page and are never
  sent into the frame. The frame learns which option ids are chosen, so the
  artifact can show them.
- A proposal never changes what the user typed.
- A session cannot answer or withdraw an answer, and the notes route refuses a
  question anchor: the answer route is the only way an answer is written.

## API

| Method and route | Purpose |
| --- | --- |
| `POST /api/artifacts` | Publish a new artifact (`title`, `html` or `html_path`, `summary`, optional `project_id`, `task_id`, `conversation_id`, `session_id`) |
| `GET /api/artifacts?project_id=&session_id=&task_id=&include_archived=` | List artifacts, without HTML |
| `GET /api/artifacts/{id}` | Metadata, version history (size and SHA-256 of each version), open-annotation count, `link` and `url` |
| `PUT /api/artifacts/{id}` | Publish a new version (`html` or `html_path`, `base_version`, `change_note`) |
| `POST /api/artifacts/{id}/patch` | Publish a new version from replacements (`base_version`, `edits: [{old, new, replace_all}]`, `change_note`) |
| `PATCH /api/artifacts/{id}` | Change `title`, `summary` or `status` |
| `GET /api/artifacts/{id}/versions/{n\|current}` | One version with its HTML, as JSON |
| `POST /api/artifacts/{id}/versions/{n\|current}/export` | Write the HTML of a version to a file in the calling session's workspace (`path`) |
| `GET /api/artifacts/{id}/render?v=n` | The sandboxed document for the viewer |
| `GET /api/artifacts/{id}/annotations?status=open\|resolved\|all` | List annotations |
| `POST /api/artifacts/{id}/annotations` | Create an annotation (`version`, `anchor`, `body`) |
| `POST /api/artifacts/{id}/annotations/{annotationId}/resolve` | Resolve, with an optional `resolution_note` |
| `POST /api/artifacts/{id}/annotations/{annotationId}/reopen` | Reopen |
| `GET /api/artifacts/{id}/questions?v=n` | The questions a version declares |
| `PUT /api/artifacts/{id}/answers` | Save answers (`version`, `answers: [{question, options, text, comment}]`): all of them or none |
| `DELETE /api/artifacts/{id}/answers/{questionId}` | Withdraw the open answer to a question |

A caller authenticated as a session is always attributed to its own session.
Artifacts also appear in the session and task document listings with ids of
the form `artifact:<id>` and type `artifact`.

A create, update or patch answers with `version`, `created`, `size_bytes` and
`sha256` (the exact size and hash of the stored HTML), `redacted`, `link` and
`url`. The HTML is stored as it was sent: `sha256` is the hash of the input
unless `redacted` is true.

### Publishing without resending the document

A large page passed inline costs its whole size on every revision. Three
things avoid that:

- **`html_path`**, on create and update: the path of an `.html` file that
  OpenPoet reads. It is resolved for the calling session, on the machine that
  session runs on (the local disk, or over SFTP for a remote project): a
  relative path is resolved against the session's working directory, and the
  file has to be inside that directory or the machine's temporary directory
  once symbolic links are followed. Only a regular `.html` or `.htm` file of at
  most 2 MiB is read. A request that is not attributed to a session is
  refused, and so is a remote Windows project. OpenPoet reaches that machine
  with the OS account the session runs as, so this reads nothing the session
  could not read itself.
- **Patch**: the server applies `edits` in order to the HTML of
  `base_version`, which must be the current version. Each `old` has to match
  exactly and once (or every occurrence with `replace_all`); if one does not,
  nothing is published and the error names the edit. An edit written with LF
  line breaks matches a document stored with CRLF.
- **Export**: the HTML of a version is written to a file under the same rules
  as `html_path` (the directory must exist; only a regular file is replaced),
  and the answer has the path, size and hash, never the HTML.

## Agent tools

Registered next to the Docs tools, so they are available to sessions over MCP,
to the chat assistant, and through `openpoet cli call`.

| Tool | Arguments |
| --- | --- |
| `openpoet_create_artifact` | `title`, `html_path` or `html`, `summary`, `task_id` |
| `openpoet_update_artifact` | `artifact_id`, `html_path` or `html`, `change_note`, `base_version` |
| `openpoet_patch_artifact` | `artifact_id`, `base_version`, `edits` (`[{old, new, replace_all}]`), `change_note` |
| `openpoet_read_artifact` | `artifact_id`, `version`, `output_path` (write the HTML to a file) or `include_html` (return it) |
| `openpoet_list_artifacts` | `project_id`, `task_id`, `session_id`, `include_archived` |
| `openpoet_list_artifact_annotations` | `artifact_id`, `status` (default `open`). Lists notes and answers, and names the questions of the current version that have no open answer |
| `openpoet_resolve_artifact_annotation` | `artifact_id`, `annotation_id`, `resolution_note` |

The link that opens an artifact for someone else comes from `openpoet_get_link`
(see [`links.md`](links.md)); the results of create, update, patch and read
carry it when a public base URL is configured.

Every publish result states the exact size and SHA-256 of the stored HTML and
whether it is byte for byte what was sent, and `openpoet_read_artifact` lists
them for each version.

`html_path` and `output_path` are for sessions (see above). The chat assistant
has no workspace and refuses them. `openpoet_patch_artifact` works everywhere.

Through the CLI, `html_file` publishes a document from a local file and
`output_file` saves the HTML of a version to one. The CLI runs as the user and
reads and writes the file itself, anywhere on the machine:

```bash
openpoet cli call openpoet_create_artifact '{"title":"Plan","html_file":"/tmp/plan.html"}'
openpoet cli call openpoet_read_artifact '{"artifact_id":"1a2b3c4d","output_file":"/tmp/plan.html"}'
```

Only the CLI honors `html_file` and `output_file`; the MCP server never opens
a path named by a client. Sessions started by OpenPoet have the binary in
`$OPENPOET_BIN` (not on a remote host, where it does not exist).

Tool policies written as an allowlist name every tool they grant. On upgrade,
allowlists that already grant `create_document` get the publishing tools,
those that grant `read_document` get the reading tools, and those that grant
`update_artifact` get `patch_artifact`. Policies that allow
everything are not touched. Remove the tools in the tool policy settings to opt
a project out.

Automation API capabilities (`artifacts.list`, `artifacts.get`,
`artifacts.get_version`, `artifacts.create`, `artifacts.update`,
`artifacts.patch`, `artifacts.update_meta`, `artifacts.annotations.list`, `.create`, `.resolve`,
`.reopen`, `artifacts.questions.list`, `artifacts.answers.save`, `.withdraw`)
use the `documents:read` and `documents:write` scopes. One automation
command is limited to the Automation API's 1 MiB request body, so larger
documents are published through the REST route or the MCP tools.

## How agents are steered to use it

- The managed instruction block that OpenPoet syncs into every harness
  (`.claude/CLAUDE.md`, `AGENTS.md`, Copilot and ACP instructions) tells agents
  to publish plans, reports and documentation as HTML artifacts, and to use a
  Doc for short plain text or when the artifact tools are unavailable.
- The built-in `html-artifact` skill is the design guide: what the sandbox
  enforces, how to structure a page with navigation, visual components, inline
  charts and restrained motion, how to keep it easy to annotate, how to ask
  the user questions, and the review loop. It is registered once; edit or
  delete it like any other skill. When the guide changes in a release, a copy
  that was never edited is replaced by the new text (the old one is kept as a
  skill version); an edited or deleted one is left as it is.
