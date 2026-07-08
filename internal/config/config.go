// Package config defines the linkerator configuration: one struct whose json
// tags are the tcort markdown-link-check keys, so the same struct parses a
// drop-in `.markdown-link-check.json` and a native `linkerator.yaml`
// (sigs.k8s.io/yaml routes YAML through JSON). Precedence is resolved by the
// caller as defaults < file < env < flags via Merge.
package config

import (
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// Pattern is a single regex entry (tcort `ignorePatterns`).
type Pattern struct {
	Pattern string `json:"pattern"`
}

// ReplacementPattern rewrites a link before it is checked (tcort
// `replacementPatterns`). Global applies the regex globally.
type ReplacementPattern struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
	Global      bool   `json:"global,omitempty"`
}

// HeaderRule attaches headers to links whose (post-replacement) URL starts with
// any of URLs (tcort `httpHeaders`). Values support {{env.NAME}} expansion.
type HeaderRule struct {
	URLs    []string          `json:"urls"`
	Headers map[string]string `json:"headers"`
}

// HostLimit overrides the rate for a specific host (native `hostOverrides`).
type HostLimit struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
}

// CacheConfig configures the on-disk URL result cache (native `cache`).
type CacheConfig struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Path    string   `json:"path,omitempty"`
	TTL     Duration `json:"ttl,omitempty"`
}

// Config is the full configuration. Tri-state fields (where a zero value is a
// meaningful non-default) use pointers so Merge can tell "unset" from "set to
// zero"; Resolve() collapses them to concrete accessors.
type Config struct {
	// --- parity keys (read verbatim from tcort markdown-link-check JSON) ---
	IgnorePatterns      []Pattern            `json:"ignorePatterns,omitempty"`
	ReplacementPatterns []ReplacementPattern `json:"replacementPatterns,omitempty"`
	HTTPHeaders         []HeaderRule         `json:"httpHeaders,omitempty"`
	AliveStatusCodes    []int                `json:"aliveStatusCodes,omitempty"`
	Timeout             Duration             `json:"timeout,omitempty"`
	RetryOn429          *bool                `json:"retryOn429,omitempty"`
	RetryCount          *int                 `json:"retryCount,omitempty"`
	FallbackRetryDelay  Duration             `json:"fallbackRetryDelay,omitempty"`
	IgnoreDisable       bool                 `json:"ignoreDisable,omitempty"`
	ProjectBaseURL      string               `json:"projectBaseUrl,omitempty"`

	// --- extended native keys ---
	PerHostRPS    float64              `json:"perHostRPS,omitempty"`
	PerHostBurst  int                  `json:"perHostBurst,omitempty"`
	HostOverrides map[string]HostLimit `json:"hostOverrides,omitempty"`
	URLWorkers    int                  `json:"urlWorkers,omitempty"`
	ParseWorkers  int                  `json:"parseWorkers,omitempty"`
	MaxRetries    int                  `json:"maxRetries,omitempty"`
	BackoffMax    Duration             `json:"backoffMax,omitempty"`
	UserAgent     string               `json:"userAgent,omitempty"`
	MaxRedirects  *int                 `json:"maxRedirects,omitempty"`
	MailtoCheckMX bool                 `json:"mailtoCheckMX,omitempty"`
	ErrorFailsRun bool                 `json:"errorFailsRun,omitempty"`
	CheckExternal *bool                `json:"checkExternal,omitempty"`
	// GitHubToken authenticates requests to GitHub hosts so a CI run is not
	// throttled by the 60/hr unauthenticated limit. Never read from a config
	// file (json:"-"); supply it via flag/env (defaulting to $GITHUB_TOKEN).
	GitHubToken string `json:"-"`
	// CheckFragments validates cross-file #anchors against the target file's
	// anchors (stricter than markdown-link-check, which checks only existence).
	CheckFragments *bool       `json:"checkFragments,omitempty"`
	Cache          CacheConfig `json:"cache,omitempty"`
}

// DefaultUserAgent identifies the tool to servers.
const DefaultUserAgent = "markdown-linkerator (+https://github.com/jhoblitt/markdown-linkerator)"

