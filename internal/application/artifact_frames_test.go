package application

import (
	"strings"
	"testing"
)

func framesDoc(body string) string {
	return "<!doctype html>\n<html><head><title>t</title></head><body>\n" + body + "\n</body></html>"
}

func TestArtifactFramesRefusesNestedFrames(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"iframe element", `<iframe title="x"></iframe>`, "line 3: <iframe> element"},
		{"upper-case iframe", `<IFRAME></IFRAME>`, "<iframe> element"},
		{"object element", `<object data="x"></object>`, "<object> element"},
		{"embed element", `<embed src="x">`, "<embed> element"},
		{"frameset", `<frameset><frame src="x"></frameset>`, "<frameset> element"},
		{"srcdoc attribute", `<div srcdoc="<p>x</p>"></div>`, "srcdoc attribute"},
		{"declarative shadow root", `<div><template shadowrootmode="open"><p>x</p></template></div>`, "declarative shadow root"},
		// The pattern of the artifact that started this: pages kept as data and
		// mounted in srcdoc frames by script.
		{"createElement iframe", "<script>\nconst f = document.createElement('iframe');\nf.srcdoc = html;\n</script>", "line 4: createElement('iframe')"},
		{"srcdoc property", "<script>\nframe.srcdoc = '<p>x</p>';\n</script>", "line 4: srcdoc assigned in script"},
		{"setAttribute srcdoc", `<script>frame.setAttribute("srcdoc", html)</script>`, "srcdoc set in script"},
		{"createElementNS", `<script>document.createElementNS(ns, "embed")</script>`, "createElement('embed')"},
		{"markup in a string", `<script>box.innerHTML = '<iframe src="x"></iframe>'</script>`, "<iframe> markup built in script"},
		{"escaped markup in JSON", `<script type="application/json" id="data">{"p":"\u003ciframe srcdoc=x>"}</script>`, "<iframe> markup built in script"},
		{"attachShadow", `<script>el.attachShadow({mode:'open'})</script>`, "attachShadow()"},
		{"document.write", `<script>document.write('<p>x</p>')</script>`, "document.write()"},
		{"event handler", `<button onclick="document.body.append(document.createElement('iframe'))">x</button>`, "createElement('iframe')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := framesDoc(tc.body)
			_, _, _, _, err := normalizeArtifactContent("html", doc)
			if ErrorCode(err) != "artifact_nested_frames" {
				t.Fatalf("err = %v, want artifact_nested_frames", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name %q:\n%s", tc.want, err)
			}
			// The error tells the agent what to write instead.
			for _, hint := range []string{"one document", "<section>", "hidden attribute", "<img src=\"data:", "scoping its CSS under its section id"} {
				if !strings.Contains(err.Error(), hint) {
					t.Fatalf("error lacks the hint %q:\n%s", hint, err)
				}
			}
			if _, err := CheckArtifactHTML(doc); ErrorCode(err) != "artifact_nested_frames" {
				t.Fatalf("dry run err = %v", err)
			}
		})
	}
}

func TestArtifactFramesAcceptsOneDocument(t *testing.T) {
	// Pages that only mention frames, or look like them, are fine.
	cases := map[string]string{
		"escaped markup in prose": `<p>Do not use <code>&lt;iframe srcdoc&gt;</code>; the srcdoc attribute and frame.srcdoc = html are blocked.</p>`,
		"markup in a comment":     `<!-- an <iframe> was here --><p>x</p>`,
		"markup in a textarea":    `<textarea><iframe></iframe></textarea>`,
		"markup in xmp":           `<xmp><iframe></iframe></xmp>`,
		"comparison in script":    `<script>for (let i = 0; i<Object.keys(o).length; i++) {} if (a<frames.length) {}</script>`,
		"tabs that show and hide": `<nav><button data-tab="a">A</button><button data-tab="b">B</button></nav>
<section id="a"><p>A</p></section><section id="b" hidden><img src="data:image/png;base64,AAAA" alt="b"></section>
<script>document.querySelectorAll('[data-tab]').forEach(b => b.onclick = () => document.querySelectorAll('section').forEach(s => s.hidden = s.id !== b.dataset.tab));</script>`,
		"questions block": `<script type="application/json" id="openpoet-questions">{"questions":[{"id":"D1","prompt":"Allow <iframe> embeds?","type":"single","options":[{"id":"a","label":"Yes"},{"id":"b","label":"No"}]}]}</script>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, _, err := normalizeArtifactContent("html", framesDoc(body)); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

func TestArtifactFramesListsAtMostSomeFindings(t *testing.T) {
	body := strings.Repeat("<iframe></iframe>\n", maxArtifactFrameFindings+3)
	_, _, _, _, err := normalizeArtifactContent("html", framesDoc(body))
	if err == nil || !strings.Contains(err.Error(), "and 3 more") {
		t.Fatalf("err = %v", err)
	}
}

func TestArtifactFramesLeaveMarkdownAlone(t *testing.T) {
	// A markdown artifact is rendered by OpenPoet; the render guard blocks any
	// frame its raw HTML would make.
	if _, _, _, _, err := normalizeArtifactContent("markdown", "# x\n\n<iframe></iframe>\n"); err != nil {
		t.Fatalf("markdown refused: %v", err)
	}
}
