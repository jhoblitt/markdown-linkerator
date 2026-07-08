package extract

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"testing"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const srcPath = "/home/u/proj/doc.md"

// resolved mirrors how extract resolves a relative link to an absolute path, so
// expectations hold on any OS — on Windows filepath.Abs adds a drive and
// backslashes, which a hard-coded "/home/u/proj/..." literal would not match.
func resolved(rel string) string {
	p, _ := filepath.Abs(filepath.Join(filepath.Dir(srcPath), rel))
	return p
}

func parse(t *testing.T, md string, cfg config.Resolved) FileLinks {
	t.Helper()
	fl, err := ParseFile(srcPath, []byte(md), cfg)
	require.NoError(t, err)
	return fl
}

func urls(targets []model.Target) []string {
	out := make([]string, 0, len(targets))
	for _, tg := range targets {
		out = append(out, tg.URL)
	}
	sort.Strings(out)
	return out
}

func onlyTarget(t *testing.T, md string, cfg config.Resolved) model.Target {
	t.Helper()
	fl := parse(t, md, cfg)
	require.Len(t, fl.Targets, 1, "expected exactly one target, got %+v", fl.Targets)
	return fl.Targets[0]
}

func TestExtractInlineLinkAndImage(t *testing.T) {
	fl := parse(t, "A [link](http://a.example.com) and ![img](pic.png).\n", config.Resolved{})
	require.Len(t, fl.Targets, 2)

	link := fl.Targets[0]
	assert.Equal(t, "http://a.example.com", link.Raw)
	assert.Equal(t, model.KindHTTP, link.Kind)
	assert.Equal(t, "http://a.example.com", link.URL)

	img := fl.Targets[1]
	assert.Equal(t, "pic.png", img.Raw)
	assert.Equal(t, model.KindFileRel, img.Kind)
	assert.Equal(t, resolved("pic.png"), img.URL)
}

func TestExtractReferenceLink(t *testing.T) {
	md := "See [the ref][r1] here.\n\n[r1]: http://ref.example.com\n"
	tg := onlyTarget(t, md, config.Resolved{})
	assert.Equal(t, model.KindHTTP, tg.Kind)
	assert.Equal(t, "http://ref.example.com", tg.URL)
}

func TestExtractAngleAutolinkURL(t *testing.T) {
	tg := onlyTarget(t, "Visit <https://auto.example.com> now.\n", config.Resolved{})
	assert.Equal(t, model.KindHTTP, tg.Kind)
	assert.Equal(t, "https://auto.example.com", tg.URL)
}

func TestExtractAngleAutolinkEmailBecomesMailto(t *testing.T) {
	tg := onlyTarget(t, "Contact <foo@bar.com> please.\n", config.Resolved{})
	assert.Equal(t, model.KindMailto, tg.Kind)
	assert.Equal(t, "mailto:foo@bar.com", tg.URL)
	assert.Equal(t, "mailto:foo@bar.com", tg.Raw)
}

func TestExtractGFMBareAutolinks(t *testing.T) {
	md := "Bare www.bare.example.com and mail me at plain@example.org here.\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{"http://www.bare.example.com", "mailto:plain@example.org"}, urls(fl.Targets))

	var mailto model.Target
	for _, tg := range fl.Targets {
		if tg.Kind == model.KindMailto {
			mailto = tg
		}
	}
	assert.Equal(t, model.KindMailto, mailto.Kind)
	assert.Equal(t, "mailto:plain@example.org", mailto.URL)
}

func TestExtractRawHTMLInline(t *testing.T) {
	md := "See <a href=\"http://raw.example.com\">x</a> and <img src=\"pic.png\"> end.\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{resolved("pic.png"), "http://raw.example.com"}, urls(fl.Targets))
}

func TestExtractRawHTMLBlock(t *testing.T) {
	md := "<div>\n<a href=\"http://block.example.com\">x</a>\n<img src=\"deep/pic.png\">\n</div>\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{resolved("deep/pic.png"), "http://block.example.com"}, urls(fl.Targets))
}

func TestDedupKeepsFirstLine(t *testing.T) {
	md := "One [a](http://dup.example.com)\n\nTwo [b](http://dup.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	require.Len(t, fl.Targets, 1)
	assert.Equal(t, "http://dup.example.com", fl.Targets[0].URL)
	assert.Equal(t, 1, fl.Targets[0].Line)
}

func TestLineNumbers(t *testing.T) {
	md := "line1\nline2 [a](http://a.example.com)\nline3\nline4 [b](http://b.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	require.Len(t, fl.Targets, 2)
	assert.Equal(t, 2, fl.Targets[0].Line)
	assert.Equal(t, 4, fl.Targets[1].Line)
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		dest     string
		wantKind model.Kind
		wantURL  string
		wantFrag string
		unixOnly bool // absolute-path / file:// semantics differ on Windows
	}{
		{"http", "http://x.example.com/p", model.KindHTTP, "http://x.example.com/p", "", false},
		{"https", "https://x.example.com", model.KindHTTP, "https://x.example.com", "", false},
		{"mailto", "mailto:a@b.com", model.KindMailto, "mailto:a@b.com", "", false},
		{"hash local", "#some-section", model.KindHashLocal, "#some-section", "some-section", false},
		{"relative file", "sub/other.md", model.KindFileRel, resolved("sub/other.md"), "", false},
		{"relative with fragment", "other.md#sec", model.KindFileRel, resolved("other.md"), "sec", false},
		{"dot relative", "./sibling.md", model.KindFileRel, resolved("sibling.md"), "", false},
		{"parent relative", "../up.md", model.KindFileRel, resolved("../up.md"), "", false},
		{"absolute path", "/etc/hosts", model.KindFileRel, "/etc/hosts", "", true},
		{"file scheme", "file:///tmp/f.txt", model.KindFileRel, "/tmp/f.txt", "", true},
		{"file scheme with fragment", "file:///tmp/f.md#frag", model.KindFileRel, "/tmp/f.md", "frag", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.unixOnly && runtime.GOOS == "windows" {
				t.Skip("absolute-path / file:// resolution is Unix-specific")
			}
			tg := onlyTarget(t, "[x]("+c.dest+")\n", config.Resolved{})
			assert.Equal(t, c.wantKind, tg.Kind)
			assert.Equal(t, c.wantURL, tg.URL)
			assert.Equal(t, c.wantFrag, tg.Fragment)
			assert.Equal(t, c.dest, tg.Raw)
		})
	}
}

