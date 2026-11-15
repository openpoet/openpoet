// OpenPoet artifact mermaid step.
//
// The server adds this script, after the mermaid library, to a markdown
// artifact that has a ```mermaid block. It is not part of the stored artifact
// and runs inside the sandboxed frame, with no network. It replaces each
// rendered <pre><code class="language-mermaid"> by the diagram mermaid draws
// from that text. A block mermaid cannot parse keeps its source as text.
(function () {
  'use strict';
  var blocks = document.querySelectorAll('pre > code.language-mermaid');
  if (!blocks.length || !window.mermaid) return;
  var nodes = [];
  for (var i = 0; i < blocks.length; i++) {
    var pre = blocks[i].parentNode;
    var figure = document.createElement('div');
    figure.className = 'mermaid';
    figure.textContent = blocks[i].textContent;
    pre.parentNode.replaceChild(figure, pre);
    nodes.push(figure);
  }
  // A page being printed is drawn for paper, whatever the theme of the screen.
  var printing = document.documentElement.hasAttribute('data-openpoet-print');
  var dark = !printing && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  try {
    window.mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: dark ? 'dark' : 'default' });
    // The print step waits for this before it prints.
    window.__openpoetRendering = window.mermaid.run({ nodes: nodes, suppressErrors: true });
  } catch (error) {
    // The source stays on the page as text.
  }
})();
