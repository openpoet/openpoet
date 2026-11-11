#!/usr/bin/env node

// Annotations on an artifact made of tabs, against a running dev instance.
// The page mounts one tab at a time into the same container (so two tabs put
// different content at the same position) and also has panels that stay in
// the page and are hidden with CSS. A note records the id or data-id of the
// panel it was made in, is placed only in that panel, and is not drawn while
// its panel is closed.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

const PAGE = `<!doctype html><html><head><meta charset="utf-8"><title>Tabs</title>
<style>body{font-family:system-ui;margin:2rem}[hidden]{display:none}.tab{margin-right:8px}</style></head>
<body>
<h1 id="title">Rollout by team</h1>
<nav><button class="tab" data-open="TM8">Team 8</button><button class="tab" data-open="TM9">Team 9</button></nav>
<main id="mount"></main>
<section id="kept-a"><p>Always here: first panel.</p></section>
<section id="kept-b" hidden><p>Always here: second panel.</p><button>Approve</button></section>
<button id="toggle">Toggle second panel</button>
<script>
  var teams = {
    TM8: '<div data-id="TM8"><p>Budget is 40k.</p><button>Launch</button></div>',
    TM9: '<div data-id="TM9"><p>Budget is 90k.</p><button>Hold</button></div>'
  };
  function show(id) { document.getElementById('mount').innerHTML = teams[id]; }
  document.querySelectorAll('.tab').forEach(function (b) { b.addEventListener('click', function () { show(b.dataset.open); }); });
  document.getElementById('toggle').addEventListener('click', function () {
    var panel = document.getElementById('kept-b'); panel.hidden = !panel.hidden;
  });
  show('TM8');
</script>
</body></html>`;

