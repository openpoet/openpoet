#!/usr/bin/env node

// Pinch-zoom in the artifact viewer on a phone, against a running dev instance.
//
// The app locks page zoom in its viewport meta. An artifact is shown in a frame
// of that page, so the lock used to reach it too and an image in an artifact
// could not be enlarged. The viewer lifts the lock while it is open: this test
// pinches with two real touch points in an emulated phone and reads the page
// scale the browser reports.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

const LOCKED = /maximum-scale=1(\.0)?\b.*user-scalable=no/;

function artifactHTML() {
  const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="1600" height="900">' +
    '<rect width="1600" height="900" fill="#1f3a5f"/>' +
    '<text x="40" y="120" font-size="80" fill="#fff">detail image</text>' +
    '<text x="40" y="860" font-size="14" fill="#fff">small print 0123456789</text></svg>';
  const src = 'data:image/svg+xml;base64,' + Buffer.from(svg).toString('base64');
  return `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>zoom</title>
<style>body{font-family:system-ui;margin:1rem}img{max-width:100%;height:auto}</style></head>
<body><h1 id="label">zoom probe</h1><p>Text before the image.</p>
<img id="pic" alt="detail" src="${src}"><p style="height:1200px">Tall text after the image.</p></body></html>`;
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

const pageScale = page => page.evaluate(() => window.visualViewport.scale);
const viewportMeta = page => page.evaluate(() => document.querySelector('meta[name="viewport"]').getAttribute('content'));

// Two fingers moving apart around (x, y), as a phone reports them.
async function pinchOut(cdp, page, x, y) {
  const points = spread => [{ x: x - spread, y, id: 0 }, { x: x + spread, y, id: 1 }];
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: points(40) });
  for (let spread = 45; spread <= 100; spread += 5) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: points(spread) });
    await page.waitForTimeout(16);
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await page.waitForTimeout(300);
}

// One finger dragging from (x, y) by (dx, dy).
async function drag(cdp, page, x, y, dx, dy) {
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y, id: 0 }] });
  for (let step = 1; step <= 10; step++) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: x + dx * step / 10, y: y + dy * step / 10, id: 0 }] });
    await page.waitForTimeout(16);
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await page.waitForTimeout(300);
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: { width: 412, height: 915 }, deviceScaleFactor: 2.625, isMobile: true, hasTouch: true,
    userAgent: 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36',
  });
  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', err => pageErrors.push(err.message));
  let artifactId = '';

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });
    const cdp = await context.newCDPSession(page);
    const center = { x: 206, y: 480 };

    // --- outside the viewer the app keeps its zoom lock
    assert.match(await viewportMeta(page), LOCKED, 'the app must ship with page zoom locked');
    await pinchOut(cdp, page, center.x, center.y);
    assert.equal(await pageScale(page), 1, 'a pinch must not zoom the app itself');

    const created = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E zoom probe', html: artifactHTML(), session_id: `e2e-artifact-zoom-${Date.now()}`,
    });
    assert.equal(created.status, 201, created.text);
    artifactId = created.json.id;

    // --- with an artifact open, a pinch inside the frame zooms it
    await page.evaluate(id => window.artifactViewer.open(id), artifactId);
    const frame = page.frameLocator('#artifact-viewer-frame');
    await frame.locator('#pic').waitFor();
    assert.doesNotMatch(await viewportMeta(page), /user-scalable=no|maximum-scale=1(\.0)?\b/, 'the open viewer must lift the zoom lock');
    assert.match(await viewportMeta(page), /width=device-width/, 'the rest of the viewport value must be kept');

    const stage = await page.locator('#artifact-viewer-frame').boundingBox();
    assert.ok(center.y > stage.y && center.y < stage.y + stage.height, 'the pinch must land inside the artifact frame');
    await pinchOut(cdp, page, center.x, center.y);
    const zoomed = await pageScale(page);
    assert.ok(zoomed > 1.5, `a pinch inside the artifact must zoom in (scale ${zoomed})`);

    // Zoomed in, one finger still reaches everything: a sideways drag moves
    // the enlarged view, and a vertical one scrolls the artifact under it.
    // Touch positions are measured from the corner of what is on screen, in
    // page units, so the middle of the screen is half the visible size.
    const visible = await page.evaluate(() => ({ x: window.visualViewport.width / 2, y: window.visualViewport.height / 2 }));
    const offsetBefore = await page.evaluate(() => window.visualViewport.offsetLeft);
    await drag(cdp, page, visible.x, visible.y, -30, 0);
    const offsetAfter = await page.evaluate(() => window.visualViewport.offsetLeft);
    assert.ok(offsetAfter - offsetBefore > 10, `a sideways drag must pan the zoomed view (offset ${offsetBefore} -> ${offsetAfter})`);
    const frameScrollY = () => frame.locator('body').evaluate(() => window.scrollY);
    assert.equal(await frameScrollY(), 0);
    await drag(cdp, page, visible.x, visible.y, 0, -60);
    assert.ok(await frameScrollY() > 20, `a vertical drag must still scroll the artifact while zoomed (scrollY ${await frameScrollY()})`);

    // --- typing in one of the viewer's own fields brings the lock back
    await page.locator('#artifact-viewer-version').focus();
    await page.waitForTimeout(300);
    assert.match(await viewportMeta(page), LOCKED, 'a focused viewer field must restore the lock');
    assert.equal(await pageScale(page), 1, 'restoring the lock must return the page to its normal scale');
    await page.locator('#artifact-viewer-version').blur();
    await page.waitForTimeout(300);
    assert.doesNotMatch(await viewportMeta(page), /user-scalable=no/, 'zoom must be allowed again once the field loses focus');
    await pinchOut(cdp, page, center.x, center.y);
    assert.ok(await pageScale(page) > 1.5, 'zoom must work again after the field loses focus');

    // --- closing the viewer restores the app as it was
    await page.evaluate(() => window.artifactViewer.close());
    await page.waitForTimeout(300);
    assert.match(await viewportMeta(page), LOCKED, 'closing the viewer must restore the lock');
    assert.equal(await pageScale(page), 1, 'closing a zoomed viewer must return the page to its normal scale');
    await pinchOut(cdp, page, center.x, center.y);
    assert.equal(await pageScale(page), 1, 'a pinch must not zoom the app after the viewer closed');

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join('; ')}`);
    console.log('artifact viewer zoom smoke: OK');
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
