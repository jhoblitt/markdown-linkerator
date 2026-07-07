package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Resolved is a Config with pointers dereferenced, durations concretized, and
// regexes compiled. The engine builds one and hands it to the leaf packages so
// they never touch the tri-state pointer/Duration machinery.
type Resolved struct {
	IgnorePatterns      []*regexp.Regexp
	ReplacementPatterns []CompiledReplacement
	HTTPHeaders         []HeaderRule
	AliveStatusCodes    map[int]bool
	Timeout             time.Duration
	RetryOn429          bool
	RetryCount          int
	FallbackRetryDelay  time.Duration
	IgnoreDisable       bool
	ProjectBaseURL      string

	PerHostRPS    float64
	PerHostBurst  int
	HostOverrides map[string]HostLimit
	URLWorkers    int
	ParseWorkers  int
	MaxRetries    int
	BackoffMax    time.Duration
	UserAgent     string
	MaxRedirects  int
	MailtoCheckMX bool
	ErrorFailsRun bool
	CheckExternal bool
	Cache         ResolvedCache
}

// CompiledReplacement is a pre-compiled replacementPattern. Replacement retains
// any {{BASEURL}} token (expanded per source file) and $1/$name group refs
// (applied by regexp.ReplaceAllString).
type CompiledReplacement struct {
	Re          *regexp.Regexp
	Replacement string
}

// ResolvedCache is the concretized cache configuration.
type ResolvedCache struct {
	Enabled bool
	Path    string
	TTL     time.Duration
}

// Resolve compiles patterns and collapses the tri-state fields, applying
// defaults for any still-unset value. Env tokens ({{env.NAME}}) in header
// values and replacement strings are expanded here since the environment is
// fixed for the run; {{BASEURL}} is left for per-file expansion in extract.
func (c Config) Resolve() (Resolved, error) {
	d := Defaults()
	d.Merge(c) // ensure any field the caller left unset falls back to a default
	r := Resolved{
		HTTPHeaders:        expandHeaderEnv(d.HTTPHeaders),
		AliveStatusCodes:   codeSet(d.AliveStatusCodes),
		Timeout:            d.Timeout.Or(10 * time.Second),
		RetryOn429:         Bool(d.RetryOn429),
		RetryCount:         Int(d.RetryCount, 4),
		FallbackRetryDelay: d.FallbackRetryDelay.Or(30 * time.Second),
		IgnoreDisable:      d.IgnoreDisable,
		ProjectBaseURL:     d.ProjectBaseURL,
		PerHostRPS:         d.PerHostRPS,
		PerHostBurst:       d.PerHostBurst,
		HostOverrides:      d.HostOverrides,
		URLWorkers:         d.URLWorkers,
		ParseWorkers:       d.ParseWorkers,
		MaxRetries:         resolveMaxRetries(d),
		BackoffMax:         d.BackoffMax.Or(2 * time.Minute),
		UserAgent:          d.UserAgent,
		MaxRedirects:       Int(d.MaxRedirects, 8),
		MailtoCheckMX:      d.MailtoCheckMX,
		ErrorFailsRun:      d.ErrorFailsRun,
		CheckExternal:      Bool(d.CheckExternal),
		Cache: ResolvedCache{
			Enabled: Bool(d.Cache.Enabled),
			Path:    d.Cache.Path,
			TTL:     d.Cache.TTL.Or(24 * time.Hour),
		},
	}
	for _, p := range d.IgnorePatterns {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return Resolved{}, fmt.Errorf("ignorePattern %q: %w", p.Pattern, err)
		}
		r.IgnorePatterns = append(r.IgnorePatterns, re)
	}
	for _, p := range d.ReplacementPatterns {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return Resolved{}, fmt.Errorf("replacementPattern %q: %w", p.Pattern, err)
		}
		r.ReplacementPatterns = append(r.ReplacementPatterns, CompiledReplacement{
			Re:          re,
			Replacement: ExpandEnv(p.Replacement),
		})
	}
	return r, nil
}

// resolveMaxRetries folds the tcort `retryCount` into the retry limit: an
// explicit maxRetries wins, else retryCount, else 4.
func resolveMaxRetries(d Config) int {
	if d.MaxRetries > 0 {
		return d.MaxRetries
	}
	if n := Int(d.RetryCount, 0); n > 0 {
		return n
	}
	return 4
}

func codeSet(codes []int) map[int]bool {
	m := make(map[int]bool, len(codes))
	for _, c := range codes {
		m[c] = true
	}
	if len(m) == 0 {
		m[200] = true
	}
	return m
}

func expandHeaderEnv(rules []HeaderRule) []HeaderRule {
	out := make([]HeaderRule, len(rules))
	for i, ru := range rules {
		nr := HeaderRule{URLs: ru.URLs, Headers: map[string]string{}}
		for k, v := range ru.Headers {
			nr.Headers[k] = ExpandEnv(v)
		}
		out[i] = nr
	}
	return out
}

var envToken = regexp.MustCompile(`\{\{env\.([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// ExpandEnv replaces {{env.NAME}} tokens with the corresponding environment
// variable (empty when unset), mirroring markdown-link-check's special
// replacements.
func ExpandEnv(s string) string {
	return envToken.ReplaceAllStringFunc(s, func(m string) string {
		name := envToken.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
}

// ExpandBaseURL replaces {{BASEURL}} with the given base URL (used per source
// file, where the base differs).
func ExpandBaseURL(s, baseURL string) string {
	return strings.ReplaceAll(s, "{{BASEURL}}", baseURL)
}
