// artifact_guard.js runs as the first script of every rendered artifact,
// before any agent-written markup or script. It removes the WebRTC
// peer-connection constructors so a hostile artifact cannot open an
// RTCPeerConnection to reach an external STUN/TURN host.
//
// Why this is needed on top of the CSP: the artifact CSP's `connect-src 'none'`
// does not govern WebRTC, and the `webrtc 'block'` directive is still ignored by
// Chromium (it logs "Unrecognized Content-Security-Policy directive"), so a
// peer connection could gather server-reflexive candidates and smuggle data to
// an arbitrary host.
//
// Removing the globals is only enough if the artifact has no other JavaScript
// realm to take them from. Workers are closed by the CSP (`worker-src 'none'`),
// but nested frames are not: `frame-src` and `child-src` govern URL loads, and
// an <iframe srcdoc> (or about:blank) frame is not a URL load. Such a frame runs
// its own scripts with a fresh copy of RTCPeerConnection. It is also out of
// reach of the annotation bridge, so whatever the artifact shows in it cannot be
// commented on. The second part of this script therefore blocks nested frames
// altogether (see "Nested frames" below); publishing a document that builds
// them is refused too (internal/application/artifact_frames.go).
(function () {
  "use strict";
  // Only the peer-connection constructors can open a connection; RTCDataChannel
  // has no public constructor (it comes from pc.createDataChannel) but is frozen
  // too for defence in depth. Media capture (getUserMedia) is already denied by
  // the render's Permissions-Policy.
  var names = [
    "RTCPeerConnection",
    "webkitRTCPeerConnection",
    "mozRTCPeerConnection",
    "RTCDataChannel",
  ];
  for (var i = 0; i < names.length; i++) {
    try {
      Object.defineProperty(window, names[i], {
        value: undefined,
        writable: false,
        configurable: false,
        enumerable: false,
      });
    } catch (e) {
      // A browser that refuses to redefine the property: fall back to a plain
      // assignment, which at least blanks it where the property is writable.
      try {
        window[names[i]] = undefined;
      } catch (e2) {
        /* nothing else to try */
      }
    }
  }
})();

