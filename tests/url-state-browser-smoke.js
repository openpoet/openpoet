#!/usr/bin/env node

const assert = require('node:assert/strict');
const path = require('node:path');
const { chromium } = require('playwright');

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();

  try {
    await page.setContent(`<!doctype html>
      <html><body>
        <div id="doc-review-overlay" class="hidden">
          <span id="doc-review-name"></span>
          <button id="doc-review-download"></button>
          <button id="doc-review-download-pdf"></button>
          <button id="doc-review-close"></button>
          <div id="doc-review-content"></div>
          <div id="doc-review-footer" class="hidden"></div>
        </div>
      </body></html>`);

    await page.evaluate(() => {
      window.__routeNotifications = [];
      window.__pdfPrinted = false;
      window.app = {
        _onDocumentURLStateChanged(options) {
          window.__routeNotifications.push(options || {});
        },
      };

      const originalAppendChild = document.body.appendChild.bind(document.body);
      document.body.appendChild = element => {
        const result = originalAppendChild(element);
        if (element.tagName === 'IFRAME') {
          element.contentWindow.print = () => {
            window.__pdfPrinted = true;
            element.contentWindow.dispatchEvent(new Event('afterprint'));
          };
        }
        return result;
      };
    });

    await page.addScriptTag({ path: path.resolve('web/static/js/doc-viewer.js') });
    await page.evaluate(() => document.dispatchEvent(new Event('DOMContentLoaded')));

    const result = await page.evaluate(async () => {
      window.docViewer.openWithContent('Task 42', '# Task\n\nA rendered document.', {
        urlState: { kind: 'task', projectId: 7, taskId: 42 },
      });
      const firstRoute = window.docViewer.getURLState();

      window.docViewer.openWithContent('Verification', '## Result\n\nPassed.', {
        urlState: { kind: 'document', id: 'verification-1' },
      });
      const nestedRoute = window.docViewer.getURLState();
      window.docViewer.close();
      const restoredRoute = window.docViewer.getURLState();

      document.getElementById('doc-review-download-pdf').click();
      await new Promise(resolve => setTimeout(resolve, 50));

      return {
        firstRoute,
        nestedRoute,
        restoredRoute,
        pdfPrinted: window.__pdfPrinted,
        notificationCount: window.__routeNotifications.length,
        title: document.getElementById('doc-review-name').textContent,
      };
    });

    assert.deepEqual(result.firstRoute, { kind: 'task', projectId: 7, taskId: 42 });
    assert.deepEqual(result.nestedRoute, { kind: 'document', id: 'verification-1' });
    assert.deepEqual(result.restoredRoute, { kind: 'task', projectId: 7, taskId: 42 });
    assert.equal(result.title, 'Task 42');
    assert.equal(result.pdfPrinted, true, 'PDF action did not invoke the browser print/save flow');
    assert.equal(result.notificationCount, 3);

    console.log('URL state and PDF browser smoke passed');
  } finally {
    await browser.close();
  }
}

main().catch(error => {
  console.error(error);
  process.exit(1);
});
