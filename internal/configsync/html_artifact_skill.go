package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strconv"

	"openpoet/internal/database"
)

// The html-artifact skill is the design guide behind openpoet_create_artifact:
// the steering block tells every agent to publish plans and documentation as
// HTML artifacts, and this skill tells it how to build one that works in the
// sandboxed viewer and is worth reading.

const HTMLArtifactSkillName = "html-artifact"

// htmlArtifactSkillSetting records that the built-in skill was registered, so
// a user who deletes or renames it is not handed it back on the next start.
const htmlArtifactSkillSetting = "builtin_skill_html_artifact_registered"

// htmlArtifactSkillRevision is the revision of the built-in guide below, and
// htmlArtifactSkillRevisionSetting records the last revision an installation
// was offered. When the guide changes, bump the revision and add the SHA-256
// of the text it replaces to htmlArtifactSkillSuperseded.
const (
	htmlArtifactSkillRevision        = 4
	htmlArtifactSkillRevisionSetting = "builtin_skill_html_artifact_revision"
)

// htmlArtifactSkillSuperseded holds the SHA-256 of every earlier built-in text
// of the guide. A stored skill whose content is one of them was never edited
// by the user, so it can be replaced by the current text; anything else is the
// user's own and is left alone.
var htmlArtifactSkillSuperseded = map[string]bool{
	"326d24eab92e2d87211c3291790053c376570be2409b32eff246051139045e06": true, // revision 1
	"50443d4ee9bd35229bf8c417e7b19093f52f3e730c0a3d8b2deddd42b0fe1341": true, // revision 2
	"a894910e960c94469a52eb71433b739728dfbee6743219b8e45f6b6d8ac9ee6a": true, // revision 3
}

