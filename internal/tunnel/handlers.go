package tunnel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// HandlePairingPage generates a pending pairing session and displays the code.
// The remote device sees the code and waits for the local admin to enter it.
func (pm *PairingManager) HandlePairingPage(w http.ResponseWriter, r *http.Request) {
	code, sessionID, expirySecs, err := pm.CreatePendingPairing(r.UserAgent())
	if err != nil {
		log.Printf("[PAIRING] Failed to create pending pairing: %v", err)
		http.Error(w, "Failed to generate pairing code", http.StatusInternalServerError)
		return
	}

	log.Printf("[PAIRING] Pending session created: %s (code: %s..., expires in %ds)", sessionID[:8], code[:3], expirySecs)
	servePairingDisplayHTML(w, code, sessionID, expirySecs)
}

// HandlePairingStatus is the polling endpoint for remote devices waiting for approval.
func (pm *PairingManager) HandlePairingStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("id")
	if sessionID == "" {
		http.Error(w, `{"error":"missing session id"}`, http.StatusBadRequest)
		return
	}

	approved, sessionToken, encKey, err := pm.CheckPairingStatus(sessionID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if !approved {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"approved": false})
		return
	}

	// Approved — set session cookie on this response so the remote device is authenticated
	SetSessionCookie(w, sessionToken)

	// Pass encryption key via a short-lived cookie (read once by JS, then cleared)
	if encKey != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     "dm_enc_key",
			Value:    encKey,
			Path:     "/",
			MaxAge:   60,
			HttpOnly: false,
			SameSite: http.SameSiteLaxMode,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"approved": true})

	log.Printf("[PAIRING] Device paired via reversed flow, session: %s", sessionID[:8])
}

func servePairingDisplayHTML(w http.ResponseWriter, code string, sessionID string, expirySecs int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// Split code into individual digits for the display
	digits := make([]byte, len(code))
	copy(digits, code)

	html := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>DevManager - Pair Device</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
	font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
	background: #0d1117;
	color: #e6edf3;
	display: flex;
	justify-content: center;
	align-items: center;
	min-height: 100vh;
	padding: 20px;
}
.card {
	background: #161b22;
	border: 1px solid #30363d;
	border-radius: 12px;
	padding: 32px;
	max-width: 420px;
	width: 100%;
	text-align: center;
}
h1 {
	font-size: 1.4em;
	margin-bottom: 8px;
	color: #58a6ff;
}
p {
	color: #8b949e;
	margin-bottom: 24px;
	font-size: 0.9em;
	line-height: 1.5;
}
.code-display {
	display: flex;
	gap: 10px;
	justify-content: center;
	margin-bottom: 24px;
}
.code-digit {
	width: 52px;
	height: 64px;
	display: flex;
	align-items: center;
	justify-content: center;
	font-size: 32px;
	font-weight: bold;
	font-family: 'SF Mono', Monaco, 'Cascadia Code', monospace;
	background: #0d1117;
	border: 2px solid #30363d;
	border-radius: 10px;
	color: #e6edf3;
	user-select: all;
}
.status {
	display: flex;
	align-items: center;
	justify-content: center;
	gap: 8px;
	margin-bottom: 16px;
	font-size: 0.85em;
	color: #8b949e;
}
.status-dot {
	width: 8px;
	height: 8px;
	border-radius: 50%;
	background: #f0883e;
	animation: pulse 2s ease-in-out infinite;
}
.status-dot.approved {
	background: #3fb950;
	animation: none;
}
.status-dot.expired {
	background: #f85149;
	animation: none;
}
@keyframes pulse {
	0%, 100% { opacity: 1; }
	50% { opacity: 0.4; }
}
.timer {
	font-size: 0.8em;
	color: #484f58;
	margin-bottom: 8px;
}
.instructions {
	background: #0d1117;
	border: 1px solid #21262d;
	border-radius: 8px;
	padding: 16px;
	margin-top: 16px;
	text-align: left;
}
.instructions ol {
	margin: 0;
	padding-left: 20px;
	font-size: 0.82em;
	color: #8b949e;
	line-height: 1.8;
}
.logo { font-size: 2em; margin-bottom: 16px; }
</style>
</head>
<body>
<div class="card">
	<div class="logo">&#x1f517;</div>
	<h1>Pair This Device</h1>
	<p>To grant this device access to DevManager, enter the code below on your admin machine.</p>
	<div class="code-display" id="codeDisplay">
		<div class="code-digit">` + string(digits[0]) + `</div>
		<div class="code-digit">` + string(digits[1]) + `</div>
		<div class="code-digit">` + string(digits[2]) + `</div>
		<div class="code-digit">` + string(digits[3]) + `</div>
		<div class="code-digit">` + string(digits[4]) + `</div>
		<div class="code-digit">` + string(digits[5]) + `</div>
	</div>
	<div class="status" id="status">
		<div class="status-dot" id="statusDot"></div>
		<span id="statusText">Waiting for approval...</span>
	</div>
	<div class="timer" id="timer"></div>
	<div class="instructions">
		<ol>
			<li>On your computer, open <strong>DevManager</strong></li>
			<li>Go to <strong>Config &gt; Settings &gt; Remote Access</strong></li>
			<li>Click <strong>Pair New Device</strong> and type this code</li>
			<li>Once approved, this page will redirect automatically</li>
		</ol>
	</div>
</div>
<script>
const SESSION_ID = "` + sessionID + `";
const EXPIRY_SECONDS = ` + fmt.Sprintf("%d", expirySecs) + `;
let remaining = EXPIRY_SECONDS;
let pollTimer = null;
let countdownTimer = null;

function updateTimer() {
	remaining--;
	if (remaining <= 0) {
		clearInterval(countdownTimer);
		clearInterval(pollTimer);
		document.getElementById('timer').textContent = 'Code expired';
		document.getElementById('statusText').textContent = 'Code expired. Refresh to get a new code.';
		document.getElementById('statusDot').className = 'status-dot expired';
		return;
	}
	const m = Math.floor(remaining / 60);
	const s = remaining % 60;
	document.getElementById('timer').textContent = 'Expires in ' + m + ':' + String(s).padStart(2, '0');
}

async function pollStatus() {
	try {
		const res = await fetch('/pair/status?id=' + SESSION_ID);
		if (res.status === 410) {
			clearInterval(pollTimer);
			clearInterval(countdownTimer);
			document.getElementById('statusText').textContent = 'Session expired. Refresh to try again.';
			document.getElementById('statusDot').className = 'status-dot expired';
			return;
		}
		const data = await res.json();
		if (data.approved) {
			clearInterval(pollTimer);
			clearInterval(countdownTimer);
			document.getElementById('statusText').textContent = 'Paired! Redirecting...';
			document.getElementById('statusDot').className = 'status-dot approved';
			setTimeout(() => { window.location.href = '/'; }, 500);
		}
	} catch (e) {
		// Network error, keep polling
	}
}

updateTimer();
countdownTimer = setInterval(updateTimer, 1000);
pollTimer = setInterval(pollStatus, 3000);
// First poll immediately
pollStatus();
</script>
</body>
</html>`

	w.Write([]byte(html))
}