// ---- Nested frames ----------------------------------------------------------
//
// An artifact is one document: the viewer does not let it open nested browsing
// contexts (<iframe>, <frame>, <object>, <embed>, <fencedframe>, <portal>). A
// frame element can come into existence only by being created by script, by
// being parsed from markup, or by being declared as a customized built-in, so
// those are the doors this closes:
//
//   - document.createElement / createElementNS of a frame tag throws, and so
//     does customElements.define with `extends` naming one;
//   - every way script parses markup (innerHTML, outerHTML, insertAdjacentHTML,
//     setHTMLUnsafe and setHTML, DOMParser, Range.createContextualFragment,
//     Document.parseHTMLUnsafe, XSLTProcessor) has the frames taken out of the
//     parsed nodes before they can enter the page: markup set on an element is
//     parsed into a detached element of the same kind, where nothing loads and
//     none of the artifact's code runs, cleaned, and only then moved in;
//   - document.write and writeln throw, and execCommand('insertHTML') does
//     nothing: they feed the page's parser, where a frame would be live before
//     anything could take it out;
//   - a MutationObserver removes any frame that still reaches the page, which
//     is how frames written in the stored markup itself (refused when
//     publishing, so only older versions have them) are removed before the
//     next of the artifact's scripts runs.
//
// The replaced methods and setters are defined non-writable and
// non-configurable, and everything this code calls is captured before the
// artifact runs, so the artifact can neither restore the originals nor
// interfere with the cleaning. The number of frames blocked is readable as
// window.__openpoetBlockedFrames, and each change is announced with an
// "openpoet:frames-blocked" event on window, so the annotation bridge can tell
// the viewer, which explains the gap to the user.
(function () {
  "use strict";
  var apply = Reflect.apply;
  var describe = Object.getOwnPropertyDescriptor;
  var define = Object.defineProperty;
  var HTML_NS = "http://www.w3.org/1999/xhtml";
  var FRAME_TAGS = { iframe: 1, frame: 1, frameset: 1, object: 1, embed: 1, fencedframe: 1, portal: 1 };
  var FRAME_SELECTOR = "iframe,frame,frameset,object,embed,fencedframe,portal";
  // Elements whose content is text, not markup: setting their innerHTML
  // cannot create elements, so it is left to the browser.
  var RAW_TEXT = { script: 1, style: 1, textarea: 1, title: 1, xmp: 1, noembed: 1, noframes: 1, noscript: 1, plaintext: 1 };
  var MESSAGE = "OpenPoet artifacts are one document: nested frames (iframe, srcdoc, object, embed) are blocked. " +
    "Put the content in this page as sections shown and hidden by the page's own script.";

  function getter(proto, name) {
    var d = proto && describe(proto, name);
    return d && d.get;
  }

  var NodeP = Node.prototype;
  var ElementP = Element.prototype;
  var DocumentP = Document.prototype;
  var FragmentP = DocumentFragment.prototype;
  var ShadowP = window.ShadowRoot ? ShadowRoot.prototype : null;

  var nodeTypeOf = getter(NodeP, "nodeType");
  var parentOf = getter(NodeP, "parentNode");
  var firstChildOf = getter(NodeP, "firstChild");
  var ownerDocumentOf = getter(NodeP, "ownerDocument");
  var localNameOf = getter(ElementP, "localName");
  var namespaceOf = getter(ElementP, "namespaceURI");
  var hostOf = getter(ShadowP, "host");
  var templateContentOf = window.HTMLTemplateElement ? getter(HTMLTemplateElement.prototype, "content") : null;
  var listLength = getter(NodeList.prototype, "length");
  var listItem = NodeList.prototype.item;
  var removeChild = NodeP.removeChild;
  var appendChild = NodeP.appendChild;
  var elementQuery = ElementP.querySelectorAll;
  var documentQuery = DocumentP.querySelectorAll;
  var fragmentQuery = FragmentP.querySelectorAll;
  var createElementNS = DocumentP.createElementNS;
  var createFragment = DocumentP.createDocumentFragment;
  var elementReplaceChildren = ElementP.replaceChildren;
  var fragmentReplaceChildren = FragmentP.replaceChildren;
  var elementAppend = ElementP.append;
  var elementPrepend = ElementP.prepend;
  var elementBefore = ElementP.before;
  var elementAfter = ElementP.after;
  var elementReplaceWith = ElementP.replaceWith;
  var innerHTML = describe(ElementP, "innerHTML");
  var DOMParserCtor = window.DOMParser;
  var parseFromString = DOMParserCtor ? DOMParserCtor.prototype.parseFromString : null;
  var stringOf = String;
  var lower = String.prototype.toLowerCase;
  var indexOf = String.prototype.indexOf;
  var slice = String.prototype.slice;
  var setTimer = window.setTimeout;
  var dispatch = EventTarget.prototype.dispatchEvent;
  var EventCtor = window.Event;
  var ErrorCtor = window.DOMException || Error;
  var warn = window.console && console.warn;
  var MutationObserverCtor = window.MutationObserver;
  var observe = MutationObserverCtor ? MutationObserverCtor.prototype.observe : null;
  var addedNodesOf = window.MutationRecord ? getter(MutationRecord.prototype, "addedNodes") : null;

  var blocked = 0;
  var announceQueued = false;
  var warned = false;
  try {
    define(window, "__openpoetBlockedFrames", {
      get: function () { return blocked; },
      configurable: false,
      enumerable: false,
    });
  } catch (e) { /* nothing to report through */ }

  function announce() {
    announceQueued = false;
    try { apply(dispatch, window, [new EventCtor("openpoet:frames-blocked")]); } catch (e) { /* ignore */ }
  }

  function count(n) {
    if (!n) return;
    blocked += n;
    if (!warned && warn) {
      warned = true;
      try { apply(warn, console, [MESSAGE]); } catch (e) { /* ignore */ }
    }
    if (!announceQueued) {
      announceQueued = true;
      try { apply(setTimer, window, [announce, 0]); } catch (e) { announceQueued = false; }
    }
  }

  function refuse() {
    count(1);
    throw new ErrorCtor(MESSAGE, "NotSupportedError");
  }

  function isFrameName(name) {
    return FRAME_TAGS[apply(lower, stringOf(name), [])] === 1;
  }

  // Removes every frame element from root (an element, document or fragment)
  // and from the contents of the templates inside it. Returns how many.
  function strip(root) {
    if (!root) return 0;
    var type = apply(nodeTypeOf, root, []);
    var query = type === 1 ? elementQuery : type === 9 ? documentQuery : type === 11 ? fragmentQuery : null;
    if (!query) return 0;
    var removed = 0;
    var frames = apply(query, root, [FRAME_SELECTOR]);
    var n = apply(listLength, frames, []);
    for (var i = 0; i < n; i++) {
      var frame = apply(listItem, frames, [i]);
      var parent = apply(parentOf, frame, []);
      if (parent) apply(removeChild, parent, [frame]);
    }
    count(n);
    removed += n;
    if (templateContentOf) {
      var templates = apply(query, root, ["template"]);
      var t = apply(listLength, templates, []);
      for (var j = 0; j < t; j++) removed += strip(apply(templateContentOf, apply(listItem, templates, [j]), []));
      if (type === 1 && isTemplate(root)) removed += strip(apply(templateContentOf, root, []));
    }
    return removed;
  }

  function isHTML(el) {
    return apply(namespaceOf, el, []) === HTML_NS;
  }

  function isTemplate(el) {
    return isHTML(el) && apply(localNameOf, el, []) === "template";
  }

  function isRawText(el) {
    return isHTML(el) && RAW_TEXT[apply(localNameOf, el, [])] === 1;
  }

  function markupOf(value) {
    return value === null || value === undefined ? "" : stringOf(value);
  }

  // Parses markup as innerHTML would in context (an element, or null for a
  // body-like context) and returns the nodes in a fragment, frames removed.
  // The holder is detached: frames in it never load and custom elements in it
  // are not connected, so none of the artifact's code runs before the frames
  // are gone.
  function parseClean(context, doc, markup) {
    var ns = HTML_NS;
    var name = "div";
    if (context) {
      ns = apply(namespaceOf, context, []);
      name = apply(localNameOf, context, []);
      // A custom element's constructor is the artifact's code; parse in a
      // plain element instead.
      if (ns === HTML_NS && (apply(indexOf, name, ["-"]) !== -1 || name === "template" || FRAME_TAGS[name] === 1)) name = "div";
      if (ns === null) { ns = HTML_NS; name = "div"; }
    }
    var holder = apply(createElementNS, doc, [ns, name]);
    apply(innerHTML.set, holder, [markup]);
    strip(holder);
    var fragment = apply(createFragment, doc, []);
    for (var child = apply(firstChildOf, holder, []); child; child = apply(firstChildOf, holder, [])) {
      apply(appendChild, fragment, [child]);
    }
    return fragment;
  }

  function documentOf(node) {
    return apply(ownerDocumentOf, node, []) || document;
  }

  // The element markup set on `parent` is parsed in: the parent itself when it
  // is an element, the shadow host in a shadow root, otherwise none.
  function contextOf(parent) {
    if (!parent) return null;
    var type = apply(nodeTypeOf, parent, []);
    if (type === 1) return parent;
    if (type === 11 && hostOf) {
      try { return apply(hostOf, parent, []); } catch (e) { return null; }
    }
    return null;
  }

  function lockMethod(proto, name, wrap) {
    var d = proto && describe(proto, name);
    if (!d || typeof d.value !== "function") return;
    try {
      define(proto, name, { value: wrap(d.value), writable: false, configurable: false, enumerable: d.enumerable });
    } catch (e) { /* already locked by the browser */ }
  }

  function lockSetter(proto, name, wrap) {
    var d = proto && describe(proto, name);
    if (!d || !d.set) return;
    try {
      define(proto, name, { get: d.get, set: wrap(d.set), configurable: false, enumerable: d.enumerable });
    } catch (e) { /* already locked by the browser */ }
  }

  // ---- creating frame elements

  lockMethod(DocumentP, "createElement", function (original) {
    return function (name) {
      if (isFrameName(name)) refuse();
      return apply(original, this, arguments);
    };
  });
  lockMethod(DocumentP, "createElementNS", function (original) {
    return function (namespace, qualifiedName) {
      if (namespace === HTML_NS) {
        var local = stringOf(qualifiedName);
        var colon = apply(indexOf, local, [":"]);
        if (isFrameName(colon === -1 ? local : apply(slice, local, [colon + 1]))) refuse();
      }
      return apply(original, this, arguments);
    };
  });
  if (window.CustomElementRegistry) {
    lockMethod(CustomElementRegistry.prototype, "define", function (original) {
      return function (name, constructor, options) {
        if (options && options.extends !== undefined && isFrameName(options.extends)) refuse();
        return apply(original, this, arguments);
      };
    });
  }

  // ---- parsing markup

  function setMarkup(original) {
    return function (value) {
      if (isRawText(this)) return apply(original, this, [value]);
      if (isTemplate(this)) {
        apply(original, this, [value]);
        strip(apply(templateContentOf, this, []));
        return;
      }
      apply(elementReplaceChildren, this, [parseClean(this, documentOf(this), markupOf(value))]);
    };
  }
  lockSetter(ElementP, "innerHTML", setMarkup);
  lockMethod(ElementP, "setHTMLUnsafe", function (original) {
    var set = setMarkup(original);
    return function (value) { return apply(set, this, [value]); };
  });

  function setShadowMarkup(original) {
    return function (value) {
      var host = null;
      try { host = apply(hostOf, this, []); } catch (e) { /* not a shadow root */ }
      if (!host) return apply(original, this, [value]);
      apply(fragmentReplaceChildren, this, [parseClean(host, documentOf(host), markupOf(value))]);
    };
  }
  if (ShadowP) {
    lockSetter(ShadowP, "innerHTML", setShadowMarkup);
    lockMethod(ShadowP, "setHTMLUnsafe", function (original) {
      var set = setShadowMarkup(original);
      return function (value) { return apply(set, this, [value]); };
    });
  }

  // setHTML sanitizes, but its default keeps or drops elements by a list this
  // code does not control: clean what it inserted, right away.
  lockMethod(ElementP, "setHTML", function (original) {
    return function () {
      var result = apply(original, this, arguments);
      strip(this);
      return result;
    };
  });
  if (ShadowP) {
    lockMethod(ShadowP, "setHTML", function (original) {
      return function () {
        var result = apply(original, this, arguments);
        strip(this);
        return result;
      };
    });
  }

  lockSetter(ElementP, "outerHTML", function (original) {
    return function (value) {
      var parent = apply(parentOf, this, []);
      // No parent, or the document itself: the browser throws or does nothing.
      if (!parent || apply(nodeTypeOf, parent, []) === 9) return apply(original, this, [value]);
      apply(elementReplaceWith, this, [parseClean(contextOf(parent), documentOf(this), markupOf(value))]);
    };
  });

  lockMethod(ElementP, "insertAdjacentHTML", function (original) {
    return function (position, value) {
      var where = apply(lower, stringOf(position), []);
      if (where === "beforebegin" || where === "afterend") {
        var parent = apply(parentOf, this, []);
        if (!parent || apply(nodeTypeOf, parent, []) === 9) return apply(original, this, arguments);
        var outside = parseClean(contextOf(parent), documentOf(this), markupOf(value));
        return apply(where === "beforebegin" ? elementBefore : elementAfter, this, [outside]);
      }
      if (where === "afterbegin" || where === "beforeend") {
        if (isRawText(this) || isTemplate(this)) {
          var result = apply(original, this, arguments);
          strip(this);
          return result;
        }
        var inside = parseClean(this, documentOf(this), markupOf(value));
        return apply(where === "afterbegin" ? elementPrepend : elementAppend, this, [inside]);
      }
      return apply(original, this, arguments); // the browser throws SyntaxError
    };
  });

  // Documents and fragments made from markup: frames in them never load (they
  // are not in the page), but they could be moved into it.
  if (DOMParserCtor) {
    lockMethod(DOMParserCtor.prototype, "parseFromString", function (original) {
      return function () {
        var doc = apply(original, this, arguments);
        strip(doc);
        return doc;
      };
    });
  }
  lockMethod(Range.prototype, "createContextualFragment", function (original) {
    return function () {
      var fragment = apply(original, this, arguments);
      strip(fragment);
      return fragment;
    };
  });
  if (typeof Document.parseHTMLUnsafe === "function") {
    lockMethod(Document, "parseHTMLUnsafe", function () {
      return function (value) {
        // DOMParser does not attach declarative shadow roots, so every node
        // of the result can be cleaned.
        var doc = apply(parseFromString, new DOMParserCtor(), [markupOf(value), "text/html"]);
        strip(doc);
        return doc;
      };
    });
  }
  if (window.XSLTProcessor) {
    lockMethod(XSLTProcessor.prototype, "transformToFragment", function (original) {
      return function () {
        var fragment = apply(original, this, arguments);
        strip(fragment);
        return fragment;
      };
    });
    lockMethod(XSLTProcessor.prototype, "transformToDocument", function (original) {
      return function () {
        var doc = apply(original, this, arguments);
        strip(doc);
        return doc;
      };
    });
  }

  // ---- writing into the page's parser

  function refuseWrite() {
    throw new ErrorCtor("OpenPoet artifacts cannot use document.write: build the page in its HTML or with DOM methods.", "NotSupportedError");
  }
  lockMethod(DocumentP, "write", function () { return refuseWrite; });
  lockMethod(DocumentP, "writeln", function () { return refuseWrite; });
  lockMethod(DocumentP, "execCommand", function (original) {
    return function (command) {
      if (apply(lower, stringOf(command), []) === "inserthtml") return false;
      return apply(original, this, arguments);
    };
  });

  // ---- whatever still reaches the page

  if (MutationObserverCtor && observe && addedNodesOf) {
    var observer = new MutationObserverCtor(function (records) {
      for (var i = 0; i < records.length; i++) {
        var added = apply(addedNodesOf, records[i], []);
        var n = apply(listLength, added, []);
        for (var j = 0; j < n; j++) {
          var node = apply(listItem, added, [j]);
          var type = apply(nodeTypeOf, node, []);
          if (type === 1 && isHTML(node) && FRAME_TAGS[apply(localNameOf, node, [])] === 1) {
            var parent = apply(parentOf, node, []);
            if (parent) apply(removeChild, parent, [node]);
            count(1);
          } else if (type === 1 || type === 11) {
            strip(node);
          }
        }
      }
    });
    apply(observe, observer, [document, { childList: true, subtree: true }]);
  }
})();
