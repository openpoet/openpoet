#!/usr/bin/env node

// Nested frames in artifacts, against a running dev instance.
//
// An artifact is one document. Content in an <iframe srcdoc> (or any nested
// frame) is out of reach of the annotation bridge, and a nested frame is a
// fresh JavaScript realm with its own RTCPeerConnection, which the render
// guard removes from the page. So:
//   1. publishing HTML that builds frames is refused, naming the line;
//   2. a page that gets past that check (here, by spelling the tag names in
//      pieces) still cannot open a frame by any of the ways script has, nor
//      reach a WebRTC constructor, and the viewer says content was blocked;
//   3. a page written as one document (tabs that show and hide sections) is
//      annotated normally, with no notice.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

// Each attempt mounts a frame whose document would report back to the page.
// The tag names are assembled at run time so the publish check does not see
// them: this is what a page that tries to get around it would do.
const HOSTILE = `<!doctype html><html><head><meta charset="utf-8"><title>Frames</title></head>
<body>
<h1 id="title">Frame attempts</h1>
<div id="mount"></div>
<table><tbody id="rows"></tbody></table>
<svg id="chart" width="40" height="40"></svg>
<div id="editable" contenteditable="true"></div>
<script>
  var T = ['ifr', 'ame'].join('');
  var O = ['obj', 'ect'].join('');
  var HTML_NS = 'http://www.w3.org/1999/xhtml';
  var END = '</' + 'script>';
  var SRC = ['src', 'doc'].join('');
  var leaks = [];
  var outcomes = {};
  window.addEventListener('message', function (event) {
    if (event.data && event.data.kind === 'leak') leaks.push(event.data);
  });
  function inner(label) {
    return '<p>inner</p><script>parent.postMessage({kind:"leak",label:"' + label + '",rtc:typeof RTCPeerConnection},"*")' + END;
  }
  function frameMarkup(label) {
    return '<' + T + ' srcdoc="' + inner(label).replace(/"/g, '&quot;') + '"></' + T + '>';
  }
  function attempt(label, fn) {
    try { fn(); outcomes[label] = 'ran'; } catch (e) { outcomes[label] = (e && e.name) || 'error'; }
  }
  var mount = document.getElementById('mount');

  attempt('createElement', function () {
    var f = document.createElement(T); f[SRC] = inner('createElement'); mount.appendChild(f);
  });
  attempt('createElementNS', function () {
    var f = document.createElementNS(HTML_NS, T.toUpperCase()); f[SRC] = inner('createElementNS'); mount.append(f);
  });
  attempt('customizedBuiltIn', function () {
    customElements.define('x-frame', class extends HTMLElement {}, { extends: T });
  });
  attempt('innerHTML', function () { var d = document.createElement('div'); mount.append(d); d.innerHTML = frameMarkup('innerHTML'); });
  attempt('innerHTMLObject', function () { var d = document.createElement('div'); mount.append(d); d.innerHTML = '<' + O + ' data="x"></' + O + '>'; });
  attempt('insertAdjacentHTML', function () { mount.insertAdjacentHTML('beforeend', frameMarkup('insertAdjacentHTML')); });
  attempt('insertAdjacentHTMLOutside', function () { mount.insertAdjacentHTML('afterend', frameMarkup('insertAdjacentHTMLOutside')); });
  attempt('outerHTML', function () { var d = document.createElement('div'); mount.append(d); d.outerHTML = frameMarkup('outerHTML'); });
  attempt('setHTMLUnsafe', function () { var d = document.createElement('div'); mount.append(d); d.setHTMLUnsafe(frameMarkup('setHTMLUnsafe')); });
  attempt('DOMParser', function () {
    var doc = new DOMParser().parseFromString('<body>' + frameMarkup('DOMParser') + '</body>', 'text/html');
    mount.append.apply(mount, Array.from(doc.body.childNodes));
  });
  attempt('contextualFragment', function () {
    var range = document.createRange(); range.selectNodeContents(mount);
    mount.appendChild(range.createContextualFragment(frameMarkup('contextualFragment')));
  });
  attempt('template', function () {
    var t = document.createElement('template'); t.innerHTML = frameMarkup('template');
    mount.appendChild(t.content.cloneNode(true));
  });
  attempt('shadowRoot', function () {
    var host = document.createElement('div'); mount.append(host);
    host['attach' + 'Shadow']({ mode: 'closed' }).innerHTML = frameMarkup('shadowRoot');
  });
  attempt('write', function () { document['wri' + 'te'](frameMarkup('write')); });
  attempt('execCommand', function () {
    var ed = document.getElementById('editable'); ed.focus();
    document.execCommand('insertHTML', false, frameMarkup('execCommand'));
  });
  attempt('restoreCreateElement', function () {
    Object.defineProperty(Document.prototype, 'createElement', { value: function () { return null; } });
  });
  attempt('restoreInnerHTML', function () {
    Object.defineProperty(Element.prototype, 'innerHTML', { set: function () {} });
  });

  // Ordinary markup still works, in its own parsing context.
  document.getElementById('rows').innerHTML = '<tr><td id="cell">cell</td></tr>';
  document.getElementById('chart').innerHTML = '<circle id="dot" r="5" cx="10" cy="10"></circle>';
  mount.insertAdjacentHTML('afterbegin', '<p id="fine">Fine paragraph.</p>');
  window.__report = function () {
    return {
      leaks: leaks,
      outcomes: outcomes,
      frames: document.querySelectorAll('iframe,frame,object,embed').length,
      blocked: window.__openpoetBlockedFrames,
      rtc: typeof RTCPeerConnection,
      cell: !!document.getElementById('cell') && document.getElementById('cell').parentNode.localName,
      dot: !!document.getElementById('dot') && document.getElementById('dot').namespaceURI,
      fine: !!document.getElementById('fine')
    };
  };
</script>
</body></html>`;

