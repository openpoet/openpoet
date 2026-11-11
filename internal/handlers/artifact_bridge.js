// OpenPoet artifact annotation bridge.
//
// The server appends this script to an artifact when it serves it to the
// viewer; it is not part of the stored artifact. It runs inside the sandboxed
// frame (opaque origin, no network), next to the artifact's own scripts, and
// talks to the OpenPoet page only through postMessage:
//
//   frame -> page   ready | selection | annotation-click | anchors | key |
//                   answer-intent
//   page  -> frame  set-mode | show-annotations | focus | clear-selection |
//                   answers | focus-question
//
// It reports where the user selected text or clicked an element, and draws the
// highlights of saved annotations. It never receives annotation text: the note
// is typed and stored outside the frame. Highlights live in one shadow-rooted
// element appended to <html>, not in the artifact's DOM.
//
// Questions: an artifact marks an element with data-openpoet-question="<id>"
// and the choices inside it with data-openpoet-option="<id>" (the questions
// themselves are declared in the document's openpoet-questions data block,
// which the server reads). A click on a choice tells the page which one; the
// page decides what that means, shows it to the user and stores it only when
// the user confirms there. The page then tells the frame which options are
// chosen, and the bridge reflects that on the marked elements as
// data-openpoet-selected / data-openpoet-answered and the matching ARIA state:
// the only attributes it ever sets on the artifact's elements. Comments and
// free-text answers never enter the frame.
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
  var answers = {};       // question id -> {options: [ids], state} from the page
  var hostSeen = false;   // the embedding page has spoken to the bridge
  var applyTimer = null;

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

  // A value of id or data-id that can be written into a selector as it is.
  var MARK = /^[\w.:-]{1,80}$/;

  function dataIdSelector(value) {
    return '[data-id="' + value + '"]';
  }

  // The closest id or data-id at or above an element, as "#stage-2" or
  // [data-id="TM8"], or '' when there is none. A page made of tabs or panels
  // usually marks each one that way: the mark says which panel an annotation
  // belongs to, for the agent that reads it and for placing it again.
  function nearMark(element) {
    var el = element;
    for (var depth = 0; el && el.nodeType === 1 && el !== document.body && el !== document.documentElement && depth < 60; depth++) {
      var id = el.getAttribute('id');
      if (id && /^[A-Za-z]/.test(id) && MARK.test(id)) return '#' + id;
      var mark = el.getAttribute('data-id');
      if (mark && MARK.test(mark)) return dataIdSelector(mark);
      el = el.parentElement;
    }
    return '';
  }

  // The element a mark names, or null. Only the two forms nearMark writes are
  // understood; anything else in a stored anchor is ignored.
  function nearElement(near) {
    if (typeof near !== 'string' || !near) return null;
    var el = null;
    if (near.charAt(0) === '#' && MARK.test(near.slice(1))) {
      el = document.getElementById(near.slice(1));
    } else {
      var match = /^\[data-id="([\w.:-]{1,80})"\]$/.exec(near);
      if (match) el = query(dataIdSelector(match[1]));
    }
    return el && !isOwn(el) ? el : null;
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
      // A unique data-id anchors the path as an id does. Panels that are
      // mounted one at a time into the same container differ only by it.
      var mark = el.getAttribute('data-id');
      if (mark && MARK.test(mark) && document.querySelectorAll(dataIdSelector(mark)).length === 1) {
        parts.unshift(dataIdSelector(mark));
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
    // Then inside the panel the annotation was made in, when it is in the
    // page: the same sentence in another panel is not the annotated one.
    var near = nearElement(anchor.near);
    if (near && near !== scoped) {
      var panel = textIndex(near);
      var inPanel = findQuote(panel.text, anchor);
      if (inPanel !== -1) {
        var panelRange = rangeFromOffsets(panel, inPanel, inPanel + anchor.exact.length);
        if (panelRange) return panelRange;
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
      // An element anchor that names its panel is placed only inside it. A
      // page that mounts one tab at a time puts different content at the same
      // position, and the annotation belongs to one of them.
      if (anchor.near) {
        var scope = nearElement(anchor.near);
        if (!scope || !scope.contains(el)) return null;
      }
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
    var hidden = [];
    for (var i = 0; i < annotations.length; i++) {
      var annotation = annotations[i];
      var hit = locate(annotation.anchor);
      if (!hit) { orphaned.push(annotation.id); continue; }
      found.push(annotation.id);
      resolved[annotation.id] = hit;
      // In the page but not rendered (a closed tab, a collapsed section):
      // there is nothing to outline, and a box of no size would pile every
      // such badge in the corner of the page.
      var rendered = false;
      for (var k = 0; k < hit.rects.length; k++) {
        if (hit.rects[k].width > 0 || hit.rects[k].height > 0) { rendered = true; break; }
      }
      if (!rendered) { hidden.push(annotation.id); continue; }
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
    var report = found.join(',') + '|' + orphaned.join(',') + '|' + hidden.join(',');
    if (report !== lastAnchorReport) {
      lastAnchorReport = report;
      post('anchors', { found: found, orphaned: orphaned, hidden: hidden });
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
    var textNear = nearMark(container);
    if (textNear) anchor.near = textNear;
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
    var picked = { type: 'element', selector: cssPath(el), tag: el.tagName.toLowerCase(), label: elementLabel(el) };
    var elementNear = nearMark(el);
    if (elementNear) picked.near = elementNear;
    post('selection', { anchor: picked, rect: viewportRect(el.getBoundingClientRect()) });
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

  // ---- questions ------------------------------------------------------------

  var MAX_ID = 64;

  function attr(el, name, value) {
    if (value === null) {
      if (el.hasAttribute(name)) el.removeAttribute(name);
    } else if (el.getAttribute(name) !== value) {
      el.setAttribute(name, value);
    }
  }

  function closestMarked(el, name) {
    return el && el.closest ? el.closest('[' + name + ']') : null;
  }

  // Tells the page which choice was clicked. The page checks it against the
  // questions the server stored for this version; nothing is saved here.
  function onAnswerClick(event) {
    if (annotate || event.isTrusted === false) return; // annotate mode annotates
    var el = event.target;
    if (el && el.nodeType !== 1) el = el.parentElement;
    var option = closestMarked(el, 'data-openpoet-option');
    var holder = option || closestMarked(el, 'data-openpoet-answer');
    var question = closestMarked(holder, 'data-openpoet-question');
    if (!question) return;
    var message = { question: clip(question.getAttribute('data-openpoet-question') || '', MAX_ID) };
    if (!message.question) return;
    if (option) message.option = clip(option.getAttribute('data-openpoet-option') || '', MAX_ID);
    post('answer-intent', message);
  }

  // Reflects the chosen options on the elements the artifact marked.
  function applyAnswers() {
    applyTimer = null;
    var questions = document.querySelectorAll('[data-openpoet-question]');
    for (var i = 0; i < questions.length; i++) {
      var question = questions[i];
      var answer = answers[question.getAttribute('data-openpoet-question')];
      attr(question, 'data-openpoet-answered', answer ? answer.state : null);
      var options = question.querySelectorAll('[data-openpoet-option]');
      for (var j = 0; j < options.length; j++) {
        var option = options[j];
        if (closestMarked(option.parentElement, 'data-openpoet-question') !== question) continue; // a nested question's
        var chosen = !!answer && answer.options.indexOf(option.getAttribute('data-openpoet-option')) !== -1;
        attr(option, 'data-openpoet-selected', chosen ? '' : null);
        if (option.tagName === 'INPUT' && (option.type === 'radio' || option.type === 'checkbox')) {
          if (option.checked !== chosen) option.checked = chosen;
          continue;
        }
        var role = option.getAttribute('role');
        var state = role === 'radio' || role === 'checkbox' || role === 'switch' || role === 'menuitemradio' || role === 'menuitemcheckbox'
          ? 'aria-checked' : (role === 'option' || role === 'tab' ? 'aria-selected' : 'aria-pressed');
        attr(option, state, chosen ? 'true' : 'false');
      }
    }
  }

  function queueApplyAnswers() {
    if (applyTimer || !hostSeen) return;
    applyTimer = setTimeout(applyAnswers, 60);
  }

  function copyAnswers() {
    var copy = {};
    for (var id in answers) {
      if (Object.prototype.hasOwnProperty.call(answers, id)) copy[id] = { options: answers[id].options.slice(), state: answers[id].state };
    }
    return copy;
  }

  function announce(name, detail) {
    try { document.dispatchEvent(new CustomEvent(name, { detail: detail })); } catch (e) { /* old engines */ }
  }

  function setAnswers(list) {
    answers = {};
    if (Array.isArray(list)) {
      for (var i = 0; i < list.length && i < 500; i++) {
        var item = list[i];
        if (!item || typeof item.question !== 'string' || !Array.isArray(item.options)) continue;
        answers[item.question] = {
          options: item.options.filter(function (o) { return typeof o === 'string'; }).slice(0, 100),
          state: item.state === 'saved' ? 'saved' : 'pending'
        };
      }
    }
    applyAnswers();
    announce('openpoet:answers', { answers: copyAnswers() });
  }

  function focusQuestion(id) {
    var questions = document.querySelectorAll('[data-openpoet-question]');
    for (var i = 0; i < questions.length; i++) {
      if (questions[i].getAttribute('data-openpoet-question') === id) {
        questions[i].scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'auto' });
        return;
      }
    }
  }

  // What an artifact's own script may call. It is a convenience over the
  // marked elements, not a privilege: it sends the same message a click does.
  var api = {
    available: false,
    answer: function (question, option) {
      post('answer-intent', { question: clip(String(question), MAX_ID), option: clip(String(option), MAX_ID) });
    },
    ask: function (question) {
      post('answer-intent', { question: clip(String(question), MAX_ID) });
    }
  };
  try {
    Object.defineProperty(api, 'answers', { enumerable: true, get: copyAnswers });
    if (!window.openpoet) window.openpoet = api;
  } catch (e) { /* the artifact defined its own */ }

  function sawHost() {
    if (hostSeen) return;
    hostSeen = true;
    api.available = true;
    document.documentElement.setAttribute('data-openpoet-host', '');
    announce('openpoet:ready', {});
  }

  // ---- messages from the page ----------------------------------------------

  window.addEventListener('message', function (event) {
    // Only the embedding page is listened to. An artifact opened directly in a
    // tab has no parent, so nothing can drive the bridge there.
    if (event.source !== window.parent || window.parent === window) return;
    var data = event.data;
    if (!data || data.source !== HOST) return;
    sawHost();
    switch (data.type) {
      case 'answers':
        setAnswers(data.answers);
        break;
      case 'focus-question':
        if (typeof data.question === 'string') focusQuestion(data.question);
        break;
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
  document.addEventListener('click', onAnswerClick, true);
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
        // The artifact redrew something: highlights and chosen options follow.
        if (!isOwn(records[i].target)) { queueSlowDraw(); queueApplyAnswers(); return; }
      }
    }).observe(document.documentElement, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['class', 'style', 'hidden', 'open'] });
  }
  if (window.ResizeObserver && document.body) {
    new ResizeObserver(queueSlowDraw).observe(document.body);
  }

  post('ready', {});
})();
