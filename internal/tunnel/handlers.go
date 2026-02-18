package tunnel

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/skip2/go-qrcode"
)

// HandlePairingPage serves the pairing page or auto-validates a QR token.
func (pm *PairingManager) HandlePairingPage(w http.ResponseWriter, r *http.Request) {
	// Check if this is a QR code flow (token in query param)
	token := r.URL.Query().Get("token")
	if token != "" {
		valid, err := pm.ValidateQRToken(token)
		if err != nil || !valid {
			servePairingHTML(w, "Invalid or expired pairing link. Please try again.")
			return
		}

		// QR token valid — complete pairing
		deviceID, sessionToken, encKey, err := pm.CompletePairing(r.UserAgent())
		if err != nil {
			log.Printf("[PAIRING] Failed to complete QR pairing: %v", err)
			servePairingHTML(w, "Pairing failed. Please try again.")
			return
		}

		log.Printf("[PAIRING] Device paired via QR: %s", deviceID)
		SetSessionCookie(w, sessionToken)
		// Pass encryption key via a short-lived cookie (read once by JS, then cleared)
		if encKey != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     "dm_enc_key",
				Value:    encKey,
				Path:     "/",
				MaxAge:   60, // 1 minute - JS reads and clears it
				HttpOnly: false, // Must be readable by JavaScript
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// No token — show 6-digit code entry form
	servePairingHTML(w, "")
}

// HandlePairDevice validates a 6-digit code and completes pairing.
func (pm *PairingManager) HandlePairDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}

	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
	} else {
		r.ParseForm()
		body.Code = r.FormValue("code")
	}

	if body.Code == "" {
		respondPairError(w, "Please enter a pairing code.")
		return
	}

	if !pm.ValidateCode(body.Code) {
		respondPairError(w, "Invalid or expired code. Please try again.")
		return
	}

	deviceID, sessionToken, encKey, err := pm.CompletePairing(r.UserAgent())
	if err != nil {
		log.Printf("[PAIRING] Failed to complete code pairing: %v", err)
		respondPairError(w, "Pairing failed. Please try again.")
		return
	}

	log.Printf("[PAIRING] Device paired via code: %s", deviceID)
	SetSessionCookie(w, sessionToken)

	if r.Header.Get("Content-Type") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":        true,
			"device_id":      deviceID,
			"redirect":       "/",
			"encryption_key": encKey,
		})
	} else {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// HandleQRImage generates and serves a QR code PNG image.
func (pm *PairingManager) HandleQRImage(w http.ResponseWriter, r *http.Request) {
	tunnelURL := r.URL.Query().Get("url")
	if tunnelURL == "" {
		http.Error(w, "Missing url parameter", http.StatusBadRequest)
		return
	}

	token, err := pm.GenerateQRToken()
	if err != nil {
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	pairingURL := tunnelURL + "/pair?token=" + token

	png, err := qrcode.Encode(pairingURL, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "Failed to generate QR code", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func respondPairError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func servePairingHTML(w http.ResponseWriter, errorMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	errorDiv := ""
	if errorMsg != "" {
		errorDiv = `<div class="error">` + errorMsg + `</div>`
	}

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
	max-width: 400px;
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
.code-inputs {
	display: flex;
	gap: 8px;
	justify-content: center;
	margin-bottom: 24px;
}
.code-inputs input {
	width: 48px;
	height: 56px;
	text-align: center;
	font-size: 24px;
	font-weight: bold;
	background: #0d1117;
	border: 2px solid #30363d;
	border-radius: 8px;
	color: #e6edf3;
	outline: none;
	transition: border-color 0.2s;
}
.code-inputs input:focus {
	border-color: #58a6ff;
}
button {
	width: 100%;
	padding: 12px;
	font-size: 1em;
	font-weight: 600;
	background: #238636;
	color: #fff;
	border: none;
	border-radius: 8px;
	cursor: pointer;
	transition: background 0.2s;
}
button:hover { background: #2ea043; }
button:disabled { background: #21262d; color: #484f58; cursor: not-allowed; }
.error {
	background: #da36331a;
	border: 1px solid #da3633;
	color: #f85149;
	padding: 10px;
	border-radius: 6px;
	margin-bottom: 16px;
	font-size: 0.85em;
}
.spinner {
	display: none;
	width: 24px;
	height: 24px;
	border: 3px solid #30363d;
	border-top-color: #58a6ff;
	border-radius: 50%;
	animation: spin 0.8s linear infinite;
	margin: 0 auto 16px;
}
@keyframes spin { to { transform: rotate(360deg); } }
.logo { font-size: 2em; margin-bottom: 16px; }
</style>
</head>
<body>
<div class="card">
	<div class="logo">&#x1f4bb;</div>
	<h1>Pair Device</h1>
	<p>Enter the 6-digit code shown on your DevManager screen.</p>
	` + errorDiv + `
	<div id="spinner" class="spinner"></div>
	<form id="pairForm">
		<div class="code-inputs">
			<input type="text" maxlength="1" inputmode="numeric" pattern="[0-9]" autocomplete="off" autofocus>
			<input type="text" maxlength="1" inputmode="numeric" pattern="[0-9]" autocomplete="off">
			<input type="text" maxlength="1" inputmode="numeric" pattern="[0-9]" autocomplete="off">
			<input type="text" maxlength="1" inputmode="numeric" pattern="[0-9]" autocomplete="off">
			<input type="text" maxlength="1" inputmode="numeric" pattern="[0-9]" autocomplete="off">
			<input type="text" maxlength="1" inputmode="numeric" pattern="[0-9]" autocomplete="off">
		</div>
		<button type="submit" id="submitBtn" disabled>Pair Device</button>
	</form>
</div>
<script>
const inputs = document.querySelectorAll('.code-inputs input');
const form = document.getElementById('pairForm');
const btn = document.getElementById('submitBtn');
const spinner = document.getElementById('spinner');

inputs.forEach((input, i) => {
	input.addEventListener('input', (e) => {
		const val = e.target.value.replace(/[^0-9]/g, '');
		e.target.value = val;
		if (val && i < inputs.length - 1) inputs[i + 1].focus();
		checkComplete();
	});
	input.addEventListener('keydown', (e) => {
		if (e.key === 'Backspace' && !e.target.value && i > 0) {
			inputs[i - 1].focus();
		}
	});
	input.addEventListener('paste', (e) => {
		e.preventDefault();
		const text = (e.clipboardData || window.clipboardData).getData('text').replace(/[^0-9]/g, '');
		for (let j = 0; j < Math.min(text.length, inputs.length); j++) {
			inputs[j].value = text[j];
		}
		if (text.length >= inputs.length) inputs[inputs.length - 1].focus();
		checkComplete();
	});
});

function checkComplete() {
	const code = Array.from(inputs).map(i => i.value).join('');
	btn.disabled = code.length < 6;
}

form.addEventListener('submit', async (e) => {
	e.preventDefault();
	const code = Array.from(inputs).map(i => i.value).join('');
	if (code.length < 6) return;

	btn.disabled = true;
	spinner.style.display = 'block';

	try {
		const res = await fetch('/pair', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ code }),
		});
		const data = await res.json();
		if (data.success) {
			if (data.encryption_key) {
				localStorage.setItem('dm_encryption_key', data.encryption_key);
			}
			window.location.href = data.redirect || '/';
		} else {
			showError(data.error || 'Pairing failed');
		}
	} catch (err) {
		showError('Connection error. Please try again.');
	}
	btn.disabled = false;
	spinner.style.display = 'none';
});

function showError(msg) {
	let el = document.querySelector('.error');
	if (!el) {
		el = document.createElement('div');
		el.className = 'error';
		form.parentNode.insertBefore(el, form);
	}
	el.textContent = msg;
}
</script>
</body>
</html>`

	w.Write([]byte(html))
}
