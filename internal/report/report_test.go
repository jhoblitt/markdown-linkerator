package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

func res(file string, line int, url string, state model.State, code int) model.Result {
	return model.Result{
		Target:     model.Target{URL: url, SourceFile: file, Line: line},
		State:      state,
		StatusCode: code,
	}
}

// collect builds a Collector writing to a fresh buffer, adds the results, and
// finishes. It disables color so assertions run against plain text.
func collect(t *testing.T, cfg config.Resolved, opts Options, hosts []model.HostStat, results ...model.Result) (*bytes.Buffer, Summary) {
	t.Helper()
	buf := &bytes.Buffer{}
	opts.Out = buf
	opts.NoColor = true
	c := NewCollector(cfg, opts)
	for _, r := range results {
		c.Add(r)
	}
	return buf, c.Finish(hosts)
}

func TestAliveOnly(t *testing.T) {
	buf, s := collect(t, config.Resolved{}, Options{},
		nil,
		res("a.md", 1, "http://alive.one", model.StateAlive, 200),
		res("a.md", 2, "http://alive.two", model.StateAlive, 200),
	)

	assert.Equal(t, 2, s.Total)
	assert.Equal(t, 2, s.Alive)
	assert.Equal(t, 0, s.ExitCode)

	out := buf.String()
	assert.Contains(t, out, "FILE: a.md")
	assert.Contains(t, out, "http://alive.one")
	assert.Contains(t, out, "http://alive.two")
	assert.Contains(t, out, "2 link(s) checked.")
	assert.NotContains(t, out, "ERROR:")
	assert.NotContains(t, out, "dead")
	assert.Contains(t, out, model.StateAlive.Glyph())
}

func TestDeadLinks(t *testing.T) {
	buf, s := collect(t, config.Resolved{}, Options{},
		nil,
		res("a.md", 1, "http://alive", model.StateAlive, 200),
		res("a.md", 3, "http://dead", model.StateDead, 404),
	)

	assert.Equal(t, 1, s.Dead)
	assert.Equal(t, 1, s.ExitCode)

	out := buf.String()
	assert.Contains(t, out, "2 link(s) checked.")
	assert.Contains(t, out, "ERROR: 1 dead link(s) found!")
	// The dead block re-lists the dead link with its status code.
	assert.Contains(t, out, "http://dead → Status: 404")
	assert.Contains(t, out, model.StateDead.Glyph())
}

func TestQuietMode(t *testing.T) {
	buf, s := collect(t, config.Resolved{}, Options{Quiet: true},
		nil,
		res("a.md", 1, "http://alive", model.StateAlive, 200),
		res("a.md", 2, "http://dead", model.StateDead, 404),
		res("b.md", 1, "http://fine", model.StateAlive, 200),
	)

	assert.Equal(t, 1, s.ExitCode) // exit code is unaffected by quiet

	out := buf.String()
	// Only the failing file/link is shown.
	assert.Contains(t, out, "FILE: a.md")
	assert.Contains(t, out, "http://dead")
	assert.NotContains(t, out, "http://alive")
	assert.NotContains(t, out, "http://fine")
	assert.NotContains(t, out, "FILE: b.md")
	// No summary chrome in quiet mode.
	assert.NotContains(t, out, "link(s) checked.")
	assert.NotContains(t, out, "ERROR:")
}

func TestQuietModeNoFailuresIsSilent(t *testing.T) {
	buf, s := collect(t, config.Resolved{}, Options{Quiet: true},
		nil,
		res("a.md", 1, "http://alive", model.StateAlive, 200),
	)
	assert.Equal(t, 0, s.ExitCode)
	assert.Empty(t, buf.String())
}

func TestVerbose(t *testing.T) {
	buf, _ := collect(t, config.Resolved{}, Options{Verbose: true},
		nil,
		res("a.md", 1, "http://alive", model.StateAlive, 200),
		func() model.Result {
			r := res("a.md", 2, "http://dead", model.StateDead, 404)
			r.Detail = "not found"
			return r
		}(),
	)

	out := buf.String()
	assert.Contains(t, out, "http://alive → Status: 200")
	assert.Contains(t, out, "http://dead → Status: 404")
	assert.Contains(t, out, "not found")
}

