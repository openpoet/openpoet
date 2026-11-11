#!/usr/bin/env node

// Annotations on an HTML artifact, against a running dev instance: the user
// annotates a text range and an element, the notes reach the API an agent
// reads, a resolution shows up live, and a new version re-anchors what it can.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';
const NOTE_TEXT = 'Tighten this sentence e2e-note-marker';
const NOTE_ELEMENT = 'Rename this button e2e-button-marker';

// The artifact records every message it receives and tries to forge bridge
// messages, as a hostile artifact sharing the frame with the bridge could.
const spyScript = `<script>
window.__seen = [];
window.addEventListener('message', e => { try { window.__seen.push(JSON.stringify(e.data)); } catch (_) {} });
function forge(anchor) { parent.postMessage({ source: 'openpoet-artifact-bridge', type: 'selection', anchor: anchor }, '*'); }
forge({ type: 'text', selector: 'body', exact: 'forged before annotate mode' });
</script>`;

const V1 = `<!doctype html><html><head><meta charset="utf-8"><title>Plan</title>
<style>body{font-family:system-ui;margin:2rem;line-height:1.6}button{padding:.5rem 1rem}</style></head>
<body>
<h1>Rollout plan</h1>
<p id="p1">The migration runs in three stages over two weeks.</p>
<p id="p2">Each stage has a rollback point and a named owner.</p>
<button id="cta" onclick="this.textContent='clicked'">Launch</button>
${spyScript}
</body></html>`;

