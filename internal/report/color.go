package report

import (
	"os"
	"strings"
)

// palette applies ANSI SGR color codes, or passes strings through untouched
// when color is disabled. It is intentionally dependency-free.
type palette struct {
	enabled bool
}

// newPalette enables color unless the caller opted out or the NO_COLOR
// convention is set in the environment (https://no-color.org).
func newPalette(noColor bool) palette {
	if noColor || os.Getenv("NO_COLOR") != "" {
		return palette{}
	}
	return palette{enabled: true}
}

func (p palette) wrap(code, s string) string {
	if !p.enabled || s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(code) + 6)
	b.WriteString("\x1b[")
	b.WriteString(code)
	b.WriteByte('m')
	b.WriteString(s)
	b.WriteString("\x1b[0m")
	return b.String()
}

func (p palette) cyan(s string) string   { return p.wrap("36", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }
func (p palette) dim(s string) string    { return p.wrap("90", s) }
