// artifact_guard.js runs as the first script of every rendered artifact,
// before any agent-written markup or script. It removes the WebRTC
// peer-connection constructors so a hostile artifact cannot open an
// RTCPeerConnection to reach an external STUN/TURN host.
//
// Why this is needed on top of the CSP: the artifact CSP's `connect-src 'none'`
// does not govern WebRTC, and the `webrtc 'block'` directive is still ignored by
// Chromium (it logs "Unrecognized Content-Security-Policy directive"), so a
// peer connection could gather server-reflexive candidates and smuggle data to
// an arbitrary host. The artifact sandbox already blocks nested browsing
// contexts and workers (`frame-src`, `child-src` and `worker-src` are 'none'),
// so once these globals are gone there is no other context the artifact can run
// code in to get them back.
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
