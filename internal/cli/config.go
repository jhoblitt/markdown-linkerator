package cli

import (
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
)

// resolveConfig builds the merged configuration in precedence order:
// defaults (applied later by Resolve) < file < env < flags.
func resolveConfig(cmd *cobra.Command) (config.Config, error) {
	path := stringValue(cmd, "config", "LINKERATOR_CONFIG", "")
	if path == "" {
		path = autoDetectConfig()
	}
	var cfg config.Config
	if path != "" {
		fileCfg, err := config.Load(path)
		if err != nil {
			return config.Config{}, err
		}
		cfg = fileCfg
	}
	cfg.Merge(configFromEnv())
	cfg.Merge(configFromFlags(cmd))
	return cfg, nil
}

func configFromEnv() config.Config {
	var c config.Config
	if v, ok := lookInt("LINKERATOR_WORKERS"); ok {
		c.URLWorkers = v
	}
	if v, ok := lookInt("LINKERATOR_PARSE_WORKERS"); ok {
		c.ParseWorkers = v
	}
	if v, ok := lookFloat("LINKERATOR_RATE"); ok {
		c.PerHostRPS = v
	}
	if v, ok := lookInt("LINKERATOR_BURST"); ok {
		c.PerHostBurst = v
	}
	if v, ok := lookDuration("LINKERATOR_TIMEOUT"); ok {
		c.Timeout = config.NewDuration(v)
	}
	if v, ok := lookBool("LINKERATOR_RETRY_ON_429"); ok {
		c.RetryOn429 = &v
	}
	if v, ok := lookInt("LINKERATOR_RETRY_COUNT"); ok {
		c.RetryCount = &v
	}
	if v, ok := lookDuration("LINKERATOR_RETRY_MAX_WAIT"); ok {
		c.BackoffMax = config.NewDuration(v)
	}
	// LINKERATOR_ALIVE_CODES is applied additively after Resolve (see aliveExtra).
	if v, ok := lookBool("LINKERATOR_CHECK_EXTERNAL"); ok {
		c.CheckExternal = &v
	}
	if v, ok := lookBool("LINKERATOR_CHECK_FRAGMENTS"); ok {
		c.CheckFragments = &v
	}
	if v, ok := lookBool("LINKERATOR_MAILTO_CHECK_MX"); ok {
		c.MailtoCheckMX = v
	}
	if v, ok := lookString("LINKERATOR_BASE_URL"); ok {
		c.ProjectBaseURL = v
	}
	if v, ok := lookString("LINKERATOR_USER_AGENT"); ok {
		c.UserAgent = v
	}
	// Prefer an explicit LINKERATOR_GITHUB_TOKEN, else fall back to the standard
	// $GITHUB_TOKEN that GitHub Actions injects, so auth is automatic in CI.
	if v, ok := lookString("LINKERATOR_GITHUB_TOKEN"); ok {
		c.GitHubToken = v
	} else if v, ok := lookString("GITHUB_TOKEN"); ok {
		c.GitHubToken = v
	}
	if v, ok := lookInt("LINKERATOR_MAX_REDIRECTS"); ok {
		c.MaxRedirects = &v
	}
	if v, ok := lookBool("LINKERATOR_CACHE"); ok {
		c.Cache.Enabled = &v
	}
	if v, ok := lookString("LINKERATOR_CACHE_PATH"); ok {
		c.Cache.Path = v
	}
	if v, ok := lookDuration("LINKERATOR_CACHE_TTL"); ok {
		c.Cache.TTL = config.NewDuration(v)
	}
	if v, ok := lookBool("LINKERATOR_FAIL_ON_ERROR"); ok {
		c.ErrorFailsRun = v
	}
	return c
}

