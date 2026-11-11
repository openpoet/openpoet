#!/usr/bin/env node

// Questions an artifact asks its reader, against a running dev instance: a tap
// on a choice inside the sandboxed frame marks it, nothing is stored until the
// user confirms in the OpenPoet page, the artifact's own script cannot answer
// for the user, and what the user types never enters the frame.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';
const COMMENT = 'Fewer collisions e2e-comment-marker';
const FREE_TEXT = 'Validate in staging first e2e-text-marker';

const questions = {
  questions: [
    { id: 'D1', prompt: 'What do we call it?', type: 'single', options: [{ id: 'a', label: 'Routine' }, { id: 'b', label: 'Automation' }] },
    { id: 'D2', prompt: 'Which triggers ship first?', type: 'multi', comment: false, options: [{ id: 'form', label: 'Form' }, { id: 'chat', label: 'Chat' }, { id: 'cron', label: 'Schedule' }] },
    { id: 'NOTES', prompt: 'Anything else?', type: 'text' },
  ],
};

// The page builds its questions from script, as artifacts usually do, records
// every message it receives, and can forge bridge messages like a hostile
// artifact sharing the frame with the bridge could.
function artifact(declared) {
  return `<!doctype html><html><head><meta charset="utf-8"><title>Decisions</title>
<style>body{font-family:system-ui;margin:1.5rem}button{display:block;margin:.4rem 0;padding:.6rem 1rem}
[data-openpoet-selected]{outline:3px solid #2563eb}</style></head>
<body><h1 id="title">Decisions</h1><div id="root"></div>
<script type="application/json" id="openpoet-questions">${JSON.stringify(declared)}</script>
<script>
window.__seen = [];
window.addEventListener('message', e => { try { window.__seen.push(JSON.stringify(e.data)); } catch (_) {} });
window.__events = [];
document.addEventListener('openpoet:answers', e => window.__events.push(JSON.stringify(e.detail.answers)));
function forge(message) { parent.postMessage(Object.assign({ source: 'openpoet-artifact-bridge', type: 'answer-intent' }, message), '*'); }
var declared = JSON.parse(document.getElementById('openpoet-questions').textContent).questions;
document.getElementById('root').innerHTML = declared.map(function (q) {
  return '<section data-openpoet-question="' + q.id + '"><h2>' + q.prompt + '</h2>' +
    (q.options || []).map(function (o) { return '<button data-openpoet-option="' + o.id + '"><span>' + o.label + '</span></button>'; }).join('') +
    (q.type === 'text' ? '<button data-openpoet-answer>Write an answer</button>' : '') + '</section>';
}).join('');
</script></body></html>`;
}

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

  const frame = () => page.frameLocator('#artifact-viewer-frame');
  const inFrame = fn => page.frames().find(f => /\/render\?v=/.test(f.url())).evaluate(fn);
  const annotations = async () => (await api(page, 'GET', `/api/artifacts/${artifactId}/annotations?status=all`)).json;
  const pendingBar = () => page.locator('#artifact-viewer-pending');
  const openViewer = async () => {
    await page.evaluate(id => window.artifactViewer.open(id), artifactId);
    await page.waitForFunction(() => window.artifactViewer._bridgeReady && window.artifactViewer._questions.length > 0, null, { timeout: 15000 });
  };
  const card = id => page.locator(`#artifact-panel-answers .artifact-question[data-question-id="${id}"]`);

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });

    // --- a malformed questions block refuses the publish
    const malformed = await api(page, 'POST', '/api/artifacts', { title: 'E2E bad questions', html: artifact({ questions: [{ id: 'has space', prompt: 'P' }] }) });
    assert.equal(malformed.status, 400, malformed.text);
    assert.equal(malformed.json.code, 'invalid_artifact_questions');

    const created = await api(page, 'POST', '/api/artifacts', { title: 'E2E questions', html: artifact(questions), session_id: `e2e-questions-${Date.now()}` });
    assert.equal(created.status, 201, created.text);
    assert.equal(created.json.questions, 3);
    artifactId = created.json.id;

    await openViewer();
    // The artifact can tell it is being read inside OpenPoet.
    assert.equal(await inFrame(() => document.documentElement.hasAttribute('data-openpoet-host') && window.openpoet.available === true), true);
    assert.equal(await page.locator('#artifact-panel-tab-answers').evaluate(el => el.classList.contains('hidden')), false);
    assert.equal(await page.locator('#artifact-panel-count-answers').textContent(), '0/3');

    // --- what the artifact's own script cannot do
    await inFrame(() => {
      forge({ question: 'FORGED', option: 'a' });              // a question that was never declared
      forge({ question: 'D1', option: 'approve-everything' }); // an option that was never declared
      forge({ question: 'D1', option: ['a'] });                  // not a string
      forge({ question: { id: 'D1' }, option: 'a' });
      forge({ question: 'x'.repeat(5000), option: 'a' });
    });
    await page.waitForTimeout(300);
    assert.equal(await page.evaluate(() => window.artifactViewer._pending.size), 0, 'undeclared questions and options are ignored');
    assert.equal(await pendingBar().isHidden(), true);

    // --- a tap on a choice marks it; nothing is saved
    await frame().locator('[data-openpoet-question="D1"] [data-openpoet-option="b"] span').click();
    await pendingBar().waitFor({ state: 'visible' });
    assert.equal((await page.locator('#artifact-viewer-pending-text').textContent()).trim(), '1 answer to confirm');
    await page.waitForFunction(() => true);
    const marked = await inFrame(() => {
      const q = document.querySelector('[data-openpoet-question="D1"]');
      const b = q.querySelector('[data-openpoet-option="b"]');
      const a = q.querySelector('[data-openpoet-option="a"]');
      return { answered: q.getAttribute('data-openpoet-answered'), b: b.hasAttribute('data-openpoet-selected'), bAria: b.getAttribute('aria-pressed'), a: a.hasAttribute('data-openpoet-selected'), aAria: a.getAttribute('aria-pressed') };
    });
    assert.deepEqual(marked, { answered: 'pending', b: true, bAria: 'true', a: false, aAria: 'false' });
    assert.equal((await annotations()).length, 0, 'a tap in the frame stores nothing');

    // A declared choice proposed by the script is staged like a tap, and is
    // just as unsaved: it reaches the agent only if the user confirms it.
    await inFrame(() => window.openpoet.answer('D2', 'form'));
    await page.waitForFunction(() => window.artifactViewer._pending.size === 2);
    assert.equal((await annotations()).length, 0, 'a proposal from the script stores nothing');

    // --- review in the panel: the choices are there, typing happens there
    await page.locator('#artifact-viewer-pending-review').click();
    await card('D1').waitFor();
    assert.equal(await pendingBar().isHidden(), true, 'the bar gives way to the open panel');
    assert.equal(await card('D1').locator('input[value="b"]').isChecked(), true);
    assert.equal((await card('D1').locator('.artifact-question-state').textContent()).trim(), 'to confirm');
    assert.equal(await card('D2').locator('input[value="form"]').isChecked(), true);
    assert.equal(await card('D2').locator('textarea').count(), 0, 'a question declared without comment has no comment box');
    await card('D1').locator('textarea').fill(COMMENT);

    // A second option of the multiple choice, tapped in the frame while the
    // panel is open: the card follows without being rebuilt.
    await frame().locator('[data-openpoet-question="D2"] [data-openpoet-option="cron"]').click();
    await page.waitForFunction(() => document.querySelector('#artifact-panel-answers [data-question-id="D2"] input[value="cron"]')?.checked === true);
    assert.equal(await card('D1').locator('textarea').inputValue(), COMMENT, 'a tap in the frame keeps what was typed');

    // The free-text question: its button in the artifact leads to the panel.
    await frame().locator('[data-openpoet-question="NOTES"] [data-openpoet-answer]').click();
    await page.waitForFunction(() => document.activeElement?.closest?.('[data-question-id="NOTES"]') && document.activeElement.tagName === 'TEXTAREA');
    await page.keyboard.type(FREE_TEXT);
    assert.equal((await page.locator('#artifact-panel-confirm').textContent()).trim(), 'Confirm 3 answers');
    assert.equal((await annotations()).length, 0);

    // --- confirming saves them, as annotations
    await page.locator('#artifact-panel-confirm').click();
    await page.waitForFunction(() => window.artifactViewer._pending.size === 0 && window.artifactViewer._annotations.length === 3);
    let saved = await annotations();
    const byQuestion = Object.fromEntries(saved.map(a => [a.question_id, a]));
    assert.deepEqual(saved.map(a => a.kind), ['answer', 'answer', 'answer']);
    assert.deepEqual(byQuestion.D1.answer.options, [{ id: 'b', label: 'Automation' }]);
    assert.equal(byQuestion.D1.body, COMMENT);
    assert.equal(byQuestion.D1.quote, 'What do we call it?');
    assert.deepEqual(byQuestion.D2.answer.options.map(o => o.id), ['form', 'cron']);
    assert.equal(byQuestion.NOTES.body, FREE_TEXT);
    assert.equal(byQuestion.NOTES.created_by, 'user:local-ui');
    assert.equal((await card('D1').locator('.artifact-question-state').textContent()).trim(), 'answered');
    assert.equal(await page.locator('#artifact-panel-count-answers').textContent(), '3/3');
    assert.equal(await inFrame(() => document.querySelector('[data-openpoet-question="D1"]').getAttribute('data-openpoet-answered')), 'saved');

    // What the user typed never entered the frame.
    const seen = await inFrame(() => window.__seen.join('\n'));
    assert.ok(seen.includes('"answers"'), 'the frame is told which options are chosen');
    assert.ok(!seen.includes('e2e-comment-marker') && !seen.includes('e2e-text-marker'), 'comments and free text stay out of the frame');
    assert.ok((await inFrame(() => window.__events.length)) > 0, 'the artifact is told when answers change');

    // The answers are listed with the notes and counted as open.
    assert.equal((await page.locator('#artifact-viewer-open-count').textContent()).trim(), '3');
    await page.locator('#artifact-panel-tab-open').click();
    const listed = await page.locator('#artifact-panel-list .artifact-annotation').allTextContents();
    assert.equal(listed.length, 3);
    assert.ok(listed.some(text => text.includes('answer · D1') && text.includes('→ Automation') && text.includes(COMMENT)));

    // --- answering again replaces the answer
    await page.locator('#artifact-panel-tab-answers').click();
    await card('D1').locator('input[value="a"]').check();
    assert.equal((await card('D1').locator('.artifact-question-state').textContent()).trim(), 'to confirm');
    await page.locator('#artifact-panel-confirm').click();
    await page.waitForFunction(() => window.artifactViewer._pending.size === 0);
    saved = await annotations();
    assert.equal(saved.length, 3, 'the answer was replaced, not added');
    const replaced = saved.find(a => a.question_id === 'D1');
    assert.equal(replaced.id, byQuestion.D1.id);
    assert.deepEqual(replaced.answer.options, [{ id: 'a', label: 'Routine' }]);

    // --- a staged change survives a reload, and can be discarded
    await frame().locator('[data-openpoet-question="D1"] [data-openpoet-option="b"]').click();
    await page.waitForFunction(() => window.artifactViewer._pending.size === 1);
    await page.reload({ waitUntil: 'networkidle' });
    await openViewer();
    await page.waitForFunction(() => window.artifactViewer._pending.size === 1);
    await pendingBar().waitFor({ state: 'visible' });
    await page.locator('#artifact-viewer-pending-review').click();
    await card('D1').locator('[data-action="discard"]').click();
    assert.equal(await page.evaluate(() => window.artifactViewer._pending.size), 0);
    assert.equal(await card('D1').locator('input[value="a"]').isChecked(), true);

    // --- an answer can be withdrawn
    await card('NOTES').locator('[data-action="withdraw"]').click();
    await page.waitForFunction(() => window.artifactViewer._annotations.length === 2);
    assert.equal((await card('NOTES').locator('.artifact-question-state').textContent()).trim(), 'not answered');

    // --- in annotate mode a click annotates, it does not answer
    await page.locator('#artifact-viewer-annotate').click();
    await frame().locator('[data-openpoet-question="D2"] [data-openpoet-option="chat"]').click();
    await page.locator('#artifact-panel-composer').waitFor({ state: 'visible' });
    assert.equal(await page.evaluate(() => window.artifactViewer._pending.size), 0);
    await page.locator('#artifact-composer-cancel').click();
    await page.locator('#artifact-viewer-annotate').click();

    // --- a session cannot answer for the user
    const forgedBySession = await page.evaluate(async id => {
      const response = await fetch(`/api/artifacts/${id}/answers`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json', Authorization: 'Bearer opst1_not-a-real-token' },
        body: JSON.stringify({ answers: [{ question: 'D1', options: ['b'] }] }),
      });
      return response.status;
    }, artifactId);
    assert.notEqual(forgedBySession, 200);

    // --- a new version that no longer asks D2: its answer is kept and flagged
    const v2 = await api(page, 'PUT', `/api/artifacts/${artifactId}`, {
      html: artifact({ questions: [questions.questions[0], questions.questions[2]] }), base_version: 1,
    });
    assert.equal(v2.status, 200, v2.text);
    assert.equal(v2.json.questions, 2);
    // The open viewer is offered the new version; move to it.
    await page.locator('#artifact-viewer-banner-open').waitFor({ state: 'visible' });
    await page.locator('#artifact-viewer-banner-open').click();
    await page.waitForFunction(() => window.artifactViewer._bridgeReady && window.artifactViewer._questions.length === 2);
    assert.equal(await page.locator('#artifact-panel-count-answers').textContent(), '1/2');
    await page.locator('#artifact-panel-tab-open').click();
    const orphan = page.locator('#artifact-panel-list .artifact-annotation', { hasText: 'answer · D2' });
    assert.equal(await orphan.locator('.is-orphan').count(), 1);
    assert.equal(await page.locator('#artifact-panel-list .artifact-annotation', { hasText: 'answer · D1' }).locator('.is-orphan').count(), 0);
    // The chosen option is shown on the new version too.
    assert.equal(await inFrame(() => document.querySelector('[data-openpoet-question="D1"] [data-openpoet-option="a"]').hasAttribute('data-openpoet-selected')), true);

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact questions smoke: OK');
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
