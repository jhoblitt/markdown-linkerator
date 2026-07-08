// Package extract parses a markdown file into checkable link targets and the
// set of in-file anchors. It reproduces tcort/markdown-link-check's link
// extraction, disable-directive handling and GitHub heading-slug behavior,
// but classifies each target for the linkerator checker pipeline.
package extract

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	xhtml "golang.org/x/net/html"
)

// FileLinks is the result of parsing one markdown file.
type FileLinks struct {
	// Targets holds one entry per unique (pre-transform) link destination in
	// the file, in first-occurrence order, keeping the first occurrence's line.
	Targets []model.Target
	// Anchors is the set of anchor targets #fragments resolve against.
	Anchors map[string]bool
}

// Disable-directive patterns, mirroring tcort/markdown-link-check. Internal
// spacing is [ \t]+ (exactly, not \s*), so only the canonical comment forms
// match. Upstream's second pattern uses a negative lookahead that RE2 lacks,
// but with a greedy [\s\S]* it only ever matches to EOF, so the lookahead is a
// no-op and is dropped here.
var (
	reDisableBlock = regexp.MustCompile(
		`<!--[ \t]+markdown-link-check-disable[ \t]+-->[\s\S]*?<!--[ \t]+markdown-link-check-enable[ \t]+-->`)
	reDisableToEOF = regexp.MustCompile(
		`<!--[ \t]+markdown-link-check-disable[ \t]+-->[\s\S]*`)
	reDisableNextLine = regexp.MustCompile(
		`<!--[ \t]+markdown-link-check-disable-next-line[ \t]+-->\r?\n[^\r\n]*`)
	reDisableLine = regexp.MustCompile(
		`[^\r\n]*<!--[ \t]+markdown-link-check-disable-line[ \t]+-->[^\r\n]*`)
)

// ParseFile parses the contents (src) of the markdown file at path. Unless
// cfg.IgnoreDisable is set, markdown-link-check-disable regions are stripped
// first, so both the targets and the anchors reflect the disabled document.
func ParseFile(path string, src []byte, cfg config.Resolved) (FileLinks, error) {
	cleaned := src
	if !cfg.IgnoreDisable {
		cleaned = stripDisableDirectives(src)
	}
	return FileLinks{
		Targets: extractTargets(path, cleaned, cfg),
		Anchors: Anchors(cleaned),
	}, nil
}

// stripDisableDirectives removes disabled regions exactly as upstream does:
// paired block, then any unclosed disable to EOF, then disable-next-line plus
// its following line, then any line carrying disable-line. Matches are deleted
// (not blanked), matching upstream; line numbers after a disabled region may
// therefore shift, which is acceptable for best-effort reporting.
func stripDisableDirectives(src []byte) []byte {
	s := string(src)
	s = reDisableBlock.ReplaceAllString(s, "")
	s = reDisableToEOF.ReplaceAllString(s, "")
	s = reDisableNextLine.ReplaceAllString(s, "")
	s = reDisableLine.ReplaceAllString(s, "")
	return []byte(s)
}

func extractTargets(path string, src []byte, cfg config.Resolved) []model.Target {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	doc := md.Parser().Parse(text.NewReader(src))

	baseURL := projectBaseURL(cfg)
	sourceDir := filepath.Dir(path)

	seen := make(map[string]bool)
	var targets []model.Target
	add := func(dest string, off int) {
		if dest == "" || seen[dest] {
			return
		}
		seen[dest] = true
		targets = append(targets, classify(dest, transform(dest, cfg, baseURL), path, sourceDir, lineAt(src, off)))
	}

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			add(string(v.Destination), v.Pos())
		case *ast.Image:
			add(string(v.Destination), v.Pos())
		case *ast.AutoLink:
			dest := string(v.URL(src))
			if v.AutoLinkType == ast.AutoLinkEmail {
				dest = "mailto:" + dest
			}
			add(dest, v.Pos())
		case *ast.RawHTML:
			if v.Segments.Len() > 0 {
				start := v.Segments.At(0).Start
				end := v.Segments.At(v.Segments.Len() - 1).Stop
				scanHTML(src, start, end, add)
			}
		case *ast.HTMLBlock:
			start, end := htmlBlockSpan(v)
			if start >= 0 {
				scanHTML(src, start, end, add)
			}
		}
		return ast.WalkContinue, nil
	})
	return targets
}