const ONE_DOCUMENT = `<!doctype html><html><head><meta charset="utf-8"><title>Tabs</title>
<style>body{font-family:system-ui;margin:2rem}[hidden]{display:none}#lp-2 h2{color:#0a7}</style></head>
<body>
<nav><button data-tab="lp-1">Page 1</button><button data-tab="lp-2">Page 2</button></nav>
<section id="lp-1"><h2>First proposal</h2><p>The first page talks about tasks.</p></section>
<section id="lp-2" hidden><h2>Second proposal</h2><p>The second page talks about access.</p>
<img alt="dot" src="data:image/gif;base64,R0lGODlhAQABAAAAACw="></section>
<script>
  document.querySelectorAll('[data-tab]').forEach(function (b) {
    b.addEventListener('click', function () {
      document.querySelectorAll('section').forEach(function (s) { s.hidden = s.id !== b.dataset.tab; });
    });
  });
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

async function renderFrame(page) {
  await page.waitForFunction(() => document.querySelector('#artifact-viewer-frame')?.getAttribute('src')?.includes('/render'));
  for (let i = 0; i < 50; i++) {
    const frame = page.frames().find(f => f.url().includes('/render'));
    if (frame) return frame;
    await page.waitForTimeout(100);
  }
  throw new Error('render frame not found');
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 860 } });
  const pageErrors = [];
  page.on('pageerror', err => pageErrors.push(err.message));
  const created = [];

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });

    // --- 1. publishing frames is refused, with the line and what to do
    const refused = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E framed', session_id: `e2e-frames-${Date.now()}`,
      html: '<!doctype html><html><body>\n<p>x</p>\n<iframe srcdoc="<p>y</p>"></iframe>\n</body></html>',
    });
    assert.equal(refused.status, 400, refused.text);
    assert.equal(refused.json.code, 'artifact_nested_frames', refused.text);
    assert.match(refused.json.error, /line 3: <iframe> element/);
    assert.match(refused.json.error, /one document/);

    // --- 2. a page that gets past the check still opens no frame
    const hostile = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E frame attempts', html: HOSTILE, session_id: `e2e-frames-${Date.now()}`,
    });
    assert.equal(hostile.status, 201, hostile.text);
    created.push(hostile.json.id);
    await page.evaluate(id => window.artifactViewer.open(id), hostile.json.id);
    const frame = await renderFrame(page);
    await frame.locator('#title').waitFor();
    await page.waitForTimeout(1500); // time for any frame that got through to load and report
    const report = await frame.evaluate(() => window.__report());

    assert.deepEqual(report.leaks, [], `a nested frame ran: ${JSON.stringify(report.leaks)}`);
    assert.equal(report.frames, 0, 'a frame element is in the page');
    assert.equal(page.frames().filter(f => f.parentFrame() === frame).length, 0, 'the render frame has child frames');
    assert.equal(report.rtc, 'undefined', 'RTCPeerConnection is reachable in the page');
    for (const label of ['createElement', 'createElementNS', 'customizedBuiltIn', 'write']) {
      assert.equal(report.outcomes[label], 'NotSupportedError', `${label}: ${report.outcomes[label]}`);
    }
    for (const label of ['restoreCreateElement', 'restoreInnerHTML']) {
      assert.equal(report.outcomes[label], 'TypeError', `${label} was not refused: ${report.outcomes[label]}`);
    }
    // 3 refused creations + 10 frames taken out of parsed markup (execCommand
    // inserts nothing: insertHTML is refused).
    assert.equal(report.blocked, 13, `outcomes = ${JSON.stringify(report.outcomes)}`);
    // Ordinary markup keeps its parsing context.
    assert.equal(report.cell, 'tr');
    assert.equal(report.dot, 'http://www.w3.org/2000/svg');
    assert.equal(report.fine, true);

    const notice = page.locator('#artifact-viewer-frames');
    await notice.waitFor({ state: 'visible' });
    assert.match(await notice.textContent(), /nested frames, which OpenPoet blocks/);

    // --- 3. one document: annotated normally, no notice
    const clean = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E one document', html: ONE_DOCUMENT, session_id: `e2e-frames-${Date.now()}`,
    });
    assert.equal(clean.status, 201, clean.text);
    created.push(clean.json.id);
    await page.evaluate(id => window.artifactViewer.open(id), clean.json.id);
    const cleanFrame = await renderFrame(page);
    await cleanFrame.locator('#lp-1 p').waitFor();
    assert.equal(await notice.isVisible(), false, 'the frames notice is shown for a page without frames');

    await cleanFrame.locator('[data-tab="lp-2"]').click();
    await cleanFrame.locator('#lp-2 p').waitFor();
    await page.locator('#artifact-viewer-annotate').click();
    await page.locator('#artifact-viewer-panel:not(.hidden)').waitFor();
    await cleanFrame.locator('#lp-2 p').evaluate(el => {
      const range = document.createRange();
      range.selectNodeContents(el);
      getSelection().removeAllRanges();
      getSelection().addRange(range);
      el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    await page.locator('#artifact-panel-composer:not(.hidden)').waitFor();
    await page.locator('#artifact-composer-text').fill('Say which access');
    await page.locator('#artifact-composer-save').click();
    await page.waitForFunction(() => document.querySelectorAll('#artifact-panel-list .artifact-annotation').length === 1);
    const open = await api(page, 'GET', `/api/artifacts/${clean.json.id}/annotations?status=open`);
    assert.deepEqual(open.json.map(a => [a.anchor.type, a.quote, a.anchor.near]), [
      ['text', 'The second page talks about access.', '#lp-2'],
    ]);

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact frame block smoke: OK');
  } finally {
    for (const id of created) {
      await api(page, 'PATCH', `/api/artifacts/${id}`, { status: 'archived' }).catch(() => {});
    }
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
