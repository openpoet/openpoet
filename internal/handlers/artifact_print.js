// OpenPoet artifact print step.
//
// The server appends this script, instead of the annotation bridge, when an
// artifact is rendered to be printed (render?print=1). That response alone
// carries "allow-modals", which is what lets a sandboxed frame open the print
// dialog; the viewer loads it in a frame it throws away afterwards. The script
// waits for the page to finish loading (and for diagrams being drawn), opens
// the print dialog and tells the page when the dialog is gone.
(function () {
  'use strict';
  if (window.__openpoetArtifactPrint) return;
  window.__openpoetArtifactPrint = true;

  var done = false;
  function finish(outcome) {
    if (done) return;
    done = true;
    // The frame has an opaque origin and cannot name the page's origin; the
    // message carries nothing of the document.
    try { window.parent.postMessage({ source: 'openpoet-artifact-print', type: outcome }, '*'); } catch (error) { /* no page to tell */ }
  }

  function print() {
    window.addEventListener('afterprint', function () { finish('printed'); });
    try {
      window.print();
    } catch (error) {
      finish('failed');
    }
  }

  function start() {
    var rendering = window.__openpoetRendering;
    var ready = rendering && typeof rendering.then === 'function' ? rendering : Promise.resolve();
    // A moment after the last change, so the layout printed is the final one.
    // A timer, not requestAnimationFrame: the frame this runs in is never
    // painted, and a browser does not run animation frames for it.
    var go = function () { setTimeout(print, 100); };
    ready.then(go, go);
  }

  if (document.readyState === 'complete') start();
  else window.addEventListener('load', start);
})();