func htmlBlockSpan(v *ast.HTMLBlock) (int, int) {
	start, end := -1, -1
	if lines := v.Lines(); lines.Len() > 0 {
		start = lines.At(0).Start
		end = lines.At(lines.Len() - 1).Stop
	}
	if v.HasClosure() {
		cl := v.ClosureLine
		if start < 0 || cl.Start < start {
			start = cl.Start
		}
		if cl.Stop > end {
			end = cl.Stop
		}
	}
	if start < 0 || end <= start {
		return -1, -1
	}
	return start, end
}

// scanHTML tokenizes src[start:end] (a contiguous run of raw HTML) and reports
// href from <a> and src from <img>, with the byte offset of each tag so the
// caller can attribute a line number.
func scanHTML(src []byte, start, end int, add func(dest string, off int)) {
	fragment := src[start:end]
	z := xhtml.NewTokenizer(bytes.NewReader(fragment))
	consumed := 0
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			return
		}
		tokenStart := start + consumed
		consumed += len(z.Raw())
		if tt != xhtml.StartTagToken && tt != xhtml.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if !hasAttr {
			continue
		}
		var want string
		switch string(name) {
		case "a":
			want = "href"
		case "img":
			want = "src"
		default:
			continue
		}
		for {
			key, val, more := z.TagAttr()
			if string(key) == want {
				add(string(val), tokenStart)
				break
			}
			if !more {
				break
			}
		}
	}
}

// transform applies the replacement patterns in order, expanding {{BASEURL}}
// per file. ReplaceAllString (global) is used per the extract contract; the
// resolved config does not carry the per-pattern global flag.
func transform(dest string, cfg config.Resolved, baseURL string) string {
	for _, cr := range cfg.ReplacementPatterns {
		dest = cr.Re.ReplaceAllString(dest, config.ExpandBaseURL(cr.Replacement, baseURL))
	}
	return dest
}

// classify turns a transformed destination into a Target. rawDest is retained
// verbatim as Target.Raw.
func classify(rawDest, dest, sourceFile, sourceDir string, line int) model.Target {
	t := model.Target{Raw: rawDest, SourceFile: sourceFile, Line: line}

	if strings.HasPrefix(dest, "#") {
		t.Kind = model.KindHashLocal
		t.URL = dest
		t.Fragment = dest[1:]
		return t
	}

	if u, err := url.Parse(dest); err == nil {
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			t.Kind = model.KindHTTP
			t.URL = dest
			return t
		case "mailto":
			t.Kind = model.KindMailto
			t.URL = dest
			return t
		case "file":
			t.Kind = model.KindFileRel
			t.URL = fileURLPath(u)
			t.Fragment = u.Fragment
			return t
		}
	}

	// No (recognized) scheme: a relative or absolute filesystem path.
	t.Kind = model.KindFileRel
	pathPart := dest
	if i := strings.IndexByte(pathPart, '#'); i >= 0 {
		t.Fragment = pathPart[i+1:]
		pathPart = pathPart[:i]
	}
	// A link encodes spaces and other characters (e.g. "My%20File.png"); decode
	// to the real filesystem name before resolving.
	if dec, err := url.PathUnescape(pathPart); err == nil {
		pathPart = dec
	}
	t.URL = resolvePath(sourceDir, pathPart)
	return t
}

// resolvePath resolves a link path to an absolute filesystem path. A leading
// '/' is treated as filesystem-absolute (as markdown-link-check does when
// resolving against a file:// base); other paths are relative to the source
// file's directory.
func resolvePath(sourceDir, pathPart string) string {
	var p string
	if filepath.IsAbs(pathPart) {
		p = filepath.Clean(pathPart)
	} else {
		p = filepath.Join(sourceDir, pathPart)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// fileURLPath converts a file:// URL to a filesystem path.
func fileURLPath(u *url.URL) string {
	if u.Path != "" {
		return u.Path
	}
	return u.Opaque
}

func projectBaseURL(cfg config.Resolved) string {
	if cfg.ProjectBaseURL != "" {
		return cfg.ProjectBaseURL
	}
	if cwd, err := os.Getwd(); err == nil {
		return "file://" + cwd
	}
	return ""
}

func lineAt(src []byte, off int) int {
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	return 1 + bytes.Count(src[:off], []byte{'\n'})
}