async function api(page, method, path, body) {
  return page.evaluate(async ({ method, path, body }) => {
    const response = await fetch(path, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await response.text();
    let json = null;
    try { json = JSON.parse(text); } catch {}
    return { status: response.status, json, text };
  }, { method, path, body });
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1366, height: 900 } });
  const pageErrors = [];
  page.on('pageerror', err => pageErrors.push(err.message));
  let artifactId = '';

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });
    const created = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E annotation panels', html: PAGE, session_id: `e2e-panels-${Date.now()}`,
    });
    assert.equal(created.status, 201, created.text);
    artifactId = created.json.id;

    await page.evaluate(id => window.artifactViewer.open(id), artifactId);
    const frame = page.frameLocator('#artifact-viewer-frame');
    await frame.locator('[data-id="TM8"]').waitFor();
    const items = page.locator('#artifact-panel-list .artifact-annotation');
    const composer = page.locator('#artifact-panel-composer:not(.hidden)');
    const boxes = kind => frame.locator(`openpoet-annotations .box.${kind}`);
    const badges = frame.locator('openpoet-annotations .badge');
    const save = async text => {
      await composer.waitFor();
      await page.locator('#artifact-composer-text').fill(text);
      await page.locator('#artifact-composer-save').click();
    };

    // --- a note on the button of the tab that is mounted (team 8)
    await page.locator('#artifact-viewer-annotate').click();
    await page.locator('#artifact-viewer-panel:not(.hidden)').waitFor();
    await frame.locator('[data-id="TM8"] button').click();
    await composer.waitFor();
    assert.equal(await page.locator('#artifact-composer-quote').textContent(), '<button> Launch');
    await save('Rename for team 8');
    await page.waitForFunction(() => document.querySelectorAll('#artifact-panel-list .artifact-annotation').length === 1);
    await boxes('element').waitFor({ state: 'attached' });

    // --- a note on a sentence of the panel that stays in the page
    await frame.locator('#kept-a p').evaluate(el => {
      const range = document.createRange();
      range.selectNodeContents(el);
      const selection = getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    await save('Shorter');
    await page.waitForFunction(() => document.querySelectorAll('#artifact-panel-list .artifact-annotation').length === 2);

    // --- and one on the button of the panel that is hidden with CSS, while it is open
    await page.locator('#artifact-viewer-annotate').click(); // leave annotate mode: the artifact gets its clicks
    await frame.locator('#toggle').click();
    await frame.locator('#kept-b button').waitFor();
    await page.locator('#artifact-viewer-annotate').click();
    await frame.locator('#kept-b button').click();
    await save('Who approves');
    await page.waitForFunction(() => document.querySelectorAll('#artifact-panel-list .artifact-annotation').length === 3);
    await page.locator('#artifact-viewer-annotate').click();

    // What the agent reads: each note names the panel it was made in, and the
    // selector starts at that panel instead of at a position in the page.
    const open = await api(page, 'GET', `/api/artifacts/${artifactId}/annotations?status=open`);
    assert.equal(open.status, 200, open.text);
    assert.deepEqual(open.json.map(a => [a.anchor.type, a.quote, a.anchor.near, a.anchor.selector]), [
      ['element', 'Launch', '[data-id="TM8"]', '[data-id="TM8"] > button:nth-of-type(1)'],
      ['text', 'Always here: first panel.', '#kept-a', '#kept-a > p:nth-of-type(1)'],
      ['element', 'Approve', '#kept-b', '#kept-b > button:nth-of-type(1)'],
    ]);
    const [teamNote, , hiddenNote] = open.json;
    // The viewer shows the panel next to each note.
    assert.deepEqual(await items.evaluateAll(nodes => nodes.map(n => n.querySelector('.is-near')?.textContent)), ['TM8', '#kept-a', '#kept-b']);

    // All three are drawn while their panels are shown.
    await frame.locator('openpoet-annotations .badge').nth(2).waitFor({ state: 'attached' });
    assert.equal(await badges.count(), 3);

    // --- the other tab is mounted in the same place: the team 8 note is not
    // drawn on team 9's button, and is reported as not in the page.
    await frame.locator('[data-open="TM9"]').click();
    await frame.locator('[data-id="TM9"]').waitFor();
    await page.locator(`#artifact-panel-list .artifact-annotation[data-annotation-id="${teamNote.id}"] .is-orphan`).waitFor();
    assert.equal(await boxes('element').count(), 1, 'only the note of the open panel #kept-b is outlined');
    assert.equal(await badges.count(), 2);

    // --- back on its own tab it is drawn again
    await frame.locator('[data-open="TM8"]').click();
    await frame.locator('[data-id="TM8"]').waitFor();
    await page.waitForFunction(id => !document.querySelector(`#artifact-panel-list .artifact-annotation[data-annotation-id="${id}"] .is-orphan`), teamNote.id);
    assert.equal(await boxes('element').count(), 2);

    // --- closing the CSS panel: its note is found but not drawn, and no badge
    // is left in the corner of the page.
    await frame.locator('#toggle').click();
    await page.locator(`#artifact-panel-list .artifact-annotation[data-annotation-id="${hiddenNote.id}"] .is-hidden-part`).waitFor();
    assert.equal(await page.locator(`#artifact-panel-list .artifact-annotation[data-annotation-id="${hiddenNote.id}"] .is-orphan`).count(), 0);
    assert.equal(await badges.count(), 2);
    const corner = await badges.evaluateAll(nodes => nodes.filter(n => parseFloat(n.style.left) <= 12 && parseFloat(n.style.top) <= 22).length);
    assert.equal(corner, 0, 'a badge was drawn in the corner for a note in a closed panel');

    // --- and it comes back when the panel is opened
    await frame.locator('#toggle').click();
    await page.waitForFunction(id => !document.querySelector(`#artifact-panel-list .artifact-annotation[data-annotation-id="${id}"] .is-hidden-part`), hiddenNote.id);
    assert.equal(await badges.count(), 3);

    // --- for the agent: a later version drops team 8; the listing says so
    const updated = await api(page, 'PUT', `/api/artifacts/${artifactId}`, {
      html: PAGE.replace(/TM8: '.*?',\n/, "TM8: '<div data-id=\"TM7\"><p>Merged into team 7.</p></div>',\n").replace('first panel', 'first panel, reworded'),
      base_version: 1,
    });
    assert.equal(updated.status, 200, updated.text);
    assert.equal(updated.json.version, 2);
    const placed = await api(page, 'GET', `/api/artifacts/${artifactId}/annotations?status=open&placement=1`);
    assert.deepEqual(placed.json.map(a => [a.quote, a.placement.version, a.placement.checked, a.placement.found]), [
      ['Launch', 2, true, false],
      ['Always here: first panel.', 2, true, false],
      ['Approve', 2, true, true],
    ]);

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact annotation panels smoke: OK');
  } finally {
    if (artifactId) {
      await api(page, 'PATCH', `/api/artifacts/${artifactId}`, { status: 'archived' }).catch(() => {});
    }
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
