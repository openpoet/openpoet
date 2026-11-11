// OpenPoet artifact annotation bridge.
//
// The server appends this script to an artifact when it serves it to the
// viewer; it is not part of the stored artifact. It runs inside the sandboxed
// frame (opaque origin, no network), next to the artifact's own scripts, and
// talks to the OpenPoet page only through postMessage:
//
//   frame -> page   ready | selection | annotation-click | anchors | key
//   page  -> frame  set-mode | show-annotations | focus | clear-selection
//
// It reports where the user selected text or clicked an element, and draws the
// highlights of saved annotations. It never receives annotation text: the note
// is typed and stored outside the frame. It does not modify the artifact's DOM
// either; highlights live in one shadow-rooted element appended to <html>.
(function () {
  'use strict';
  if (window.__openpoetArtifactBridge) return;
  window.__openpoetArtifactBridge = true;

  var HOST = 'openpoet-artifact-host';
  var BRIDGE = 'openpoet-artifact-bridge';
  var CONTEXT_CHARS = 48;
  var MAX_QUOTE = 2000;

  var annotate = false;
  var annotations = [];   // [{id, n, anchor, status}] from the page
  var resolved = {};      // id -> {rects: DOMRect[] in document coordinates, el}
  var layer = null;       // shadow root that holds the highlight boxes
  var hoverBox = null;
  var redrawQueued = false;
  var slowRedrawTimer = null;
  var lastAnchorReport = '';
  var lastSelectionReport = '';
  var selectionTimer = null;

  function post(type, payload) {
    var message = payload || {};
    message.source = BRIDGE;
    message.type = type;
    // The frame has an opaque origin and cannot name the page's origin; the
    // messages carry nothing but positions and text of this document.
    window.parent.postMessage(message, '*');
  }

  // ---- highlight layer -----------------------------------------------------

  function ensureLayer() {
    if (layer) return layer;
    var hostEl = document.createElement('openpoet-annotations');
    hostEl.setAttribute('aria-hidden', 'true');
    hostEl.style.cssText = 'position:absolute;top:0;left:0;width:0;height:0;overflow:visible;z-index:2147483647;pointer-events:none;';
    // The shadow root keeps the artifact's CSS from restyling the highlights.
    // It is not a security boundary: the artifact's scripts share this frame.
    layer = hostEl.attachShadow({ mode: 'open' });
    var style = document.createElement('style');
    style.textContent =
      '.box{position:absolute;box-sizing:border-box;pointer-events:none;border-radius:2px;}' +
      '.text{background:rgba(255,196,0,.32);border-bottom:2px solid rgba(214,146,0,.95);}' +
      '.element{outline:2px solid rgba(214,146,0,.95);outline-offset:2px;background:rgba(255,196,0,.12);}' +
      '.resolved.text{background:rgba(120,130,150,.22);border-bottom-color:rgba(120,130,150,.8);}' +
      '.resolved.element{outline-color:rgba(120,130,150,.8);background:rgba(120,130,150,.10);}' +
      '.flash{animation:op-flash 1.1s ease-out 1;}' +
      '@keyframes op-flash{0%{box-shadow:0 0 0 0 rgba(214,146,0,.9)}100%{box-shadow:0 0 0 14px rgba(214,146,0,0)}}' +
      '.badge{position:absolute;pointer-events:auto;cursor:pointer;min-width:18px;height:18px;padding:0 5px;' +
      'box-sizing:border-box;border-radius:9px;background:#b45309;color:#fff;font:600 11px/18px system-ui,sans-serif;' +
      'text-align:center;transform:translate(-50%,-110%);box-shadow:0 1px 3px rgba(0,0,0,.35);}' +
      '.badge.resolved{background:#64748b;}' +
      '.hover{position:absolute;box-sizing:border-box;pointer-events:none;outline:2px dashed rgba(37,99,235,.95);' +
      'outline-offset:1px;background:rgba(37,99,235,.08);border-radius:2px;}' +
      '@media (prefers-reduced-motion: reduce){.flash{animation:none;}}';
    layer.appendChild(style);
    document.documentElement.appendChild(hostEl);
    return layer;
  }

  function pageRect(rect) {
    return {
      left: rect.left + window.scrollX, top: rect.top + window.scrollY,
      width: rect.width, height: rect.height
    };
  }

  function addBox(root, className, rect) {
    var box = document.createElement('div');
    box.className = className;
    box.style.left = rect.left + 'px';
    box.style.top = rect.top + 'px';
    box.style.width = rect.width + 'px';
    box.style.height = rect.height + 'px';
    root.appendChild(box);
    return box;
  }

  function clearLayer() {
    if (!layer) return;
    var nodes = layer.querySelectorAll('.box,.badge,.hover');
    for (var i = 0; i < nodes.length; i++) nodes[i].remove();
    hoverBox = null;
  }

  // ---- locating anchors ----------------------------------------------------

  function isOwn(node) {
    return !!node && node.nodeType === 1 && node.tagName === 'OPENPOET-ANNOTATIONS';
  }

  function cssPath(element) {
    var parts = [];
    var el = element;
    while (el && el.nodeType === 1 && el !== document.documentElement && parts.length < 40) {
      if (el === document.body) { parts.unshift('body'); break; }
      var id = el.getAttribute('id');
      if (id && /^[A-Za-z][\w-]*$/.test(id) && document.querySelectorAll('#' + id).length === 1) {
        parts.unshift('#' + id);
        break;
      }
      var tag = el.tagName.toLowerCase();
      var index = 1;
      var sibling = el;
      while ((sibling = sibling.previousElementSibling)) {
        if (sibling.tagName === el.tagName) index++;
      }
      parts.unshift(tag + ':nth-of-type(' + index + ')');
      el = el.parentElement;
    }
    return parts.join(' > ');
  }

  function query(selector) {
    if (!selector) return null;
    try { return document.querySelector(selector); } catch (e) { return null; }
  }

  // Text nodes under root, with their offsets in the concatenated text.
  function textIndex(root) {
    var walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
      acceptNode: function (node) {
        var parent = node.parentElement;
        if (!parent) return NodeFilter.FILTER_REJECT;
        var tag = parent.tagName;
        if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'NOSCRIPT' || tag === 'TEMPLATE') return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_ACCEPT;
      }
    });
    var nodes = [];
    var text = '';
    var node;
    while ((node = walker.nextNode())) {
      nodes.push({ node: node, start: text.length });
      text += node.data;
    }
    return { nodes: nodes, text: text };
  }

  function rangeFromOffsets(index, start, end) {
    var range = document.createRange();
    var startSet = false;
    for (var i = 0; i < index.nodes.length; i++) {
      var entry = index.nodes[i];
      var nodeEnd = entry.start + entry.node.data.length;
      if (!startSet && start < nodeEnd) {
        range.setStart(entry.node, Math.max(0, start - entry.start));
        startSet = true;
      }
      if (startSet && end <= nodeEnd) {
        range.setEnd(entry.node, Math.max(0, end - entry.start));
        return range;
      }
    }
    return null;
  }

  // Finds a quote in text. With several matches, prefers the one whose
  // surrounding text matches the saved context, then the one nearest hint.
  function findQuote(text, anchor, hint) {
    var exact = anchor.exact;
    if (!exact) return -1;
    var best = -1;
    var bestScore = -1;
    var from = 0;
    var position;
    var guard = 0;
    while ((position = text.indexOf(exact, from)) !== -1 && guard++ < 500) {
      var score = 0;
      if (anchor.prefix && text.slice(Math.max(0, position - anchor.prefix.length), position) === anchor.prefix) score += 4;
      if (anchor.suffix && text.slice(position + exact.length, position + exact.length + anchor.suffix.length) === anchor.suffix) score += 4;
      if (typeof hint === 'number' && position === hint) score += 2;
      if (score > bestScore || (score === bestScore && typeof hint === 'number' && Math.abs(position - hint) < Math.abs(best - hint))) {
        best = position;
        bestScore = score;
      }
      from = position + Math.max(1, exact.length);
    }
    return best;
  }

  function locateText(anchor) {
    // First inside the element the selection was made in (same offsets when the
    // document did not change), then anywhere in the document: a new version
    // usually keeps the sentence but moves it.
    var scoped = query(anchor.selector);
    if (scoped && !isOwn(scoped)) {
      var index = textIndex(scoped);
      var at = findQuote(index.text, anchor, anchor.start);
      if (at !== -1) {
        var range = rangeFromOffsets(index, at, at + anchor.exact.length);
        if (range) return range;
      }
    }
    if (!document.body) return null;
    var whole = textIndex(document.body);
    var position = findQuote(whole.text, anchor);
    return position === -1 ? null : rangeFromOffsets(whole, position, position + anchor.exact.length);
  }

  function locate(anchor) {
    if (!anchor) return null;
    if (anchor.type === 'element') {
      var el = query(anchor.selector);
      if (!el || isOwn(el)) return null;
      if (anchor.tag && el.tagName.toLowerCase() !== anchor.tag) return null;
      return { element: el, rects: [el.getBoundingClientRect()] };
    }
    if (anchor.type === 'text') {
      var range = locateText(anchor);
      if (!range) return null;
      var rects = Array.prototype.slice.call(range.getClientRects()).filter(function (r) { return r.width > 0 && r.height > 0; });
      if (!rects.length) rects = [range.getBoundingClientRect()];
      var container = range.startContainer.nodeType === 1 ? range.startContainer : range.startContainer.parentElement;
      return { element: container, rects: rects };
    }
    return null;
  }

  // ---- drawing -------------------------------------------------------------

  function draw() {
    redrawQueued = false;
    clearLayer();
    resolved = {};
    if (!annotations.length && !annotate) return;
    var root = ensureLayer();
    var found = [];
    var orphaned = [];
    for (var i = 0; i < annotations.length; i++) {
      var annotation = annotations[i];
      var hit = locate(annotation.anchor);
      if (!hit) { orphaned.push(annotation.id); continue; }
      found.push(annotation.id);
      resolved[annotation.id] = hit;
      var kind = (annotation.anchor.type === 'element' ? 'element' : 'text') + (annotation.status === 'resolved' ? ' resolved' : '');
      var first = null;
      for (var j = 0; j < hit.rects.length; j++) {
        var rect = pageRect(hit.rects[j]);
        var box = addBox(root, 'box ' + kind, rect);
        box.setAttribute('data-id', String(annotation.id));
        if (!first) first = rect;
      }
      if (first) {
        var badge = document.createElement('div');
        badge.className = 'badge' + (annotation.status === 'resolved' ? ' resolved' : '');
        badge.textContent = String(annotation.n);
        badge.style.left = Math.max(12, first.left) + 'px';
        badge.style.top = Math.max(22, first.top) + 'px';
        badge.setAttribute('data-id', String(annotation.id));
        badge.addEventListener('click', onBadgeClick);
        root.appendChild(badge);
      }
    }
    // Report only changes: an animated artifact redraws many times a second.
    var report = found.join(',') + '|' + orphaned.join(',');
    if (report !== lastAnchorReport) {
      lastAnchorReport = report;
      post('anchors', { found: found, orphaned: orphaned });
    }
  }

  function queueDraw() {
    if (redrawQueued) return;
    redrawQueued = true;
    (window.requestAnimationFrame || setTimeout)(draw, 16);
  }

  // DOM mutations can arrive on every animation frame (charts, transitions);
  // following them a few times a second is enough and keeps the artifact fast.
  function queueSlowDraw() {
    if (slowRedrawTimer || (!annotations.length && !annotate)) return;
    slowRedrawTimer = setTimeout(function () {
      slowRedrawTimer = null;
      queueDraw();
    }, 250);
  }

  function onBadgeClick(event) {
    event.preventDefault();
    event.stopPropagation();
    var id = Number(event.currentTarget.getAttribute('data-id'));
    if (id) post('annotation-click', { id: id });
  }

  function focusAnnotation(id) {
    var hit = resolved[id];
    if (!hit) return;
    if (hit.element && hit.element.scrollIntoView) {
      hit.element.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'auto' });
    }
    // Scrolling moved everything; redraw, then flash this annotation's boxes.
    draw();
    if (!layer) return;
    var boxes = layer.querySelectorAll('.box[data-id="' + id + '"]');
    for (var i = 0; i < boxes.length; i++) boxes[i].classList.add('flash');
  }

  // ---- capturing a new anchor ----------------------------------------------

  function clip(value, limit) {
    return value.length > limit ? value.slice(0, limit) : value;
  }

  function viewportRect(rect) {
    return { left: rect.left, top: rect.top, width: rect.width, height: rect.height };
  }

  function reportSelection() {
    if (!annotate) return false;
    var selection = window.getSelection();
    if (!selection || selection.rangeCount === 0 || selection.isCollapsed) return false;
    var range = selection.getRangeAt(0);
    var exact = range.toString();
    if (!exact.trim()) return false;
    var container = range.commonAncestorContainer;
    if (container.nodeType !== 1) container = container.parentElement;
    if (!container || isOwn(container)) return false;

    // Offsets of the selection inside the container's text, to tell repeated
    // phrases apart when the highlight is drawn again.
    var index = textIndex(container);
    var start = -1;
    for (var i = 0; i < index.nodes.length; i++) {
      if (index.nodes[i].node === range.startContainer) { start = index.nodes[i].start + range.startOffset; break; }
    }
    var flat = index.text;
    if (start === -1 || flat.substr(start, exact.length) !== exact) start = flat.indexOf(exact);
    var anchor = { type: 'text', selector: cssPath(container), exact: clip(exact, MAX_QUOTE) };
    if (start !== -1) {
      anchor.start = start;
      anchor.end = start + anchor.exact.length;
      anchor.prefix = flat.slice(Math.max(0, start - CONTEXT_CHARS), start);
      anchor.suffix = flat.slice(start + exact.length, start + exact.length + CONTEXT_CHARS);
    }
    // The same selection is seen by mouseup, click and selectionchange.
    var signature = anchor.selector + '|' + anchor.start + '|' + anchor.exact;
    if (signature !== lastSelectionReport) {
      lastSelectionReport = signature;
      post('selection', { anchor: anchor, rect: viewportRect(range.getBoundingClientRect()) });
    }
    return true;
  }

  // Touch devices select text with handles, long after touchend.
  function onSelectionChange() {
    if (!annotate) return;
    clearTimeout(selectionTimer);
    selectionTimer = setTimeout(function () {
      var selection = window.getSelection();
      if (!selection || selection.isCollapsed) { lastSelectionReport = ''; return; }
      reportSelection();
    }, 450);
  }

  function elementLabel(el) {
    var label = el.getAttribute('aria-label') || el.getAttribute('alt') || el.getAttribute('title') || '';
    if (!label) label = (el.innerText || el.textContent || '').replace(/\s+/g, ' ').trim();
    return clip(label, 160);
  }

  function pickTarget(event) {
    var el = event.target;
    if (el && el.nodeType !== 1) el = el.parentElement;
    if (!el || isOwn(el) || el === document.documentElement || el === document.body) return null;
    return el;
  }

  function onClickCapture(event) {
    if (!annotate) return;
    var el = pickTarget(event);
    if (!el) return;
    // In annotate mode a click annotates; it must not trigger the artifact's
    // own links, buttons or handlers.
    event.preventDefault();
    event.stopPropagation();
    if (event.stopImmediatePropagation) event.stopImmediatePropagation();
    var selection = window.getSelection();
    if (selection && !selection.isCollapsed && selection.toString().trim()) {
      reportSelection(); // a text selection ending in this click wins
      return;
    }
    lastSelectionReport = '';
    post('selection', {
      anchor: { type: 'element', selector: cssPath(el), tag: el.tagName.toLowerCase(), label: elementLabel(el) },
      rect: viewportRect(el.getBoundingClientRect())
    });
  }

  function onMouseUp() {
    if (annotate) setTimeout(reportSelection, 0);
  }

  function onPointerMove(event) {
    if (!annotate) return;
    var el = pickTarget(event);
    var root = ensureLayer();
    if (hoverBox) { hoverBox.remove(); hoverBox = null; }
    if (!el) return;
    var selection = window.getSelection();
    if (selection && !selection.isCollapsed) return; // selecting text, not picking
    hoverBox = addBox(root, 'hover', pageRect(el.getBoundingClientRect()));
  }

  function setMode(enabled) {
    annotate = !!enabled;
    document.documentElement.style.cursor = annotate ? 'crosshair' : '';
    if (!annotate && hoverBox) { hoverBox.remove(); hoverBox = null; }
    queueDraw();
  }

  // ---- messages from the page ----------------------------------------------

  window.addEventListener('message', function (event) {
    // Only the embedding page is listened to. An artifact opened directly in a
    // tab has no parent, so nothing can drive the bridge there.
    if (event.source !== window.parent || window.parent === window) return;
    var data = event.data;
    if (!data || data.source !== HOST) return;
    switch (data.type) {
      case 'set-mode':
        setMode(data.annotate);
        break;
      case 'show-annotations':
        annotations = Array.isArray(data.annotations) ? data.annotations.slice(0, 500) : [];
        lastAnchorReport = ''; // always answer a new list, even with the same result
        queueDraw();
        break;
      case 'focus':
        focusAnnotation(Number(data.id));
        break;
      case 'clear-selection':
        var selection = window.getSelection();
        if (selection) selection.removeAllRanges();
        lastSelectionReport = '';
        break;
    }
  });

  document.addEventListener('click', onClickCapture, true);
  document.addEventListener('mouseup', onMouseUp, true);
  document.addEventListener('touchend', onMouseUp, true);
  document.addEventListener('selectionchange', onSelectionChange);
  document.addEventListener('mousemove', onPointerMove, true);
  document.addEventListener('keydown', function (event) {
    // Keys pressed inside the frame never reach the page; forward Escape so it
    // can cancel a note or close the viewer.
    if (event.key === 'Escape') post('key', { key: 'Escape' });
  }, true);

  // Highlights are positioned boxes, so they follow layout changes.
  window.addEventListener('resize', queueDraw);
  document.addEventListener('scroll', queueDraw, true);
  window.addEventListener('load', queueDraw);
  if (window.MutationObserver) {
    new MutationObserver(function (records) {
      for (var i = 0; i < records.length; i++) {
        if (!isOwn(records[i].target)) { queueSlowDraw(); return; }
      }
    }).observe(document.documentElement, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['class', 'style', 'hidden', 'open'] });
  }
  if (window.ResizeObserver && document.body) {
    new ResizeObserver(queueSlowDraw).observe(document.body);
  }

  post('ready', {});
})();
