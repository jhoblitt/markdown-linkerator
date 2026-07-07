package extract

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The anchor and slug logic reproduces tcort/markdown-link-check's
// extractSections / extractHtmlSections, with one deliberate divergence: the
// upstream wraps each heading slug in encodeURIComponent so the (already
// percent-encoded) link fragments match. The linkerator pipeline percent-
// decodes and lowercases fragments before lookup, so slugs are kept decoded
// (literal lowercased Unicode, no %XX) to match that convention.

var (
	// reHeading finds ATX headings. Upstream uses /^#+ .*$/gm: one-or-more '#'
	// (no 1..6 cap), a single literal space, then the rest of the line.
	reHeading = regexp.MustCompile(`(?m)^#+ [^\r\n]*`)

	// reCodeBlock matches a fenced code block delimited by lines that are
	// exactly ``` (backtick fences only; ~~~ is not handled upstream).
	reCodeBlock = regexp.MustCompile("(?m)^```[\\s\\S]+?^```$")

	reHTMLComment = regexp.MustCompile(`<!--[\s\S]+?-->`)

	// reAllID / reAName mirror upstream: id="" on any tag, name="" on any tag
	// whose name starts with "a". '=' has no surrounding whitespace and '.'
	// does not cross newlines, so a tag must sit on one line.
	reAllID = regexp.MustCompile(`(?i)<[^\s]+.*?id=["']([^"']*?)["'].*?>`)
	reAName = regexp.MustCompile(`(?i)<a.*?name=["']([^"']*?)["'].*?>`)

	// reSlugLink reduces an inline link to its text. The URL must start with
	// ./, /, http://, https:// or #, matching upstream's narrow pattern.
	reSlugLink = regexp.MustCompile(`\[(.+)\]\((?:\.?/|https?://|#)[0-9A-Za-z_./?=#-]+\)`)

	reSlugHead = regexp.MustCompile(`^#+\s*`)
)

// Anchors extracts the set of in-file anchor targets from src: heading slugs
// (GitHub algorithm with -1/-2 duplicate suffixing) plus HTML id="" and
// <a name=""> values. It is exposed for the golden-oracle test and performs no
// disable-directive handling.
func Anchors(src []byte) map[string]bool {
	md := normalizeNewlines(string(src))
	set := make(map[string]bool)
	for _, s := range headingSlugs(md) {
		set[s] = true
	}
	for _, s := range htmlIDs(md) {
		set[s] = true
	}
	return set
}

// Slug converts a single heading's text to its GitHub anchor slug without
// duplicate suffixing. It accepts the text with or without a leading '#'
// sequence.
func Slug(heading string) string {
	s := slugReduceLink(heading)
	s = strings.ToLower(s)
	s = reSlugHead.ReplaceAllString(s, "")

	// Keep Unicode letters, Nd/Nl numbers, '_' and '-'; map each whitespace
	// rune to a single '-' (runs are NOT collapsed and edges are NOT trimmed);
	// drop everything else (punctuation, '*', backtick, '&', '/', ...).
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.Is(unicode.Nd, r), unicode.Is(unicode.Nl, r), r == '_', r == '-':
			b.WriteRune(r)
		case isSlugSpace(r):
			b.WriteByte('-')
		}
	}
	return b.String()
}

// headingSlugs returns one slug per heading, in document order, with duplicate
// suffixing applied exactly as upstream: a per-base counter where the suffixed
// name is itself registered (so a later literal "foo-1" can collide onward).
func headingSlugs(md string) []string {
	md = reCodeBlock.ReplaceAllString(md, "")
	lines := reHeading.FindAllString(md, -1)
	seen := make(map[string]int, len(lines))
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		section := Slug(line)
		if _, ok := seen[section]; ok {
			seen[section]++
			section = section + "-" + strconv.Itoa(seen[section])
		}
		seen[section] = 0
		out = append(out, section)
	}
	return out
}

// htmlIDs returns id="" and <a name=""> values, after stripping fenced code,
// HTML comments and genuine (unescaped-backtick) inline code so that anchors
// inside those constructs are not harvested.
func htmlIDs(md string) []string {
	md = reCodeBlock.ReplaceAllString(md, "")
	md = reHTMLComment.ReplaceAllString(md, "")
	md = removeInlineCode(md)

	var out []string
	for _, m := range reAllID.FindAllStringSubmatch(md, -1) {
		out = append(out, m[1])
	}
	for _, m := range reAName.FindAllStringSubmatch(md, -1) {
		out = append(out, m[1])
	}
	return out
}

// slugReduceLink replaces the first inline link [text](url) with its text,
// matching upstream's single (non-global) replacement.
func slugReduceLink(s string) string {
	loc := reSlugLink.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + s[loc[2]:loc[3]] + s[loc[1]:]
}

// removeInlineCode deletes inline code spans delimited by backticks that are
// not backslash-escaped, emulating upstream's (?<!\\) backtick ... (?<!\\)
// backtick regex (Go's RE2 has no lookbehind). A backtick is an "unescaped"
// delimiter iff the byte immediately before it is not a backslash — matching
// upstream bug-for-bug, so a backslash-escaped backtick does not open or close
// a span and its contents (e.g. an <a id=...>) are retained. Byte-wise
// scanning is safe: backtick and backslash are ASCII and never appear inside a
// multibyte UTF-8 sequence.
func removeInlineCode(s string) string {
	b := make([]byte, 0, len(s))
	for i, n := 0, len(s); i < n; {
		if s[i] == '`' && (i == 0 || s[i-1] != '\\') {
			closeIdx := -1
			for j := i + 2; j < n; j++ { // j>=i+2: the span needs >=1 inner char
				if s[j] == '`' && s[j-1] != '\\' {
					closeIdx = j
					break
				}
			}
			if closeIdx >= 0 {
				i = closeIdx + 1
				continue
			}
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}

// isSlugSpace reports whether r is whitespace under ECMAScript's \s (the class
// upstream uses), so slug whitespace handling matches for exotic spaces too.
func isSlugSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
