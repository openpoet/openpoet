#!/usr/bin/env node

// voice.js cuts recordings into segments at pauses, transcribes each one in
// order with the first segment's language pinned, and uploads them as small base64
// slices (the public proxy rejects bodies over 1 MiB), resuming instead of
// losing audio. This drives the real voice.js against a stubbed fetch: a
// transient 5xx on a slice, a slice the server reports missing at completion
// (409), then success, and checks the transcript is the segments joined.

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
      window.__completes = [];
      let chunkZeroFailures = 1;
      let completions = 0;
      window.fetch = async (url, options = {}) => {
        const body = options.body || '';
        window.__requests.push({ url, method: options.method, bytes: body.length });
        const json = (status, payload) => new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
        const chunk = url.match(/uploads\/([^/]+)\/chunks\/(\d+)$/);
        if (chunk) {
          const [, id, index] = chunk;
          if (id.endsWith('-s0') && index === '0' && chunkZeroFailures-- > 0) return new Response('<html>502</html>', { status: 502 });
          (window.__slices[id] ||= {})[index] = JSON.parse(body).data;
          return json(200, { index: +index });
        }
        const done = url.match(/uploads\/([^/]+)\/complete$/);
        if (done) {
          const id = done[1];
          window.__completes.push({ id, ...JSON.parse(body) });
          if (id.endsWith('-s0') && ++completions === 1) {
            delete window.__slices[id]['1']; // server lost a slice (e.g. restart)
            return json(409, { error: 'missing', code: 'voice_upload_incomplete', missing: [1] });
          }
          return json(200, id.endsWith('-s0')
            ? { text: ' primeira parte, longa o bastante. ', language: 'pt' }
            : { text: 'segunda parte.', language: 'en' });
        }
        return json(404, { error: 'unexpected ' + url });
      };
    });

    await page.addScriptTag({ path: path.resolve('web/static/js/voice.js') });

    const result = await page.evaluate(async () => {
      const v = window.voiceInput;
      const original = new Uint8Array(700 * 1024).map((_, i) => (i * 31 + 7) & 0xff);
      let delivered = null;
      const target = text => { delivered = text; };
      v.job = {
        id: 'rec-unit-test-0001',
        mimeType: 'audio/webm;codecs=opus',
        closed: true,
        submit: false,
        target,
        segments: [
          { index: 0, blob: new Blob([original], { type: 'audio/webm' }), text: null, seconds: 20, uploadedSlices: new Set() },
          { index: 1, blob: new Blob([new Uint8Array(1000)], { type: 'audio/webm' }), text: null, seconds: 5, uploadedSlices: new Set() },
        ],
      };
      await v.pump(v.job);
      await new Promise(r => setTimeout(r, 50));

      const decode = b64 => Uint8Array.from(atob(b64), c => c.charCodeAt(0));
      const s0 = window.__slices['rec-unit-test-0001-s0'];
      const parts = Object.keys(s0).sort((a, b) => a - b).map(k => decode(s0[k]));
      const joined = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
      let offset = 0;
      for (const p of parts) { joined.set(p, offset); offset += p.length; }
      return {
        delivered,
        jobCleared: v.job === null,
        sliceCount: parts.length,
        identical: joined.length === original.length && joined.every((b, i) => b === original[i]),
        maxBytes: Math.max(...window.__requests.map(r => r.bytes)),
        chunkRequests: window.__requests.filter(r => /\/chunks\//.test(r.url)).map(r => r.url.split('/uploads/')[1].replace('rec-unit-test-0001-', '')),
        completes: window.__completes.map(c => ({ id: c.id.replace('rec-unit-test-0001-', ''), language: c.language ?? null, prompt: c.prompt ?? null, chunks: c.chunks, seconds: c.seconds })),
      };
    });

    assert.equal(result.delivered, 'primeira parte, longa o bastante. segunda parte.');
    assert.equal(result.jobCleared, true);
    assert.equal(result.sliceCount, 3);
    assert.equal(result.identical, true, 'slices must reassemble to the original bytes');
    assert.ok(result.maxBytes < 1024 * 1024, `request body ${result.maxBytes} B must stay under the 1 MiB proxy limit`);
    // s0 slice 0 fails once (502) and is retried; slice 1 is re-sent after the 409.
    assert.deepEqual(result.chunkRequests, ['s0/chunks/0', 's0/chunks/0', 's0/chunks/1', 's0/chunks/2', 's0/chunks/1', 's1/chunks/0']);
    // Segments go in order; the language detected on the first is pinned on
    // the next, and no previous-text prompt is sent (Whisper parrots it).
    assert.deepEqual(result.completes, [
      { id: 's0', language: null, prompt: null, chunks: 3, seconds: 20 },
      { id: 's0', language: null, prompt: null, chunks: 3, seconds: 20 },
      { id: 's1', language: 'pt', prompt: null, chunks: 1, seconds: 5 },
    ]);

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