func TestReplacementPatternPlain(t *testing.T) {
	cfg := config.Resolved{
		ReplacementPatterns: []config.CompiledReplacement{
			{Re: regexp.MustCompile(`^http://old\.example\.com`), Replacement: "http://new.example.com"},
		},
	}
	tg := onlyTarget(t, "[x](http://old.example.com/page)\n", cfg)
	assert.Equal(t, "http://old.example.com/page", tg.Raw)
	assert.Equal(t, model.KindHTTP, tg.Kind)
	assert.Equal(t, "http://new.example.com/page", tg.URL)
}

func TestReplacementPatternBaseURLExplicit(t *testing.T) {
	cfg := config.Resolved{
		ProjectBaseURL: "file:///project",
		ReplacementPatterns: []config.CompiledReplacement{
			{Re: regexp.MustCompile(`^/`), Replacement: "{{BASEURL}}/"},
		},
	}
	tg := onlyTarget(t, "[x](/foo/bar)\n", cfg)
	assert.Equal(t, "/foo/bar", tg.Raw)
	assert.Equal(t, model.KindFileRel, tg.Kind)
	assert.Equal(t, "/project/foo/bar", tg.URL)
}

func TestReplacementPatternBaseURLDefaultsToCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("BASEURL defaulting to cwd resolves through a file:// path — Unix-specific")
	}
	cwd, err := os.Getwd()
	require.NoError(t, err)
	cfg := config.Resolved{
		ReplacementPatterns: []config.CompiledReplacement{
			{Re: regexp.MustCompile(`^/`), Replacement: "{{BASEURL}}/"},
		},
	}
	tg := onlyTarget(t, "[x](/foo)\n", cfg)
	assert.Equal(t, model.KindFileRel, tg.Kind)
	assert.Equal(t, cwd+"/foo", tg.URL)
}

func TestDisableBlock(t *testing.T) {
	md := "[a](http://a.example.com)\n" +
		"<!-- markdown-link-check-disable -->\n" +
		"[b](http://b.example.com)\n" +
		"<!-- markdown-link-check-enable -->\n" +
		"[c](http://c.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{"http://a.example.com", "http://c.example.com"}, urls(fl.Targets))
}

func TestDisableNextLine(t *testing.T) {
	md := "[a](http://a.example.com)\n" +
		"<!-- markdown-link-check-disable-next-line -->\n" +
		"[b](http://b.example.com)\n" +
		"[c](http://c.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{"http://a.example.com", "http://c.example.com"}, urls(fl.Targets))
}

func TestDisableLine(t *testing.T) {
	md := "[a](http://a.example.com)\n" +
		"[b](http://b.example.com) <!-- markdown-link-check-disable-line -->\n" +
		"[c](http://c.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{"http://a.example.com", "http://c.example.com"}, urls(fl.Targets))
}

func TestDisableUnclosedRunsToEOF(t *testing.T) {
	md := "[a](http://a.example.com)\n" +
		"<!-- markdown-link-check-disable -->\n" +
		"[b](http://b.example.com)\n" +
		"[c](http://c.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{"http://a.example.com"}, urls(fl.Targets))
}

func TestIgnoreDisableKeepsEverything(t *testing.T) {
	md := "[a](http://a.example.com)\n" +
		"<!-- markdown-link-check-disable -->\n" +
		"[b](http://b.example.com)\n" +
		"[c](http://c.example.com)\n"
	fl := parse(t, md, config.Resolved{IgnoreDisable: true})
	assert.Equal(t, []string{"http://a.example.com", "http://b.example.com", "http://c.example.com"}, urls(fl.Targets))
}

func TestMarkdownlintCommentIsNotADisableDirective(t *testing.T) {
	md := "<!-- markdownlint-disable MD033 -->\n[a](http://a.example.com)\n"
	fl := parse(t, md, config.Resolved{})
	assert.Equal(t, []string{"http://a.example.com"}, urls(fl.Targets))
}

func TestParseFileAnchorsReflectDisable(t *testing.T) {
	md := "# Kept\n" +
		"<!-- markdown-link-check-disable -->\n" +
		"# Hidden\n" +
		"<!-- markdown-link-check-enable -->\n"
	fl := parse(t, md, config.Resolved{})
	assert.True(t, fl.Anchors["kept"])
	assert.False(t, fl.Anchors["hidden"])
}

func TestHashResolutionAgainstOwnAnchors(t *testing.T) {
	md := "# Some Section\n\nJump to [here](#some-section).\n"
	fl := parse(t, md, config.Resolved{})
	require.Len(t, fl.Targets, 1)
	assert.Equal(t, model.KindHashLocal, fl.Targets[0].Kind)
	assert.Equal(t, "some-section", fl.Targets[0].Fragment)
	assert.True(t, fl.Anchors[fl.Targets[0].Fragment])
}
