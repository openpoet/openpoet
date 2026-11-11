package tunnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeNextPath(t *testing.T) {
	for _, next := range []string{
		"/",
		"/app/artifact/ab12cd34",
		"/app/artifact/ab12cd34?v=2",
		"/?view=projects&doc=artifact%3Aab12cd34",
		"/app/task/4/12",
	} {
		if got := SafeNextPath(next); got != next {
			t.Errorf("SafeNextPath(%q) = %q, want it kept", next, got)
		}
	}
	for _, next := range []string{
		"",
		"app/artifact/x",                // not a path from the root
		"//evil.example/app",            // scheme-relative: another site
		"/\\evil.example",               // browsers read \ as /
		"/app\\evil",                    // backslash anywhere
		"https://evil.example/",         // absolute
		"javascript:alert(1)",           // not a path
		"/app/\nSet-Cookie: x=1",        // control character
		"/pair",                         // the pairing page is not a destination
		"/pair/status?id=x",             //
		"/" + strings.Repeat("a", 2100), // too long
	} {
		if got := SafeNextPath(next); got != "" {
			t.Errorf("SafeNextPath(%q) = %q, want it refused", next, got)
		}
	}
}

func TestRedirectToPairingKeepsTheDestination(t *testing.T) {
	location := func(method, target string) (int, string) {
		recorder := httptest.NewRecorder()
		redirectToPairing(recorder, httptest.NewRequest(method, target, nil))
		return recorder.Code, recorder.Header().Get("Location")
	}
	if code, where := location(http.MethodGet, "/app/artifact/ab12cd34?v=2"); code != http.StatusTemporaryRedirect || where != "/pair?next=%2Fapp%2Fartifact%2Fab12cd34%3Fv%3D2" {
		t.Fatalf("deep link redirect = %d %q", code, where)
	}
	if _, where := location(http.MethodGet, "/"); where != "/pair" {
		t.Fatalf("home redirect = %q, want plain /pair", where)
	}
	// Only a page load has a destination worth returning to.
	if _, where := location(http.MethodPost, "/app/artifact/ab12cd34"); where != "/pair" {
		t.Fatalf("POST redirect = %q, want plain /pair", where)
	}
	// API calls are answered, not redirected.
	recorder := httptest.NewRecorder()
	redirectToPairing(recorder, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("Location") != "" {
		t.Fatalf("API request = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestPairingPageSendsTheDeviceToItsDestination(t *testing.T) {
	page := func(next string) string {
		recorder := httptest.NewRecorder()
		servePairingDisplayHTML(recorder, "123456", "session-id-1", 120, SafeNextPath(next))
		return recorder.Body.String()
	}
	if body := page("/app/artifact/ab12cd34?v=2"); !strings.Contains(body, `const NEXT = "/app/artifact/ab12cd34?v=2";`) || !strings.Contains(body, "window.location.href = NEXT") {
		t.Fatalf("pairing page does not carry the destination")
	}
	// Without a destination, or with one that leaves the site, it is the home page.
	for _, next := range []string{"", "//evil.example/", "https://evil.example/"} {
		if body := page(next); !strings.Contains(body, `const NEXT = "/";`) {
			t.Fatalf("pairing page for next=%q does not fall back to the home page", next)
		}
	}
	// A destination cannot break out of the script it is embedded in.
	body := page(`/app/x?q=</script><script>alert(1)</script>`)
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, `</script>`) {
		t.Fatalf("destination was embedded unescaped")
	}
}
