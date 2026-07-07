package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that unmarshals from the formats
// markdown-link-check accepts via the npm `ms` package: a Go-style duration
// string ("5s", "2000ms", "1m"), a bare number of milliseconds (JSON number
// 5000 or string "5000"), or empty/null (leaving Set false). This is what makes
// a tcort JSON config with `"timeout": "20s"` or `"fallbackRetryDelay": 60000`
// parse unchanged.
type Duration struct {
	D   time.Duration
	Set bool
}

// NewDuration wraps a concrete duration as an explicitly-set value.
func NewDuration(d time.Duration) Duration { return Duration{D: d, Set: true} }

// Or returns the wrapped duration when set, otherwise def.
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Set {
		return d.D
	}
	return def
}

// UnmarshalJSON implements json.Unmarshaler. sigs.k8s.io/yaml routes YAML config
// through JSON, so this single method covers both config dialects.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		return nil
	}
	if s[0] != '"' {
		// bare JSON number → milliseconds (ms() semantics)
		ms, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("invalid duration number %q: %w", s, err)
		}
		d.D = time.Duration(ms * float64(time.Millisecond))
		d.Set = true
		return nil
	}
	str := strings.TrimSpace(strings.Trim(s, `"`))
	if str == "" {
		return nil
	}
	if dur, err := time.ParseDuration(str); err == nil {
		d.D = dur
		d.Set = true
		return nil
	}
	if ms, err := strconv.ParseFloat(str, 64); err == nil {
		d.D = time.Duration(ms * float64(time.Millisecond))
		d.Set = true
		return nil
	}
	return fmt.Errorf("invalid duration %q", str)
}

// MarshalJSON renders the duration as a Go-style string so round-tripped native
// configs stay readable.
func (d Duration) MarshalJSON() ([]byte, error) {
	if !d.Set {
		return []byte("null"), nil
	}
	return []byte(strconv.Quote(d.D.String())), nil
}