const htmlArtifactSkillContent = `Design and publish interactive HTML artifacts in OpenPoet for plans, reports and documentation.

# HTML artifacts

An artifact is one HTML page you publish into OpenPoet with ` + "`openpoet_create_artifact`" + `.
The user opens it in a viewer, reads it like a small site, and annotates the
parts they want changed. Use it for a plan, a report, documentation, a design
proposal, a comparison, a review: anything the user will read or review. Use a
Doc (` + "`openpoet_create_document`" + `) for a short plain-text answer.

## The contract (the viewer enforces it)

The page runs in a sandbox with no network. What breaks these rules does not
load, silently. It is served as ` + "`sandbox allow-scripts`" + ` with the policy
` + "`default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'`" + `.

- **One self-contained document.** All CSS in ` + "`<style>`" + `, all JavaScript in
  ` + "`<script>`" + `. No ` + "`<link>`" + `, no script ` + "`src`" + `, no CDN, no web fonts, no remote
  images. Images are inline SVG or ` + "`data:`" + ` URIs.
- **No external libraries by URL.** Draw charts with inline SVG or ` + "`<canvas>`" + `.
  If you truly need a library, its source has to be pasted inline, and it counts
  toward the size limit.
- **At most 2 MiB per version.** Aim far below: a good artifact is usually
  30–150 KB.
- **No storage, no cookies.** ` + "`localStorage`" + `, ` + "`sessionStorage`" + ` and
  ` + "`document.cookie`" + ` throw. Keep state in variables; wrap any storage use in
  try/catch.
- **No way out of the page.** ` + "`fetch`" + `, forms that submit, popups,
  ` + "`target=\"_blank\"`" + ` and links to other sites do nothing. Navigation is inside
  the page (` + "`#section`" + ` links, tabs, buttons). Show an external URL as text the
  user can copy.
- **Credential-looking text is masked.** ` + "`password=...`" + `, ` + "`api_key: ...`" + `,
  ` + "`secret = ...`" + ` are replaced with ` + "`[REDACTED]`" + ` when stored, also inside
  scripts. Never put real credentials in an artifact, and do not name script
  variables ` + "`password`" + `, ` + "`secret`" + `, ` + "`api_key`" + ` or ` + "`token`" + `-like names followed
  by ` + "`=`" + ` or ` + "`:`" + `. The tool result says how many values were masked and where:
  the line of the document you sent and the kind of text (` + "`password`" + `,
  ` + "`api_key`" + `...), never the value. To check a page before publishing it, pass
  ` + "`dry_run: true`" + `: nothing is stored, and the result says whether the page is
  accepted, its size and hash, and what would be masked.

## Build it like a small site

- **Lead with the answer.** Title, one-paragraph summary, then the key numbers
  or the decision. Detail comes after, in sections.
- **Navigation.** A sticky header or sidebar with links to the sections, or
  tabs when the sections are alternatives. Mark the current section. Every
  section has an ` + "`id`" + `.
- **Visual components instead of walls of text.** Cards for options, callouts
  for risks and decisions, status badges, step lists and timelines for
  sequences, tables for comparisons, ` + "`<details>`" + ` for depth the reader may skip.
- **Diagrams and charts, inline.** Architecture and flows as inline SVG with
  labelled boxes and arrows. Data as SVG or canvas charts: label values
  directly, add a legend when there is more than one series, use tabular
  numbers, and state the unit.
- **Motion, with restraint.** Transitions of 150–250 ms on state changes
  (tab switch, expand, hover). No looping or autoplaying animation. Honor
  ` + "`@media (prefers-reduced-motion: reduce)`" + `.
- **Interactivity that helps reading.** Filters, toggles between views,
  expandable rows, a chart that responds to a selector. Not decoration.

## Make it hold up

- **Both themes.** Define colors as CSS custom properties and redefine them in
  ` + "`@media (prefers-color-scheme: dark)`" + `. Always set the page background and
  text color yourself: the viewer's default background is white.
- **Phone width.** Fluid layout, readable at 360 px, no horizontal page
  scroll; wide tables scroll inside their own container. Keep prose lines
  under about 72 characters.
- **Accessible.** Real headings in order, ` + "`<button>`" + ` for actions, visible
  focus, sufficient contrast, ` + "`alt`" + ` or ` + "`aria-label`" + ` on meaningful graphics.
- **System fonts.** ` + "`font-family: system-ui, sans-serif`" + ` and a monospace stack
  for code.

## Make it easy to annotate and to revise

- The user annotates by selecting text or clicking an element. Keep content as
  **real text and real elements**: text painted into a canvas or baked into an
  image cannot be selected.
- Give sections and important elements **stable ` + "`id`" + ` attributes**, and keep
  wording you are not changing as it is. Annotations are placed again on a new
  version by their quoted text and element, so stable text keeps them attached.
- Put the content of **every tab, step and accordion in the DOM from the
  start** and hide the inactive ones with CSS (` + "`hidden`" + `, ` + "`display:none`" + `). An
  annotation is found again by its element: content that only exists while its
  tab is open cannot be.
- Give **every tab, panel and card its own ` + "`id`" + ` or ` + "`data-id`" + `** (` + "`data-id=\"TM8\"`" + `).
  An annotation records the nearest one: the listing tells you which panel a
  note is about ("inside #stage-2"), and the viewer places the note only in
  that panel. Without it, a note made in one tab can land on the content that
  another tab puts in the same position.

## Ask the user to decide

When the user has to choose or answer something, declare questions. Do not
build your own radios, checkboxes or text boxes for it, and do not tell the
user to "annotate the option": a choice made inside the page is lost when the
page reloads and never reaches you.

1. **Declare every question once**, in a data block. OpenPoet reads it when you
   publish; a malformed block refuses the publish and says what is wrong.

` + "```" + `
<script type="application/json" id="openpoet-questions">
{"questions":[
  {"id":"D1","prompt":"What do we call it?","type":"single",
   "options":[{"id":"a","label":"Routine"},{"id":"b","label":"Automation"}]},
  {"id":"D2","prompt":"Which triggers ship first?","type":"multi",
   "options":[{"id":"form","label":"Form"},{"id":"chat","label":"Chat"}]},
  {"id":"NOTES","prompt":"Anything else we should know?","type":"text"}
]}
</script>
` + "```" + `

   - ` + "`type`" + `: ` + "`single`" + ` (one option), ` + "`multi`" + ` (one or more) or ` + "`text`" + ` (free
     text).
   - Ids are letters, digits and ` + "`_ - . :`" + `, up to 64 characters. **Keep them
     the same across versions**: an answer follows its question id.
   - Up to 200 questions and 30 options each; a prompt of up to 500 characters,
     a label of up to 300. ` + "`\"comment\": false`" + ` removes the comment box of a
     choice.

2. **Mark the elements.** They may be created by script. Use ` + "`<button>`" + ` for
   the options.

` + "```" + `
<section data-openpoet-question="D1">
  <h3>What do we call it?</h3>
  <button data-openpoet-option="a">Routine</button>
  <button data-openpoet-option="b">Automation</button>
</section>
<section data-openpoet-question="NOTES">
  <h3>Anything else we should know?</h3>
  <button data-openpoet-answer>Write an answer</button>
</section>
` + "```" + `

3. **Style the state with CSS.** OpenPoet sets ` + "`data-openpoet-selected`" + ` on the
   chosen options, ` + "`data-openpoet-answered=\"pending\"`" + ` or ` + "`\"saved\"`" + ` on the
   question, and ` + "`aria-pressed`" + `. ` + "`html[data-openpoet-host]`" + ` is set while the
   page is read inside OpenPoet.

` + "```" + `
[data-openpoet-selected] { border-color: var(--accent); }
[data-openpoet-answered="pending"] h3::after { content: " · to confirm"; }
` + "```" + `

   For a custom widget there is ` + "`window.openpoet.answer(questionId, optionId)`" + `,
   ` + "`window.openpoet.ask(questionId)`" + ` and the ` + "`openpoet:answers`" + ` event on
   ` + "`document`" + ` (` + "`event.detail.answers`" + ` maps each question id to its chosen
   option ids). Call them from event handlers, not while the page loads.

What happens: a tap marks the choice, and the user confirms the answers in the
OpenPoet panel. Comments and free-text answers are typed there, never inside
your page, and your script cannot read them. Each answer reaches you as an
annotation: ` + "`openpoet_list_artifact_annotations`" + ` lists it as an answer to its
question, with the option chosen and the comment, and names the questions still
unanswered. Act on the answers, publish the new version and resolve each one
saying what you did.

Put the recommendation and the trade-offs of each option in the page, next to
the option. The label in the data block is only the name of the option.

## Publish from a file, change with a patch

Do not type the page into a tool argument when you can avoid it. A page of
150 KB passed inline in ` + "`html`" + ` costs minutes and its whole size in tokens, and
costs them again on every revision.

- **Write the page to an ` + "`.html`" + ` file** with your file tools and pass its path
  as ` + "`html_path`" + ` to ` + "`openpoet_create_artifact`" + ` or ` + "`openpoet_update_artifact`" + `.
  OpenPoet reads the file. The path has to be inside your workspace (a relative
  path resolves against it) or the temporary directory. Use a scratch or
  temporary directory, so the file does not end up in the project's repository;
  if you had to put it in the project, delete it when the review is over.
- **Change a published page with ` + "`openpoet_patch_artifact`" + `**: a list of
  ` + "`{old, new}`" + ` replacements applied to the current version, with
  ` + "`base_version`" + `. Each ` + "`old`" + ` has to match the stored HTML exactly (whitespace
  and line breaks included) and occur once; give it enough surrounding text to
  be unique, or set ` + "`replace_all`" + `. Edits apply in order. If one does not match,
  nothing is published and the error names it. Only the replacements travel, so
  a one-line fix to a large page costs one line. Make the same edit in your
  file to keep it in step.
- **Get the HTML back into a file** with ` + "`openpoet_read_artifact`" + ` and
  ` + "`output_path`" + `: the stored version is written byte for byte, and is not
  returned in the result. Use ` + "`include_html`" + ` only for a small page.
- **Check what was stored.** Every publish reports the exact size in bytes and
  the SHA-256 of the stored HTML, and says whether it is byte for byte what
  you sent (it is, unless credential-looking text was masked). Compare it with
  ` + "`sha256sum`" + ` of your file when it matters.
- Pass the page inline in ` + "`html`" + ` only when you cannot write a file.
- A create that timed out can be repeated: the same title and identical HTML
  from the same session within a few minutes returns the artifact already
  created, not a second one.

## The review loop

1. Publish with ` + "`openpoet_create_artifact`" + ` (` + "`title`" + `, ` + "`html_path`" + `, ` + "`summary`" + `). The
   user gets a card that opens it. Tell them in one or two sentences what it
   contains. Do not paste the HTML or a link.
2. When the user asks for changes or says they annotated it, call
   ` + "`openpoet_list_artifact_annotations`" + `. Each open annotation has the quoted
   text or element and the user's note, or is the user's answer to one of your
   questions. They are feedback on the document. A note made on an earlier
   version says whether its quoted text or element is still in the current
   one: when it is not, the passage was already rewritten or removed.
3. If you no longer have the HTML, get it with ` + "`openpoet_read_artifact`" + ` and
   ` + "`output_path`" + `.
4. Publish the change, passing ` + "`base_version`" + ` (the version you worked from)
   and a one-line ` + "`change_note`" + `: ` + "`openpoet_patch_artifact`" + ` for changes to parts
   of the page, ` + "`openpoet_update_artifact`" + ` with ` + "`html_path`" + ` for a rewrite. If it
   reports a version conflict, read the current version, reapply your change
   and publish again. Either one takes ` + "`title`" + ` and ` + "`summary`" + ` when the change
   calls for new ones; ` + "`openpoet_update_artifact`" + ` with only those renames the
   artifact without publishing a version.
5. Resolve what you addressed in one call: ` + "`openpoet_resolve_artifact_annotation`" + `
   with ` + "`resolutions`" + `, a list of ` + "`{annotation_id, resolution_note}`" + `, each note
   saying what you changed for that annotation. Leave open what you did not
   address, and say why in your reply.
6. When the user asks for a link to the artifact, get it with
   ` + "`openpoet_get_link`" + `, or use the shareable link a publish or
   ` + "`openpoet_read_artifact`" + ` returned. Never build a URL yourself.

The same tools are available from a shell when ` + "`$OPENPOET_BIN`" + ` is set (it is in
local sessions): ` + "`$OPENPOET_BIN cli call openpoet_create_artifact '{\"title\":\"Plan\",\"html_file\":\"/tmp/plan.html\"}'`" + `.
There, ` + "`html_file`" + ` and ` + "`output_file`" + ` name files anywhere on your machine,
read and written by the CLI itself.

## Starting point

` + "```html" + `
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Rollout plan</title>
<style>
  :root { --bg:#ffffff; --panel:#f5f6f8; --text:#1c1f24; --muted:#5b6472; --line:#d9dde3; --accent:#2f6fed; }
  @media (prefers-color-scheme: dark) {
    :root { --bg:#14171c; --panel:#1d2128; --text:#e7eaee; --muted:#9aa3b2; --line:#2e343d; --accent:#7aa5ff; }
  }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--text); font:16px/1.6 system-ui, sans-serif; }
  header { position:sticky; top:0; display:flex; gap:16px; align-items:center; padding:12px 20px;
           background:var(--panel); border-bottom:1px solid var(--line); overflow-x:auto; }
  header a { color:var(--muted); text-decoration:none; white-space:nowrap; }
  header a[aria-current="true"] { color:var(--accent); }
  main { max-width:72ch; margin:0 auto; padding:24px 20px 64px; }
  section { scroll-margin-top:64px; }
  .card { background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:16px; }
  @media (prefers-reduced-motion: no-preference) { .card { transition: border-color .2s; } }
</style>
</head>
<body>
<header><strong>Rollout plan</strong><a href="#summary">Summary</a><a href="#stages">Stages</a><a href="#risks">Risks</a></header>
<main>
  <section id="summary"><h1>Rollout plan</h1><p>…</p></section>
  <section id="stages"><h2>Stages</h2><div class="card">…</div></section>
  <section id="risks"><h2>Risks</h2>…</section>
</main>
<script>
  // Mark the section in view in the navigation.
  const links = [...document.querySelectorAll('header a')];
  const observer = new IntersectionObserver(entries => entries.forEach(entry => {
    if (entry.isIntersecting) links.forEach(a => a.setAttribute('aria-current', String(a.hash === '#' + entry.target.id)));
  }), { rootMargin: '-40% 0px -55% 0px' });
  document.querySelectorAll('main section').forEach(section => observer.observe(section));
</script>
</body>
</html>
` + "```" + `
`

