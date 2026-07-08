package report

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// TestSummaryCountsCacheAndReuse guards the counts that back the "N from cache /
// K reused" reporting: FromCache and Reused results are tallied separately.
func TestSummaryCountsCacheAndReuse(t *testing.T) {
	results := []model.Result{
		{State: model.StateAlive, FromCache: true},
		{State: model.StateAlive, FromCache: true},
		{State: model.StateAlive, Reused: true},
		{State: model.StateAlive},
		{State: model.StateDead},
	}
	s := summarize(results, nil, false)
	assert.Equal(t, 5, s.Total)
	assert.Equal(t, 2, s.Cached)
	assert.Equal(t, 1, s.Reused)
}

// TestRenderReportsCacheHits guards that cache-validated links are reported both
// per-link ("(cached)") and in the summary ("N from cache").
func TestRenderReportsCacheHits(t *testing.T) {
	cached := func(url string) model.Result {
		return model.Result{
			Target:     model.Target{SourceFile: "a.md", Line: 1, URL: url, Kind: model.KindHTTP},
			State:      model.StateAlive,
			StatusCode: 200,
			FromCache:  true,
		}
	}
	buf, s := collect(t, config.Resolved{}, Options{}, nil, cached("http://x.example"), cached("http://y.example"))

	assert.Equal(t, 2, s.Cached)
	out := buf.String()
	assert.Contains(t, out, "(cached)", "per-link cache marker")
	assert.Contains(t, out, "2 from cache", "summary cache count")
	assert.NotContains(t, out, "reused", "no same-run reuse in this case")
	_ = strings.TrimSpace(out)
}