// Defaults returns a fully-populated Config with the shipped conservative
// defaults: ~1 req/s per host, 10+10 workers, 24h cache TTL.
func Defaults() Config {
	true_ := true
	false_ := false
	eight := 8
	four := 4
	return Config{
		AliveStatusCodes:   []int{200},
		Timeout:            NewDuration(10 * time.Second),
		RetryOn429:         &true_,
		RetryCount:         &four,
		FallbackRetryDelay: NewDuration(30 * time.Second),
		PerHostRPS:         1,
		PerHostBurst:       2,
		URLWorkers:         10,
		ParseWorkers:       10,
		BackoffMax:         NewDuration(2 * time.Minute),
		UserAgent:          DefaultUserAgent,
		MaxRedirects:       &eight,
		CheckExternal:      &true_,
		CheckFragments:     &true_,
		Cache: CacheConfig{
			Enabled: &false_,
			Path:    ".linkerator-cache.json",
			TTL:     NewDuration(24 * time.Hour),
		},
	}
}

// Load reads and parses a config file. sigs.k8s.io/yaml handles both JSON and
// YAML, so the extension is irrelevant. The returned Config carries only the
// keys present in the file; callers Merge it onto Defaults().
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// Merge overlays the set fields of src onto c, in place. A field is "set" when
// it is non-zero (slices/maps/strings/numbers) or non-nil (pointers). This is
// how precedence layers stack: start from Defaults(), Merge the file, Merge env,
// Merge flags.
func (c *Config) Merge(src Config) {
	if src.IgnorePatterns != nil {
		c.IgnorePatterns = src.IgnorePatterns
	}
	if src.ReplacementPatterns != nil {
		c.ReplacementPatterns = src.ReplacementPatterns
	}
	if src.HTTPHeaders != nil {
		c.HTTPHeaders = src.HTTPHeaders
	}
	if src.AliveStatusCodes != nil {
		c.AliveStatusCodes = src.AliveStatusCodes
	}
	if src.Timeout.Set {
		c.Timeout = src.Timeout
	}
	if src.RetryOn429 != nil {
		c.RetryOn429 = src.RetryOn429
	}
	if src.RetryCount != nil {
		c.RetryCount = src.RetryCount
	}
	if src.FallbackRetryDelay.Set {
		c.FallbackRetryDelay = src.FallbackRetryDelay
	}
	if src.IgnoreDisable {
		c.IgnoreDisable = true
	}
	if src.ProjectBaseURL != "" {
		c.ProjectBaseURL = src.ProjectBaseURL
	}
	if src.PerHostRPS != 0 {
		c.PerHostRPS = src.PerHostRPS
	}
	if src.PerHostBurst != 0 {
		c.PerHostBurst = src.PerHostBurst
	}
	if src.HostOverrides != nil {
		if c.HostOverrides == nil {
			c.HostOverrides = map[string]HostLimit{}
		}
		for k, v := range src.HostOverrides {
			c.HostOverrides[k] = v
		}
	}
	if src.URLWorkers != 0 {
		c.URLWorkers = src.URLWorkers
	}
	if src.ParseWorkers != 0 {
		c.ParseWorkers = src.ParseWorkers
	}
	if src.MaxRetries != 0 {
		c.MaxRetries = src.MaxRetries
	}
	if src.BackoffMax.Set {
		c.BackoffMax = src.BackoffMax
	}
	if src.UserAgent != "" {
		c.UserAgent = src.UserAgent
	}
	if src.MaxRedirects != nil {
		c.MaxRedirects = src.MaxRedirects
	}
	if src.MailtoCheckMX {
		c.MailtoCheckMX = true
	}
	if src.ErrorFailsRun {
		c.ErrorFailsRun = true
	}
	if src.CheckExternal != nil {
		c.CheckExternal = src.CheckExternal
	}
	if src.CheckFragments != nil {
		c.CheckFragments = src.CheckFragments
	}
	if src.GitHubToken != "" {
		c.GitHubToken = src.GitHubToken
	}
	if src.Cache.Enabled != nil {
		c.Cache.Enabled = src.Cache.Enabled
	}
	if src.Cache.Path != "" {
		c.Cache.Path = src.Cache.Path
	}
	if src.Cache.TTL.Set {
		c.Cache.TTL = src.Cache.TTL
	}
}

// Bool safely dereferences a *bool, returning false for nil.
func Bool(p *bool) bool { return p != nil && *p }

// Int safely dereferences a *int, returning def for nil.
func Int(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}
