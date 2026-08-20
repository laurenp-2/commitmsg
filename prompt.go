package main

import (
	"strings"
	"unicode"
)

const maxCommitSubjectLength = 72

// SystemPrompt returns the instruction sent to Ollama alongside the staged diff.
// Keeping it separate from the user prompt makes it easy to inspect with --verbose
// and prevents repository content from being treated as instructions.
func SystemPrompt(includeBody bool) string {
	prompt := `Generate one Git commit message from the supplied staged changes.

Output only the commit message. Do not add a preamble, explanation, markdown, or code fences.

Use Conventional Commits in the form type(scope): subject. The allowed types are feat, fix, docs, style, refactor, perf, test, build, ci, chore, and revert. The scope is optional: omit it when it is not clear instead of inventing one.

Write the subject in imperative mood (for example, "add", not "added" or "adds"), use lowercase after the colon, do not end it with a period, and keep it at or below 72 characters.`

	if includeBody {
		prompt += `

After the subject, add one blank line followed by at most three short bullet lines. Use those bullets to explain why the change is needed, not to restate the diff.`
	}

	return prompt
}

// BuildUserPrompt combines branch context and the deterministic diff context. The
// context is expected to contain the file summary followed by any selected hunks.
func BuildUserPrompt(branch, context string) string {
	context = strings.TrimSpace(context)
	branch = strings.TrimSpace(branch)

	parts := make([]string, 0, 2)
	if branch != "" && branch != "main" && branch != "master" {
		parts = append(parts, "Current branch: "+branch)
	}
	if context != "" {
		parts = append(parts, context)
	}

	return strings.Join(parts, "\n\n")
}

// SanitizeResponse returns a single, safe commit subject from a model response.
// Use SanitizeResponseWithBody when --body was requested.
func SanitizeResponse(raw string) string {
	return SanitizeResponseWithBody(raw, false)
}

// sanitizeResponse keeps the small internal-style helper name available for
// callers in this package while the exported function remains useful to tests.
func sanitizeResponse(raw string) string {
	return SanitizeResponse(raw)
}

// SanitizeResponseWithBody removes common model formatting mistakes without making
// a network call. When includeBody is true, it retains up to three bullet lines
// after the subject; otherwise it returns only the subject.
func SanitizeResponseWithBody(raw string, includeBody bool) string {
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = stripResponseEnvelope(text)
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first == len(lines) {
		return ""
	}

	// A subject can be wrapped across multiple lines by a small model. Collapse its
	// first paragraph, but stop before bullets even if the model forgot the blank
	// separator before a body.
	subjectLines := make([]string, 0, 1)
	i := first
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || (len(subjectLines) > 0 && isBullet(line)) {
			break
		}
		subjectLines = append(subjectLines, line)
	}

	subject := strings.Join(subjectLines, " ")
	subject = trimSurroundingQuotes(strings.TrimSpace(subject))
	subject = strings.Join(strings.Fields(subject), " ")
	subject = truncateSubjectAtWordBoundary(subject, maxCommitSubjectLength)
	if subject == "" || !includeBody {
		return subject
	}

	body := sanitizeBody(lines[i:])
	if len(body) == 0 {
		return subject
	}

	return subject + "\n\n" + strings.Join(body, "\n")
}

// stripResponseEnvelope applies the independent cleanup operations repeatedly.
// Models commonly nest them (for example, a preamble followed by a fenced,
// quoted message), so a single pass is not enough to reach the real subject.
func stripResponseEnvelope(text string) string {
	for range 4 {
		before := text
		text = stripSurroundingCodeFence(text)
		text = stripLeadingPreamble(text)
		text = trimSurroundingQuotes(strings.TrimSpace(text))
		if text == before {
			break
		}
	}
	return strings.TrimSpace(text)
}

func stripSurroundingCodeFence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		return text
	}

	last := len(lines) - 1
	for last > 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if strings.TrimSpace(lines[last]) != "```" {
		return text
	}

	return strings.TrimSpace(strings.Join(lines[1:last], "\n"))
}

func stripLeadingPreamble(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for len(lines) > 0 {
		first := 0
		for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
			first++
		}
		if first == len(lines) {
			return ""
		}
		if first > 0 {
			lines = lines[first:]
		}

		remainder, isPreamble := preambleRemainder(strings.TrimSpace(lines[0]))
		if !isPreamble {
			break
		}
		if remainder == "" {
			lines = lines[1:]
			continue
		}
		lines[0] = remainder
		break
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// preambleRemainder recognizes the introductory lines models commonly emit. If a
// model puts the message after a colon on that same line, the message is retained.
func preambleRemainder(line string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(line))

	for _, prefix := range []string{
		"commit message",
		"suggested commit message",
		"suggested message",
		"the commit message",
	} {
		if remainder, ok := prefixedRemainder(line, lower, prefix); ok {
			return remainder, true
		}
	}

	if strings.HasPrefix(lower, "here is") || strings.HasPrefix(lower, "here's") {
		if colon := strings.Index(line, ":"); colon >= 0 {
			return strings.TrimSpace(line[colon+1:]), true
		}
		return "", true
	}

	return "", false
}

func prefixedRemainder(line, lower, prefix string) (string, bool) {
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(line[len(prefix):])
	if rest == "" {
		return "", true
	}
	if rest[0] != ':' && rest[0] != '-' {
		return "", false
	}
	return strings.TrimSpace(rest[1:]), true
}

func trimSurroundingQuotes(text string) string {
	text = strings.TrimSpace(text)
	for len(text) >= 2 {
		runes := []rune(text)
		first, last := runes[0], runes[len(runes)-1]
		if !matchingQuotes(first, last) {
			break
		}
		text = strings.TrimSpace(string(runes[1 : len(runes)-1]))
	}
	return text
}

func matchingQuotes(first, last rune) bool {
	return (first == '"' && last == '"') ||
		(first == '\'' && last == '\'') ||
		(first == '“' && last == '”') ||
		(first == '‘' && last == '’')
}

func truncateSubjectAtWordBoundary(subject string, max int) string {
	runes := []rune(subject)
	if len(runes) <= max {
		return subject
	}

	cut := -1
	for i := max; i >= 0; i-- {
		if i < len(runes) && unicode.IsSpace(runes[i]) {
			cut = i
			break
		}
	}
	if cut < 0 {
		return ""
	}

	truncated := strings.TrimSpace(string(runes[:cut]))
	truncated = strings.TrimRightFunc(truncated, unicode.IsPunct)
	return strings.TrimSpace(truncated)
}

func isBullet(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "• ")
}

func sanitizeBody(lines []string) []string {
	body := make([]string, 0, 3)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !isBullet(line) {
			continue
		}

		item := strings.TrimSpace(strings.TrimLeft(line, "-*• "))
		item = strings.Join(strings.Fields(item), " ")
		if item == "" {
			continue
		}
		body = append(body, "- "+item)
		if len(body) == 3 {
			break
		}
	}
	return body
}
