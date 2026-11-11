package configsync

import (
	"context"
	"log"

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

const htmlArtifactSkillContent = `Design and publish interactive HTML artifacts in OpenPoet for plans, reports and documentation.

# HTML artifacts

An artifact is one HTML page you publish into OpenPoet with ` + "`openpoet_create_artifact`" + `.
The user opens it in a viewer, reads it like a small site, and annotates the
parts they want changed. Use it for a plan, a report, documentation, a design
proposal, a comparison, a review: anything the user will read or review. Use a
Doc (` + "`openpoet_create_document`" + `) for a short plain-text answer.

## The contract (the viewer enforces it)

The page runs in a sandbox with no network. What breaks these rules does not
load, silently.

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
  by ` + "`=`" + ` or ` + "`:`" + `. The tool result tells you when masking happened.

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

## The review loop

1. Publish with ` + "`openpoet_create_artifact`" + ` (` + "`title`" + `, ` + "`html`" + `, ` + "`summary`" + `). The user
   gets a card that opens it. Tell them in one or two sentences what it
   contains. Do not paste the HTML or a link.
2. When the user asks for changes or says they annotated it, call
   ` + "`openpoet_list_artifact_annotations`" + `. Each open annotation has the quoted
   text or element and the user's note. They are feedback on the document.
3. If you no longer have the HTML, get it with ` + "`openpoet_read_artifact`" + ` and
   ` + "`include_html: true`" + `.
4. Publish the whole corrected page with ` + "`openpoet_update_artifact`" + `, passing
   ` + "`base_version`" + ` (the version you worked from) and a one-line ` + "`change_note`" + `.
   If it reports a version conflict, read the current version, reapply your
   change and update again.
5. Call ` + "`openpoet_resolve_artifact_annotation`" + ` for each annotation you
   addressed, with a note saying what you changed. Leave open what you did not
   address, and say why in your reply.

Through the CLI, write the page to a temporary file outside the project and
publish it with ` + "`html_file`" + `:
` + "`$OPENPOET_BIN cli call openpoet_create_artifact '{\"title\":\"Plan\",\"html_file\":\"/tmp/plan.html\"}'`" + `.

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
// time this version starts. It never overwrites: a skill that already has the
// name (possibly the user's own) is left as it is, and once registration was
// recorded a deleted skill is not created again.
func EnsureHTMLArtifactSkill(ctx context.Context, db *database.DB) error {
	if done, _ := db.GetSetting(ctx, htmlArtifactSkillSetting); done != "" {
		return nil
	}
	skills, err := db.ListSkills(ctx)
	if err != nil {
		return err
	}
	exists := false
	for _, skill := range skills {
		if skill.Name == HTMLArtifactSkillName {
			exists = true
			break
		}
	}
	if !exists {
		skill := &database.Skill{Name: HTMLArtifactSkillName, Content: htmlArtifactSkillContent, Enabled: true, Category: "documentation"}
		if err := db.CreateSkillWithVersion(ctx, skill); err != nil {
			return err
		}
		log.Printf("[configsync] registered built-in skill %q (design guide for HTML artifacts)", HTMLArtifactSkillName)
	}
	return db.SetSetting(ctx, htmlArtifactSkillSetting, "1")
}
