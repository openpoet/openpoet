#!/usr/bin/env node

// voice.js uploads recordings as small base64 slices (the public proxy rejects
// bodies over 1 MiB) and resumes instead of losing audio. This drives the real
// voice.js against a stubbed fetch: a transient 5xx on a slice, a slice the
// server reports missing at completion (409), then success.

const assert = require('node:assert/strict');
const path = require('node:path');
const { chromium } = require('playwright');

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();

  try {
    await page.goto('about:blank');
    await page.setContent(`<!doctype html><html><body>
      <div id="voice-indicator" class="voice-indicator hidden">
        <span id="voice-status-label"></span><span id="voice-timer"></span>
        <button id="voice-cancel"></button><button id="voice-stop-btn"></button><button id="voice-send"></button>
        <button id="voice-retry"></button><button id="voice-discard"></button>
      </div></body></html>`);

    await page.evaluate(() => {
      window.app = { showToast() {} };
      window.__requests = [];
      window.__slices = {};
      let chunkZeroFailures = 1;
      let completions = 0;
      window.fetch = async (url, options = {}) => {
        const body = options.body || '';
        window.__requests.push({ url, method: options.method, bytes: body.length });
        const json = (status, payload) => new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
        const chunk = url.match(/\/chunks\/(\d+)$/);
        if (chunk) {
          if (chunk[1] === '0' && chunkZeroFailures-- > 0) return new Response('<html>502</html>', { status: 502 });
          window.__slices[chunk[1]] = JSON.parse(body).data;
          return json(200, { index: +chunk[1] });
        }
        if (url.endsWith('/complete')) {
          completions++;
          if (completions === 1) {
            delete window.__slices['1']; // server lost a slice (e.g. restart)
            return json(409, { error: 'missing', code: 'voice_upload_incomplete', missing: [1] });
          }
          return json(200, { text: 'transcrição completa' });
        }
        return json(404, { error: 'unexpected ' + url });
      };
    });

    await page.addScriptTag({ path: path.resolve('web/static/js/voice.js') });

    const result = await page.evaluate(async () => {
      const v = window.voiceInput;
      const original = new Uint8Array(700 * 1024).map((_, i) => (i * 31 + 7) & 0xff);
      let delivered = null;
      v.pendingAudioBlob = new Blob([original], { type: 'audio/webm;codecs=opus' });
      v.pendingRecordingId = 'rec-unit-test-0001';
      v.pendingMimeType = v.pendingAudioBlob.type;
      v.pendingTargetCallback = text => { delivered = text; };
      v.uploadedSlices = new Set();
      await v.uploadWithRetry();

      const decode = b64 => Uint8Array.from(atob(b64), c => c.charCodeAt(0));
      const parts = Object.keys(window.__slices).sort((a, b) => a - b).map(k => decode(window.__slices[k]));
      const joined = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
      let offset = 0;
      for (const p of parts) { joined.set(p, offset); offset += p.length; }
      const complete = window.__requests.filter(r => r.url.endsWith('/complete'));
      return {
        delivered,
        pendingCleared: v.pendingAudioBlob === null,
        sliceCount: parts.length,
        identical: joined.length === original.length && joined.every((b, i) => b === original[i]),
        maxBytes: Math.max(...window.__requests.map(r => r.bytes)),
        chunkRequests: window.__requests.filter(r => /\/chunks\//.test(r.url)).map(r => r.url.split('/').pop()),
        completeCount: complete.length,
        filename: v.recordingFilename(v.pendingMimeType || 'audio/webm'),
      };
    });

    assert.equal(result.delivered, 'transcrição completa');
    assert.equal(result.pendingCleared, true);
    assert.equal(result.sliceCount, 3);
    assert.equal(result.identical, true, 'slices must reassemble to the original bytes');
    assert.ok(result.maxBytes < 1024 * 1024, `request body ${result.maxBytes} B must stay under the 1 MiB proxy limit`);
    // 0 fails once (502) and is retried; 1 is re-sent after the 409.
    assert.deepEqual(result.chunkRequests, ['0', '0', '1', '2', '1']);
    assert.equal(result.completeCount, 2);

    const mime = await page.evaluate(() => ['audio/mp4', 'audio/ogg;codecs=opus', 'audio/webm'].map(t => window.voiceInput.recordingFilename(t)));
    assert.deepEqual(mime, ['recording.mp4', 'recording.ogg', 'recording.webm']);

    console.log('voice chunked upload smoke: ok');
  } finally {
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
