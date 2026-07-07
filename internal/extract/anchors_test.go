package extract

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSlug(t *testing.T) {
	// Expected values verified against tcort/markdown-link-check's
	// extractSections transform (minus encodeURIComponent).
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "Hello World", "hello-world"},
		{"unicode retained lowercased", "Café Über", "café-über"},
		{"german umlaut not percent-encoded", "Header with German umlaut Ö", "header-with-german-umlaut-ö"},
		{"ampersand removed leaves double hyphen", "Foo & Bar", "foo--bar"},
		{"removed special char leaves double hyphen", "Header with special ✔ char", "header-with-special--char"},
		{"trailing removed char leaves trailing hyphen", "Header with special char at end ✔", "header-with-special-char-at-end-"},
		{"comma removed single hyphen", "Uh, oh", "uh-oh"},
		{"slashes and question removed no gap", "a/b?c", "abc"},
		{"trailing punctuation removed", "Trailing!", "trailing"},
		{"with atx prefix", "## Hello World", "hello-world"},
		{"underscores are kept", "snake_case_heading", "snake_case_heading"},
		{"repeated underscores kept", "foo___bar", "foo___bar"},
		{"emphasis markers stripped underscores kept", "**bold** and _em_ and `code`", "bold-and-_em_-and-code"},
		{"asterisks removed", "x*y*z", "xyz"},
		{"backticks removed", "a `b` c", "a-b-c"},
		{"inline link reduced to text", "[text](http://x.com)", "text"},
		{"relative link reduced to text", "[text](./rel.md)", "text"},
		{"digits kept", "Section 1.2.3", "section-123"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, Slug(c.in))
		})
	}
}

func TestAnchorsHeadingDedup(t *testing.T) {
	// Per-base counter with the suffixed name registered, so a later literal
	// "Foo 1" (slug "foo-1") collides onward to "foo-1-1".
	got := Anchors([]byte("# Foo\n## Foo\n### Foo\n#### Foo 1\n"))
	assert.Equal(t, []string{"foo", "foo-1", "foo-1-1", "foo-2"}, sortedKeys(got))
}

func TestAnchorsHeadingDedupCollision(t *testing.T) {
	got := Anchors([]byte("# Foo\n## Foo\n### Foo-1\n"))
	assert.Equal(t, []string{"foo", "foo-1", "foo-1-1"}, sortedKeys(got))
}

func TestAnchorsHTMLIDsAndNames(t *testing.T) {
	md := "## Head\n" +
		"<a id=\"tomato_id\"></a>\n" +
		"<a name=\"tomato_name\"></a>\n" +
		"<a id='tomato_id_single_quote'></a>\n" +
		"<a name='tomato_name_single_quote'></a>\n" +
		"<div id=\"onion_outer\"><span id=\"onion_inner\"></span></div>\n" +
		"<a id=\"onion\"></a>\n" +
		"`<a id=\"tomato_code\"></a>`\n" +
		"\\`<a id=\"tomato_escaped_backticks\"></a>\\`\n" +
		"<!-- <a id=\"tomato_comment\"></a> -->\n" +
		"```\n## NotAHeading\n<a id=\"in_code_block\"></a>\n```\n"

	got := Anchors([]byte(md))
	want := []string{
		"head",
		"onion", "onion_inner", "onion_outer",
		"tomato_escaped_backticks",
		"tomato_id", "tomato_id_single_quote",
		"tomato_name", "tomato_name_single_quote",
	}
	assert.Equal(t, want, sortedKeys(got))

	// Excluded: genuine inline code, HTML comment, fenced code block.
	assert.NotContains(t, got, "tomato_code")
	assert.NotContains(t, got, "tomato_comment")
	assert.NotContains(t, got, "in_code_block")
	assert.NotContains(t, got, "notaheading")
}

func TestAnchorsIDRequiresNoSpaceAroundEquals(t *testing.T) {
	// Upstream's regex is literally id=["'] with no surrounding whitespace.
	got := Anchors([]byte(`<a id = "spaced"></a> <a id="tight"></a>`))
	assert.Equal(t, []string{"tight"}, sortedKeys(got))
}

func TestAnchorsHeadingInsideInlineCodeStillCounts(t *testing.T) {
	// Headings only strip fenced code blocks, not inline code, and a '#' must
	// start the line, so this is a real heading.
	got := Anchors([]byte("# Real `code` Heading\n"))
	assert.Equal(t, []string{"real-code-heading"}, sortedKeys(got))
}

func TestAnchorsCRLF(t *testing.T) {
	got := Anchors([]byte("# Foo Bar\r\n<a id=\"x\"></a>\r\n"))
	assert.Equal(t, []string{"foo-bar", "x"}, sortedKeys(got))
}

func TestAnchorsFencedCodeBlockExcludesHeadings(t *testing.T) {
	md := "# Kept\n```\n# Ignored\n```\n# Also Kept\n"
	got := Anchors([]byte(md))
	assert.Equal(t, []string{"also-kept", "kept"}, sortedKeys(got))
}

func TestRemoveInlineCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain span removed", "a `code` b", "a  b"},
		{"escaped backticks preserved", "x \\`keep\\` y", "x \\`keep\\` y"},
		{"unterminated backtick kept", "a `b", "a `b"},
		{"triple backtick span leaves inner text", "a ```x``` b", "a x b"},
		{"genuine code hides id", "`<a id=\"y\">`", ""},
		{"escaped code exposes id", "\\`<a id=\"x\">\\`", "\\`<a id=\"x\">\\`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, removeInlineCode(c.in))
		})
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
