package pipeline

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// srcItem is one unit produced by the crawler: a markdown file to parse, a bare
// URL argument to check directly, or stdin.
type srcItem struct {
	path  string // markdown file path, or "(stdin)" / the URL for display
	url   string // non-empty when the input was a bare URL argument
	stdin bool
}

var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

func isMarkdown(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// crawl classifies each input and streams source items to out. A directory is
// walked for markdown files; a bare URL is checked directly; a glob is expanded;
// no inputs means read stdin. A plain path that does not exist is a fatal error.
func crawl(ctx context.Context, inputs []string, out chan<- srcItem) error {
	if len(inputs) == 0 {
		return send(ctx, out, srcItem{path: "(stdin)", stdin: true})
	}
	for _, in := range inputs {
		if isURL(in) {
			if err := send(ctx, out, srcItem{path: in, url: in}); err != nil {
				return err
			}
			continue
		}
		info, err := os.Stat(in)
		switch {
		case err == nil && info.IsDir():
			if err := walkMarkdown(ctx, in, out); err != nil {
				return err
			}
		case err == nil:
			if err := send(ctx, out, srcItem{path: in}); err != nil {
				return err
			}
		default:
			matches, gerr := expandGlob(in)
			if gerr != nil {
				return gerr
			}
			if len(matches) == 0 {
				return fmt.Errorf("no such file, directory, or match: %s", in)
			}
			for _, m := range matches {
				if err := send(ctx, out, srcItem{path: m}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func walkMarkdown(ctx context.Context, root string, out chan<- srcItem) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !isMarkdown(d.Name()) {
			return nil
		}
		return send(ctx, out, srcItem{path: p})
	})
}

// expandGlob expands a shell-style pattern, additionally supporting a single
// "**" segment for recursive matching (filepath.Glob does not).
func expandGlob(pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Glob(pattern)
	}
	idx := strings.Index(pattern, "**")
	base := filepath.Dir(strings.TrimRight(pattern[:idx], string(os.PathSeparator)))
	if base == "" {
		base = "."
	}
	tail := strings.TrimLeft(pattern[idx+2:], string(os.PathSeparator))
	var out []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		ok, merr := filepath.Match(tail, filepath.Base(p))
		if merr != nil {
			return merr
		}
		if ok || tail == "" {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func send[T any](ctx context.Context, ch chan<- T, v T) error {
	select {
	case ch <- v:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