func TestErrorState(t *testing.T) {
	errored := func() model.Result {
		r := res("a.md", 1, "ldap://x", model.StateError, 0)
		r.Err = errors.New("unsupported protocol")
		return r
	}

	cases := []struct {
		name          string
		errorFailsRun bool
		wantExit      int
	}{
		{"error-does-not-fail", false, 0},
		{"error-fails-run", true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, s := collect(t, config.Resolved{ErrorFailsRun: tc.errorFailsRun}, Options{Verbose: true},
				nil, errored())
			assert.Equal(t, 1, s.Errored)
			assert.Equal(t, tc.wantExit, s.ExitCode)
			// Errored links carry no dead-link block regardless of exit code.
			assert.NotContains(t, buf.String(), "dead link(s) found")
			assert.Contains(t, buf.String(), "unsupported protocol")
		})
	}
}

func TestEmptyRun(t *testing.T) {
	buf, s := collect(t, config.Resolved{}, Options{}, nil)
	assert.Equal(t, 0, s.Total)
	assert.Equal(t, 0, s.ExitCode)
	assert.Equal(t, "No hyperlinks found!\n", buf.String())
}

func TestEmptyRunQuietSilent(t *testing.T) {
	buf, _ := collect(t, config.Resolved{}, Options{Quiet: true}, nil)
	assert.Empty(t, buf.String())
}

func TestHostSection(t *testing.T) {
	hosts := []model.HostStat{
		{Host: "z.example.com", Requests: 5, Retries: 1, N429: 0, ObservedRPS: 0.5},
		{Host: "docs.ceph.com", Requests: 47, Retries: 3, N429: 0, ObservedRPS: 0.94},
	}
	buf, s := collect(t, config.Resolved{}, Options{}, hosts,
		res("a.md", 1, "http://alive", model.StateAlive, 200),
	)

	require.Len(t, s.Hosts, 2)
	// Hosts are sorted by name.
	assert.Equal(t, "docs.ceph.com", s.Hosts[0].Host)
	assert.Equal(t, "z.example.com", s.Hosts[1].Host)

	out := buf.String()
	assert.Contains(t, out, "docs.ceph.com  47 requests  0.94 req/s  3 retries  0 unresolved-429")
	assert.Less(t, strings.Index(out, "docs.ceph.com"), strings.Index(out, "z.example.com"))
}

func TestHostSectionSuppressedWhenQuiet(t *testing.T) {
	hosts := []model.HostStat{{Host: "docs.ceph.com", Requests: 1, ObservedRPS: 1}}
	buf, _ := collect(t, config.Resolved{}, Options{Quiet: true}, hosts,
		res("a.md", 1, "http://dead", model.StateDead, 500),
	)
	assert.NotContains(t, buf.String(), "docs.ceph.com")
}