// v2 moves the annotated sentence into a new section (its selector changes)
// and removes the annotated button.
const V2 = `<!doctype html><html><head><meta charset="utf-8"><title>Plan</title>
<style>body{font-family:system-ui;margin:2rem;line-height:1.6}</style></head>
<body>
<h1>Rollout plan</h1>
<section id="stages"><h2>Stages</h2>
<div class="card"><p>Intro. Each stage has a rollback point and a named owner. More detail follows.</p></div>
</section>
<p id="p1">The migration runs in three stages over two weeks.</p>
${spyScript}
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
      title: 'E2E annotations', html: V1, session_id: `e2e-annotations-${Date.now()}`,
    });
    assert.equal(created.status, 201, created.text);
    artifactId = created.json.id;

    await page.evaluate(id => window.artifactViewer.open(id), artifactId);
    const frame = page.frameLocator('#artifact-viewer-frame');
    await frame.locator('#p2').waitFor();
    const composer = page.locator('#artifact-panel-composer');
    const items = page.locator('#artifact-panel-list .artifact-annotation');

    // A forged "selection" from the artifact must not open the composer while
    // the user has not turned annotate mode on.
    await page.waitForTimeout(500);
    assert.equal(await composer.evaluate(el => el.classList.contains('hidden')), true, 'forged selection opened the composer');

    // Outside annotate mode the artifact behaves normally.
    assert.equal(await page.locator('#artifact-viewer-panel').evaluate(el => el.classList.contains('hidden')), true);

    // --- annotate a text range
    await page.locator('#artifact-viewer-annotate').click();
    assert.equal(await page.locator('#artifact-viewer-annotate').getAttribute('aria-pressed'), 'true');
    await page.locator('#artifact-viewer-panel:not(.hidden)').waitFor();
    assert.match(await page.locator('#artifact-panel-hint').textContent(), /Select text or click an element/);

    await frame.locator('#p2').evaluate(el => {
      const text = el.firstChild;
      const start = text.data.indexOf('rollback point');
      const range = document.createRange();
      range.setStart(text, start);
      range.setEnd(text, start + 'rollback point'.length);
      const selection = getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    await page.locator('#artifact-panel-composer:not(.hidden)').waitFor();
    assert.equal(await page.locator('#artifact-composer-quote').textContent(), 'rollback point');
    await page.locator('#artifact-composer-text').fill(NOTE_TEXT);

    // While the user is typing, the artifact cannot re-point the note at other
    // text, nor throw the typed text away with a forged Escape.
    await frame.locator('body').evaluate(() => {
      forge({ type: 'text', selector: 'body', exact: 'something the user never selected' });
      parent.postMessage({ source: 'openpoet-artifact-bridge', type: 'key', key: 'Escape' }, '*');
    });
    await page.waitForTimeout(300);
    assert.equal(await page.locator('#artifact-composer-quote').textContent(), 'rollback point', 'a forged selection re-pointed the note being typed');
    assert.equal(await page.locator('#artifact-composer-text').inputValue(), NOTE_TEXT, 'a forged Escape discarded the typed note');

    await page.locator('#artifact-composer-save').click();
    await items.first().waitFor();
    assert.equal(await items.count(), 1);
    assert.equal(await items.first().locator('.artifact-annotation-number').textContent(), '1');
    assert.equal(await items.first().locator('.artifact-annotation-quote').textContent(), 'rollback point');
    assert.equal(await items.first().locator('.artifact-annotation-body').textContent(), NOTE_TEXT);
    assert.equal(await page.locator('#artifact-viewer-open-count').textContent(), '1');
    // The highlight and its numbered badge are drawn inside the frame.
    await frame.locator('openpoet-annotations .box.text').first().waitFor({ state: 'attached' });
    assert.equal(await frame.locator('openpoet-annotations .badge').textContent(), '1');

    // --- annotate an element: the click annotates, the artifact's handler does not run
    await frame.locator('#cta').click();
    await page.locator('#artifact-panel-composer:not(.hidden)').waitFor();
    assert.equal(await page.locator('#artifact-composer-quote').textContent(), '<button> Launch');
    assert.equal(await frame.locator('#cta').textContent(), 'Launch', "annotate mode must not trigger the artifact's own click handlers");
    await page.locator('#artifact-composer-text').fill(NOTE_ELEMENT);
    await page.locator('#artifact-composer-save').click();
    await page.waitForFunction(() => document.querySelectorAll('#artifact-panel-list .artifact-annotation').length === 2);
    await frame.locator('openpoet-annotations .box.element').waitFor({ state: 'attached' });

    // A forged selection carrying markup is shown as text, never as HTML.
    await frame.locator('body').evaluate(() => forge({ type: 'text', selector: 'body', exact: '<img src=x onerror="document.title=\'pwned\'">' }));
    await page.locator('#artifact-panel-composer:not(.hidden)').waitFor();
    assert.equal(await page.locator('#artifact-composer-quote img').count(), 0, 'a quote from the frame was rendered as HTML');
    assert.notEqual(await page.title(), 'pwned');
    await page.locator('#artifact-composer-cancel').click();

    // Leaving annotate mode gives the artifact its clicks back.
    await page.locator('#artifact-viewer-annotate').click();
    await frame.locator('#cta').click();
    assert.equal(await frame.locator('#cta').textContent(), 'clicked');

    // --- what an agent reads: open annotations, with the quote and the version
    const open = await api(page, 'GET', `/api/artifacts/${artifactId}/annotations?status=open`);
    assert.equal(open.status, 200, open.text);
    assert.deepEqual(open.json.map(a => [a.version, a.status, a.anchor.type, a.quote, a.body]), [
      [1, 'open', 'text', 'rollback point', NOTE_TEXT],
      [1, 'open', 'element', 'Launch', NOTE_ELEMENT],
    ]);
    const [textNote, elementNote] = open.json;
    const detail = await api(page, 'GET', `/api/artifacts/${artifactId}`);
    assert.equal(detail.json.open_annotations, 2);

    // --- the text of a note never enters the frame
    const seen = await frame.locator('body').evaluate(() => window.__seen.join('\n'));
    assert.ok(seen.includes('show-annotations'), 'the bridge protocol was not observed by the spy');
    for (const marker of ['e2e-note-marker', 'e2e-button-marker']) {
      assert.ok(!seen.includes(marker), `note text reached the artifact frame: ${marker}`);
    }

    // --- an agent resolves one: the open viewer follows without a reload
    const resolved = await api(page, 'POST', `/api/artifacts/${artifactId}/annotations/${textNote.id}/resolve`, {
      resolution_note: 'Rewrote the sentence in v2.',
    });
    assert.equal(resolved.status, 200, resolved.text);
    assert.equal(resolved.json.status, 'resolved');
    await page.waitForFunction(() => document.getElementById('artifact-panel-count-resolved').textContent === '1');
    assert.equal(await page.locator('#artifact-panel-count-open').textContent(), '1');
    await page.locator('#artifact-panel-tab-resolved').click();
    assert.equal(await items.count(), 1);
    assert.match(await items.first().locator('.artifact-annotation-resolution').textContent(), /Resolved by .*Rewrote the sentence in v2\./);

    // Reopen from the UI, then resolve again without a note.
    await items.first().getByRole('button', { name: 'Reopen' }).click();
    await page.waitForFunction(() => document.getElementById('artifact-panel-count-open').textContent === '2');
    const stray = await api(page, 'POST', `/api/artifacts/other-artifact/annotations/${textNote.id}/resolve`, {});
    assert.equal(stray.status, 404, 'an annotation must only be changed through its own artifact');

    // --- new version: the moved sentence is re-anchored, the removed button is not
    const updated = await api(page, 'PUT', `/api/artifacts/${artifactId}`, { html: V2, base_version: 1 });
    assert.equal(updated.status, 200, updated.text);
    await page.locator('#artifact-viewer-banner-open').click();
    await frame.locator('#stages').waitFor();
    await page.locator('#artifact-panel-tab-open').click();
    await frame.locator('openpoet-annotations .box.text').first().waitFor({ state: 'attached' });
    await page.locator('#artifact-panel-list .artifact-annotation-tag.is-orphan').waitFor();
    const rows = await items.evaluateAll(nodes => nodes.map(n => ({
      id: Number(n.dataset.annotationId),
      orphan: !!n.querySelector('.is-orphan'),
      version: n.querySelector('.artifact-annotation-tag').textContent,
    })));
    assert.deepEqual(rows, [
      { id: textNote.id, orphan: false, version: 'v1' },
      { id: elementNote.id, orphan: true, version: 'v1' },
    ]);
    assert.equal(await frame.locator('openpoet-annotations .box.element').count(), 0);
    // Both stay open for the agent, whatever the viewer could place.
    const stillOpen = await api(page, 'GET', `/api/artifacts/${artifactId}/annotations?status=open`);
    assert.equal(stillOpen.json.length, 2);

    // Clicking a note scrolls the artifact to it and marks it active.
    await items.first().click();
    await page.locator(`#artifact-panel-list .artifact-annotation.is-active[data-annotation-id="${textNote.id}"]`).waitFor();
    await frame.locator('openpoet-annotations .box.flash').first().waitFor({ state: 'attached' });

    // --- "Send to agent" with no live session says so and keeps the notes
    assert.equal(await page.locator('#artifact-panel-send').isEnabled(), true);
    await page.locator('#artifact-panel-send').click();
    await page.locator('#artifact-panel-hint.is-error').waitFor();
    assert.match(await page.locator('#artifact-panel-hint').textContent(), /annotations stay open/);

    // --- the downloaded copy is never a text/html blob in the OpenPoet origin
    const blobType = await page.evaluate(async () => {
      const original = URL.createObjectURL;
      let type = null;
      URL.createObjectURL = blob => { type = blob.type; return original.call(URL, blob); };
      try { await window.artifactViewer._download(); } finally { URL.createObjectURL = original; }
      return type;
    });
    assert.equal(blobType, 'application/octet-stream');

    // --- validation at the API
    const bad = await api(page, 'POST', `/api/artifacts/${artifactId}/annotations`, { anchor: { type: 'weird' }, body: 'x' });
    assert.equal(bad.status, 400, bad.text);
    assert.equal(bad.json.code, 'invalid_annotation_anchor');
    const empty = await api(page, 'POST', `/api/artifacts/${artifactId}/annotations`, { anchor: { type: 'text', exact: 'x' }, body: '  ' });
    assert.equal(empty.status, 400, empty.text);

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact annotations smoke: OK');
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
