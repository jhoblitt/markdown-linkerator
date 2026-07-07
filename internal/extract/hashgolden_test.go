package extract_test

import (
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/extract"
)

// TestHashLinksGolden pins the GitHub-slug + HTML-anchor algorithm against the
// markdown-link-check hash-links.md fixture (the authoritative oracle). If this
// changes, anchor resolution behavior changed — review the diff.
func TestHashLinksGolden(t *testing.T) {
	src, err := os.ReadFile("testdata/hash-links.md")
	require.NoError(t, err)

	// The complete expected anchor set: heading slugs (no whitespace collapse,
	// no trimming, Unicode retained lowercased) plus HTML id/name, excluding
	// anchors inside inline code and HTML comments.
	want := []string{
		// heading slugs
		"foo",
		"bar",
		"baz",
		"uh-oh",
		"header-with-special-char-at-end-",
		"header-with-multiple-special-chars-at-end-",
		"header-with-special--char",
		"header-with-multiple-special--chars",
		"header-with-german-umlaut-ö",
		"header-with-german-umlaut-ö-manual-encoded-link",
		"heading-with-a-link",
		"heading-with-an-anchor-link",
		"--docker",
		"step-7---lint--test",
		"product-owner--design-approval",
		"migrating-from--v1180",
		"clientserver-examples-using--networkpeer",
		"this-header-is-linked",
		"somewhere",
		"l-is-the-package-in-the-linux-distro-base-image",
		// HTML id/name anchors (escaped-backticks one counts; code + comment do not)
		"tomato_id",
		"tomato_name",
		"tomato_id_single_quote",
		"tomato_name_single_quote",
		"onion",
		"onion_outer",
		"onion_inner",
		"tomato_escaped_backticks",
	}

	got := extract.Anchors(src)
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	sort.Strings(want)
	assert.Equal(t, want, gotKeys)

	// Anchors that must NOT be present.
	assert.NotContains(t, got, "tomato_code", "inline-code anchor must be excluded")
	assert.NotContains(t, got, "tomato_comment", "HTML-comment anchor must be excluded")
}

// TestSlugCases exercises the slug algorithm on the trickiest oracle headings
// directly, so a failure points at the transform rather than the whole set.
func TestSlugCases(t *testing.T) {
	cases := map[string]string{
		"Foo":                               "foo",
		"Uh, oh":                            "uh-oh",
		"Header with special ✔ char":        "header-with-special--char",
		"Header with special char at end ✔": "header-with-special-char-at-end-",
		"Header with German umlaut Ö":       "header-with-german-umlaut-ö",
		"Step 7 - Lint & Test":              "step-7---lint--test",
		"Product Owner / Design Approval":   "product-owner--design-approval",
		"L. Is the package in the Linux distro base image?": "l-is-the-package-in-the-linux-distro-base-image",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, extract.Slug(in))
		})
	}
}
