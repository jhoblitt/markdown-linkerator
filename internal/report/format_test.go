package report

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

func TestYAMLFormat(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollector(config.Resolved{}, Options{Format: "yaml", Out: &buf})
	c.Add(model.Result{Target: model.Target{URL: "https://x/", SourceFile: "a.md", Line: 1}, State: model.StateAlive, StatusCode: 200})
	s := c.Finish(nil)

	assert.Equal(t, 0, s.ExitCode)
	var got map[string]any
	require.NoError(t, yaml.Unmarshal(buf.Bytes(), &got), "output must be valid YAML")
	assert.EqualValues(t, 1, got["total"])
	assert.EqualValues(t, 1, got["alive"])
}

func TestReuseTag(t *testing.T) {
	assert.Equal(t, " (cached)", reuseTag(model.Result{FromCache: true}))
	assert.Equal(t, " (reused)", reuseTag(model.Result{Reused: true}))
	assert.Equal(t, " (cached)", reuseTag(model.Result{FromCache: true, Reused: true}), "cache wins")
	assert.Equal(t, "", reuseTag(model.Result{}))
}

func TestReuseMarkerInText(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollector(config.Resolved{}, Options{Format: "text", Out: &buf})
	c.Add(model.Result{Target: model.Target{URL: "https://x/", SourceFile: "a.md"}, State: model.StateAlive, StatusCode: 200})
	c.Add(model.Result{Target: model.Target{URL: "https://x/", SourceFile: "b.md"}, State: model.StateAlive, StatusCode: 200, Reused: true})
	c.Finish(nil)
	assert.Contains(t, buf.String(), "(reused)")
}