// EnsureHTMLArtifactSkill registers the built-in html-artifact skill the first
// time this version starts, and brings an untouched copy up to date when the
// guide changes. It never overwrites the user's work: a skill that already has
// the name when the built-in is first registered is left as it is, a deleted
// skill is not created again, and a skill whose text is not one of the earlier
// built-in texts (the user edited it) is not replaced.
func EnsureHTMLArtifactSkill(ctx context.Context, db *database.DB) error {
	registered, _ := db.GetSetting(ctx, htmlArtifactSkillSetting)
	offered, _ := db.GetSetting(ctx, htmlArtifactSkillRevisionSetting)
	revision, _ := strconv.Atoi(offered)
	if registered != "" && revision >= htmlArtifactSkillRevision {
		return nil
	}
	skills, err := db.ListSkills(ctx)
	if err != nil {
		return err
	}
	var existing *database.Skill
	for i := range skills {
		if skills[i].Name == HTMLArtifactSkillName {
			existing = &skills[i]
			break
		}
	}
	switch {
	case registered == "" && existing == nil:
		skill := &database.Skill{Name: HTMLArtifactSkillName, Content: htmlArtifactSkillContent, Enabled: true, Category: "documentation"}
		if err := db.CreateSkillWithVersion(ctx, skill); err != nil {
			return err
		}
		log.Printf("[configsync] registered built-in skill %q (design guide for HTML artifacts)", HTMLArtifactSkillName)
	case registered == "":
		// A skill with this name was there before the built-in: the user's.
	case existing == nil:
		// Registered earlier and deleted since: it stays deleted.
	case htmlArtifactSkillSuperseded[htmlArtifactSkillHash(existing.Content)]:
		existing.Content = htmlArtifactSkillContent
		if err := db.UpdateSkillWithVersion(ctx, existing, true); err != nil {
			return err
		}
		log.Printf("[configsync] updated built-in skill %q to revision %d (it had not been edited)", HTMLArtifactSkillName, htmlArtifactSkillRevision)
	case existing.Content != htmlArtifactSkillContent:
		log.Printf("[configsync] built-in skill %q has a new revision (%d), but the stored one was edited and is left as it is; the current text is in the OpenPoet source (internal/configsync/html_artifact_skill.go)",
			HTMLArtifactSkillName, htmlArtifactSkillRevision)
	}
	if err := db.SetSetting(ctx, htmlArtifactSkillSetting, "1"); err != nil {
		return err
	}
	return db.SetSetting(ctx, htmlArtifactSkillRevisionSetting, strconv.Itoa(htmlArtifactSkillRevision))
}

func htmlArtifactSkillHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
