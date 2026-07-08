// Package cli builds the cobra command: it defines flags, binds LINKERATOR_*
// environment variables, resolves configuration precedence
// (defaults < file < env < flags), and drives engine.Run.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/markdown-linkerator/internal/engine"
	"github.com/jhoblitt/markdown-linkerator/internal/report"
	"github.com/jhoblitt/markdown-linkerator/internal/version"
)

// NewRootCmd builds the root command and returns a pointer to the exit code it
// will set (main reads it after Execute; a returned error means exit 2).
func NewRootCmd() (*cobra.Command, *int) {
	exitCode := new(int)
	cmd := &cobra.Command{
		Use:   "markdown-linkerator [flags] [files|dirs|globs|urls...]",
		Short: "Fast, 429-safe Markdown link checker",
		Long: "markdown-linkerator crawls a markdown tree and checks its links with " +
			"per-host rate limiting, worker pools, an optional cache, and graceful 429 backoff.",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), cmd, args, exitCode)
		},
	}
	cmd.SetVersionTemplate("{{.Version}}\n")
	registerFlags(cmd)
	return cmd, exitCode
}

func registerFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringP("config", "c", "", "config file (.json or .yaml); auto-detected if empty")
	f.Int("workers", 10, "concurrent URL-check workers")
	f.Int("parse-workers", 10, "concurrent markdown-parse workers")
	f.Float64("rate", 1, "per-host requests per second")
	f.Int("burst", 2, "per-host burst")
	f.Duration("timeout", 10*time.Second, "per-request timeout")
	f.Duration("max-time", 0, "maximum total run time, e.g. 5m (0 = no limit)")
	f.Bool("retry-on-429", true, "retry on HTTP 429 honoring Retry-After")
	f.BoolP("retry", "r", true, "alias for --retry-on-429")
	f.Int("retry-count", 4, "max retries per URL on 429/503")
	f.Int("connect-retries", 3, "retries on a connection failure before giving up")
	f.Duration("retry-max-wait", 2*time.Minute, "cap on the Retry-After wait")
	f.StringP("alive", "a", "", "extra alive HTTP status codes (comma-separated)")
	f.Bool("check-external", true, "check http(s) links (false = offline)")
	f.Bool("check-fragments", true, "validate cross-file #anchors against the target file's anchors")
	f.Bool("mailto-check-mx", false, "live MX lookup for mailto links")
	f.String("project-base-url", "", "base URL for root-relative links ({{BASEURL}})")
	f.String("user-agent", "", "User-Agent header")
	f.String("github-token", "", "token for authenticated requests to GitHub hosts (defaults to $GITHUB_TOKEN)")
	f.Int("max-redirects", 8, "maximum redirects to follow")
	f.Bool("cache", false, "enable the on-disk result cache")
	f.String("cache-path", ".linkerator-cache.json", "cache file path")
	f.Duration("cache-ttl", 24*time.Hour, "cache entry TTL")
	f.Bool("fail-on-error", false, "treat errored links as failures")
	f.BoolP("quiet", "q", false, "print only failing links")
	f.BoolP("verbose", "v", false, "show status codes and detail")
	f.BoolP("progress", "p", false, "show a progress indicator")
	f.String("format", "text", "report format: text, json, or yaml")
	f.Bool("no-color", false, "disable ANSI color")
	f.String("report-json", "", "also write a JSON summary to this path")
}

func run(ctx context.Context, cmd *cobra.Command, args []string, exitCode *int) error {
	resolved, err := buildResolved(cmd)
	if err != nil {
		return err
	}

	rep := report.Options{
		Quiet:       boolValue(cmd, "quiet", "LINKERATOR_QUIET"),
		Verbose:     boolValue(cmd, "verbose", "LINKERATOR_VERBOSE"),
		Progress:    boolValue(cmd, "progress", "LINKERATOR_PROGRESS"),
		Format:      stringValue(cmd, "format", "LINKERATOR_FORMAT", "text"),
		NoColor:     boolValue(cmd, "no-color", "LINKERATOR_NO_COLOR"),
		Out:         cmd.OutOrStdout(),
		ProgressOut: cmd.ErrOrStderr(),
	}

	// --max-time bounds the whole run: on expiry the context cancels, in-flight
	// checks resolve as errors, and the run reports what it has with exit 2.
	if d := durationValue(cmd, "max-time", "LINKERATOR_MAX_TIME"); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	summary, runErr := engine.Run(ctx, resolved, args, rep)

	if summary != nil {
		if path := stringValue(cmd, "report-json", "", ""); path != "" {
			if werr := writeReportJSON(path, summary); werr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", werr)
			}
		}
	}

	if runErr != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			*exitCode = 2
			return fmt.Errorf("max-time exceeded: %w", runErr)
		case ctx.Err() != nil:
			*exitCode = 2
			return fmt.Errorf("interrupted: %w", runErr)
		default:
			return runErr
		}
	}
	*exitCode = summary.ExitCode
	return nil
}

func writeReportJSON(path string, s *report.Summary) error {
	out := struct {
		Total    int `json:"total"`
		Alive    int `json:"alive"`
		Dead     int `json:"dead"`
		Ignored  int `json:"ignored"`
		Errored  int `json:"errored"`
		Cached   int `json:"cached"`
		Reused   int `json:"reused"`
		ExitCode int `json:"exitCode"`
	}{s.Total, s.Alive, s.Dead, s.Ignored, s.Errored, s.Cached, s.Reused, s.ExitCode}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func versionString() string {
	v, commit, date := version.Info()
	return fmt.Sprintf("markdown-linkerator %s (%s, %s)", v, shortCommit(commit), date)
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// autoDetectConfig looks for a config file in the working directory.
func autoDetectConfig() string {
	for _, name := range []string{".markdown-link-check.json", "linkerator.yaml", "linkerator.yml", ".linkerator.yaml"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	return ""
}

func csvInts(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	return out
}

var _ = filepath.Base // retained for future path handling
