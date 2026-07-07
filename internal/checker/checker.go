// Package checker performs the actual link checks: filesystem, local anchor,
// mailto, and http/https (with rate-aware retry). It is a leaf package driven
// by the executor; a dead link is reported as data on model.Result, never as a
// Go error.
package checker

import (
	"context"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// IsIgnored reports whether url matches any configured ignorePattern. A match
// short-circuits to model.StateIgnored with no request performed.
func IsIgnored(url string, cfg config.Resolved) bool {
	for _, re := range cfg.IgnorePatterns {
		if re.MatchString(url) {
			return true
		}
	}
	return false
}

// CheckFile stats the resolved filesystem path in t.URL. A path that exists is
// reported alive (synthetic 200); a missing/unreadable path is dead (400).
func CheckFile(t model.Target) model.Result {
	res := model.Result{Target: t}
	if _, err := os.Stat(t.URL); err != nil {
		res.State = model.StateDead
		res.StatusCode = 400
		res.Detail = err.Error()
		return res
	}
	res.State = model.StateAlive
	res.StatusCode = 200
	return res
}

// CheckHash resolves t.Fragment against the anchor set collected for
// t.SourceFile. The fragment is percent-decoded and lowercased before lookup so
// an encoded fragment matches the non-encoded, lowercased heading slugs.
func CheckHash(t model.Target, anchors map[string]bool) model.Result {
	res := model.Result{Target: t}
	frag := strings.TrimPrefix(t.Fragment, "#")
	if dec, err := url.PathUnescape(frag); err == nil {
		frag = dec
	}
	frag = strings.ToLower(frag)
	if anchors[frag] {
		res.State = model.StateAlive
		res.StatusCode = 200
	} else {
		res.State = model.StateDead
		res.StatusCode = 404
	}
	return res
}

// CheckMailto validates a mailto: target. By default the check is syntax-only:
// the "mailto:" scheme (case-insensitive) is stripped, any "?headers" suffix is
// dropped, comma-separated recipients are split, and each is parsed with
// net/mail. All valid → alive (200); any invalid → dead (400). When
// cfg.MailtoCheckMX is set, each recipient's domain is additionally required to
// have at least one MX record (looked up under ctx).
func CheckMailto(ctx context.Context, t model.Target, cfg config.Resolved) model.Result {
	res := model.Result{Target: t}
	addr := t.URL
	if len(addr) >= len("mailto:") && strings.EqualFold(addr[:len("mailto:")], "mailto:") {
		addr = addr[len("mailto:"):]
	}
	if i := strings.IndexByte(addr, '?'); i >= 0 {
		addr = addr[:i]
	}

	detail := ""
	valid := true
	for _, rcpt := range strings.Split(addr, ",") {
		rcpt = strings.TrimSpace(rcpt)
		parsed, err := mail.ParseAddress(rcpt)
		if err != nil {
			valid, detail = false, "invalid address "+strconv.Quote(rcpt)
			break
		}
		if cfg.MailtoCheckMX {
			if d := domainOf(parsed.Address); !hasMX(ctx, d) {
				valid, detail = false, "no MX records for "+d
				break
			}
		}
	}

	if valid {
		res.State = model.StateAlive
		res.StatusCode = 200
	} else {
		res.State = model.StateDead
		res.StatusCode = 400
		res.Detail = detail
	}
	return res
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

func hasMX(ctx context.Context, domain string) bool {
	if domain == "" {
		return false
	}
	var r net.Resolver
	mxs, err := r.LookupMX(ctx, domain)
	return err == nil && len(mxs) > 0
}
