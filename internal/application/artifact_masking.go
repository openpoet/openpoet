package application

import (
	"html"
	"regexp"
	"strings"
)

// ArtifactRedaction is one place where credential-looking text was masked in
// an artifact: the line of the document as it was sent, and what kind of text
// it was (the keyword, never the value).
type ArtifactRedaction struct {
	Line int    `json:"line"`
	Kind string `json:"kind"`
}

// maxArtifactRedactionsReported bounds the places listed in a publish result.
// The count is always exact.
const maxArtifactRedactionsReported = 50

// maskArtifactSecrets masks credentials exactly as redactReportSecrets does
// (the same patterns, applied in the same order) and reports each place where
// the text changed. Lines are those of the input: a match that swallows line
// breaks does not shift the lines reported after it.
func maskArtifactSecrets(content string) (string, []ArtifactRedaction) {
	var redactions []ArtifactRedaction
	// origin[i] is the input line that line i of the current text starts on.
	var origin []int
	for index, pattern := range reportSecretPatterns {
		matches := pattern.FindAllStringSubmatchIndex(content, -1)
		if len(matches) == 0 {
			continue
		}
		if origin == nil {
			origin = make([]int, strings.Count(content, "\n")+1)
			for i := range origin {
				origin[i] = i + 1
			}
		}
		var out strings.Builder
		out.Grow(len(content))
		next := make([]int, 0, len(origin))
		line, position := 0, 0
		// keep copies the lines of text[position:end] that survive.
		keep := func(text string) {
			for i := 0; i < strings.Count(text, "\n"); i++ {
				line++
				next = append(next, origin[line])
			}
		}
		next = append(next, origin[0])
		for _, match := range matches {
			start, end := match[0], match[1]
			before := content[position:start]
			out.WriteString(before)
			keep(before)
			matched := content[start:end]
			replacement, kind := "[REDACTED]", "private key"
			switch index {
			case 0:
				replacement, kind = content[match[2]:match[3]]+"[REDACTED]", "authorization bearer"
			case 1:
				kind = strings.ToLower(content[match[2]:match[3]])
			}
			if replacement != matched {
				redactions = append(redactions, ArtifactRedaction{Line: origin[line], Kind: kind})
			}
			out.WriteString(replacement)
			// The line breaks the replacement keeps are the first ones of the
			// match; the rest are gone, and their lines fold into this one.
			kept := strings.Count(replacement, "\n")
			for i := 0; i < kept; i++ {
				line++
				next = append(next, origin[line])
			}
			line += strings.Count(matched, "\n") - kept
			position = end
		}
		rest := content[position:]
		out.WriteString(rest)
		keep(rest)
		content, origin = out.String(), next
	}
	return content, redactions
}

var (
	artifactNonTextPattern = regexp.MustCompile(`(?is)<script\b.*?</script\s*>|<style\b.*?</style\s*>|<!--.*?-->`)
	artifactTagPattern     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// artifactPlainText approximates the text a browser shows for an HTML
// document: scripts, styles, comments and tags removed, entities decoded,
// whitespace folded. It is only used to tell whether a quoted passage is still
// in a version; it does not parse the document.
func artifactPlainText(content string) string {
	content = artifactNonTextPattern.ReplaceAllString(content, "")
	content = artifactTagPattern.ReplaceAllString(content, "")
	return foldArtifactWhitespace(html.UnescapeString(content))
}

func foldArtifactWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