func TestJSONFormat(t *testing.T) {
	hosts := []model.HostStat{{Host: "h.example.com", Requests: 3, Retries: 1, N429: 2, ObservedRPS: 1.5}}
	buf, s := collect(t, config.Resolved{ErrorFailsRun: true}, Options{Format: "json", Quiet: true},
		hosts,
		res("b.md", 2, "http://alive", model.StateAlive, 200),
		res("a.md", 9, "http://dead", model.StateDead, 404),
		res("a.md", 1, "http://ignored", model.StateIgnored, 0),
	)

	raw := buf.Bytes()
	require.True(t, json.Valid(raw), "output must be valid JSON")
	// json mode is never decorated regardless of quiet/verbose.
	assert.NotContains(t, buf.String(), "FILE:")
	assert.NotContains(t, buf.String(), "link(s) checked")

	var got struct {
		Total    int `json:"total"`
		Alive    int `json:"alive"`
		Dead     int `json:"dead"`
		Ignored  int `json:"ignored"`
		Errored  int `json:"errored"`
		ExitCode int `json:"exitCode"`
		Results  []struct {
			File       string `json:"file"`
			Line       int    `json:"line"`
			URL        string `json:"url"`
			State      string `json:"state"`
			StatusCode int    `json:"statusCode"`
			Detail     string `json:"detail"`
		} `json:"results"`
		Hosts []struct {
			Host        string  `json:"host"`
			Requests    int64   `json:"requests"`
			ObservedRPS float64 `json:"observedRps"`
		} `json:"hosts"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, s.Total, got.Total)
	assert.Equal(t, 3, got.Total)
	assert.Equal(t, 1, got.Alive)
	assert.Equal(t, 1, got.Dead)
	assert.Equal(t, 1, got.Ignored)
	assert.Equal(t, 1, got.ExitCode)
	require.Len(t, got.Results, 3)
	// Results are sorted (a.md line 1, a.md line 9, b.md line 2).
	assert.Equal(t, "a.md", got.Results[0].File)
	assert.Equal(t, 1, got.Results[0].Line)
	assert.Equal(t, "ignored", got.Results[0].State)
	assert.Equal(t, "dead", got.Results[1].State)
	assert.Equal(t, 404, got.Results[1].StatusCode)
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, "h.example.com", got.Hosts[0].Host)
	assert.Equal(t, 1.5, got.Hosts[0].ObservedRPS)
}

func TestDeterministicSort(t *testing.T) {
	base := []model.Result{
		res("b.md", 1, "http://b1", model.StateAlive, 200),
		res("a.md", 10, "http://a10", model.StateAlive, 200),
		res("a.md", 2, "http://a2b", model.StateAlive, 200),
		res("a.md", 2, "http://a2a", model.StateDead, 404),
		res("c.md", 1, "http://c1", model.StateAlive, 200),
	}

	render := func(seed int64) (string, []model.Result) {
		shuffled := make([]model.Result, len(base))
		copy(shuffled, base)
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		buf, s := collect(t, config.Resolved{}, Options{}, nil, shuffled...)
		return buf.String(), s.Results
	}

	out1, results := render(1)
	out2, _ := render(99)
	assert.Equal(t, out1, out2, "rendered output must be independent of Add order")

	want := []struct {
		file string
		line int
		url  string
	}{
		{"a.md", 2, "http://a2a"},
		{"a.md", 2, "http://a2b"},
		{"a.md", 10, "http://a10"},
		{"b.md", 1, "http://b1"},
		{"c.md", 1, "http://c1"},
	}
	require.Len(t, results, len(want))
	for i, w := range want {
		assert.Equal(t, w.file, results[i].Target.SourceFile, "result %d file", i)
		assert.Equal(t, w.line, results[i].Target.Line, "result %d line", i)
		assert.Equal(t, w.url, results[i].Target.URL, "result %d url", i)
	}
}

func TestColorHonorsNoColor(t *testing.T) {
	// NO_COLOR unset so the palette can enable color when NoColor is false.
	t.Setenv("NO_COLOR", "")

	withColor := &bytes.Buffer{}
	c := NewCollector(config.Resolved{}, Options{Out: withColor, NoColor: false})
	c.Add(res("a.md", 1, "http://dead", model.StateDead, 404))
	c.Finish(nil)
	assert.Contains(t, withColor.String(), "\x1b[", "color enabled should emit ANSI codes")

	noColor := &bytes.Buffer{}
	c = NewCollector(config.Resolved{}, Options{Out: noColor, NoColor: true})
	c.Add(res("a.md", 1, "http://dead", model.StateDead, 404))
	c.Finish(nil)
	assert.NotContains(t, noColor.String(), "\x1b[", "NoColor should suppress ANSI codes")
}

func TestColorHonorsNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	buf := &bytes.Buffer{}
	c := NewCollector(config.Resolved{}, Options{Out: buf, NoColor: false})
	c.Add(res("a.md", 1, "http://dead", model.StateDead, 404))
	c.Finish(nil)
	assert.NotContains(t, buf.String(), "\x1b[", "NO_COLOR env should suppress ANSI codes")
}

func TestNilOutIsSafe(t *testing.T) {
	c := NewCollector(config.Resolved{}, Options{Out: nil})
	c.Add(res("a.md", 1, "http://alive", model.StateAlive, 200))
	assert.NotPanics(t, func() { c.Finish(nil) })
}

func TestAddConcurrent(t *testing.T) {
	c := NewCollector(config.Resolved{}, Options{Out: io.Discard, NoColor: true})
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			state := model.StateAlive
			if i%5 == 0 {
				state = model.StateDead
			}
			c.Add(res("f.md", i, fmt.Sprintf("http://x/%d", i), state, 200))
		}(i)
	}
	wg.Wait()

	s := c.Finish(nil)
	assert.Equal(t, n, s.Total)
	assert.Equal(t, n/5, s.Dead)
	assert.Equal(t, 1, s.ExitCode)
}