func configFromFlags(cmd *cobra.Command) config.Config {
	var c config.Config
	fs := cmd.Flags()
	if fs.Changed("workers") {
		c.URLWorkers, _ = fs.GetInt("workers")
	}
	if fs.Changed("parse-workers") {
		c.ParseWorkers, _ = fs.GetInt("parse-workers")
	}
	if fs.Changed("rate") {
		c.PerHostRPS, _ = fs.GetFloat64("rate")
	}
	if fs.Changed("burst") {
		c.PerHostBurst, _ = fs.GetInt("burst")
	}
	if fs.Changed("timeout") {
		d, _ := fs.GetDuration("timeout")
		c.Timeout = config.NewDuration(d)
	}
	// --retry-on-429 is canonical; -r/--retry is a shorthand to enable it.
	switch {
	case fs.Changed("retry-on-429"):
		v, _ := fs.GetBool("retry-on-429")
		c.RetryOn429 = &v
	case fs.Changed("retry"):
		v, _ := fs.GetBool("retry")
		c.RetryOn429 = &v
	}
	if fs.Changed("retry-count") {
		v, _ := fs.GetInt("retry-count")
		c.RetryCount = &v
	}
	if fs.Changed("retry-max-wait") {
		d, _ := fs.GetDuration("retry-max-wait")
		c.BackoffMax = config.NewDuration(d)
	}
	// --alive is applied additively after Resolve (see aliveExtra).
	if fs.Changed("check-external") {
		v, _ := fs.GetBool("check-external")
		c.CheckExternal = &v
	}
	if fs.Changed("check-fragments") {
		v, _ := fs.GetBool("check-fragments")
		c.CheckFragments = &v
	}
	if fs.Changed("mailto-check-mx") {
		c.MailtoCheckMX, _ = fs.GetBool("mailto-check-mx")
	}
	if fs.Changed("project-base-url") {
		c.ProjectBaseURL, _ = fs.GetString("project-base-url")
	}
	if fs.Changed("user-agent") {
		c.UserAgent, _ = fs.GetString("user-agent")
	}
	if fs.Changed("github-token") {
		c.GitHubToken, _ = fs.GetString("github-token")
	}
	if fs.Changed("max-redirects") {
		v, _ := fs.GetInt("max-redirects")
		c.MaxRedirects = &v
	}
	if fs.Changed("cache") {
		v, _ := fs.GetBool("cache")
		c.Cache.Enabled = &v
	}
	if fs.Changed("cache-path") {
		c.Cache.Path, _ = fs.GetString("cache-path")
	}
	if fs.Changed("cache-ttl") {
		d, _ := fs.GetDuration("cache-ttl")
		c.Cache.TTL = config.NewDuration(d)
	}
	if fs.Changed("fail-on-error") {
		c.ErrorFailsRun, _ = fs.GetBool("fail-on-error")
	}
	return c
}

// buildResolved resolves the full configuration (defaults < file < env < flags)
// and applies the additive alive-code codes on top.
func buildResolved(cmd *cobra.Command) (config.Resolved, error) {
	cfg, err := resolveConfig(cmd)
	if err != nil {
		return config.Resolved{}, err
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		return config.Resolved{}, err
	}
	// --alive / LINKERATOR_ALIVE_CODES are "extra" codes: they extend the alive
	// set (which already contains 200 or the config's list), never replace it —
	// so `--alive 206` cannot turn a healthy 200 into a failure.
	for _, code := range aliveExtra(cmd) {
		resolved.AliveStatusCodes[code] = true
	}
	return resolved, nil
}

// aliveExtra returns the additive alive codes from the --alive flag or the
// LINKERATOR_ALIVE_CODES env var (flag wins).
func aliveExtra(cmd *cobra.Command) []int {
	if cmd.Flags().Changed("alive") {
		s, _ := cmd.Flags().GetString("alive")
		return csvInts(s)
	}
	if v, ok := lookString("LINKERATOR_ALIVE_CODES"); ok {
		return csvInts(v)
	}
	return nil
}

// boolValue resolves a bool report option: an explicitly-set flag wins, then the
// env var, then the flag default.
func boolValue(cmd *cobra.Command, flag, env string) bool {
	if cmd.Flags().Changed(flag) {
		v, _ := cmd.Flags().GetBool(flag)
		return v
	}
	if v, ok := lookBool(env); ok {
		return v
	}
	v, _ := cmd.Flags().GetBool(flag)
	return v
}

func stringValue(cmd *cobra.Command, flag, env, def string) string {
	if cmd.Flags().Changed(flag) {
		v, _ := cmd.Flags().GetString(flag)
		return v
	}
	if env != "" {
		if v, ok := lookString(env); ok {
			return v
		}
	}
	if v, _ := cmd.Flags().GetString(flag); v != "" {
		return v
	}
	return def
}

// durationValue resolves a duration option: an explicitly-set flag wins, then
// the env var, then the flag default.
func durationValue(cmd *cobra.Command, flag, env string) time.Duration {
	if cmd.Flags().Changed(flag) {
		d, _ := cmd.Flags().GetDuration(flag)
		return d
	}
	if v, ok := lookDuration(env); ok {
		return v
	}
	d, _ := cmd.Flags().GetDuration(flag)
	return d
}

func lookString(k string) (string, bool) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func lookInt(k string) (int, bool) {
	if s, ok := lookString(k); ok {
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
	}
	return 0, false
}

func lookFloat(k string) (float64, bool) {
	if s, ok := lookString(k); ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func lookBool(k string) (bool, bool) {
	if s, ok := lookString(k); ok {
		if b, err := strconv.ParseBool(s); err == nil {
			return b, true
		}
	}
	return false, false
}

func lookDuration(k string) (time.Duration, bool) {
	if s, ok := lookString(k); ok {
		if d, err := time.ParseDuration(s); err == nil {
			return d, true
		}
	}
	return 0, false
}
