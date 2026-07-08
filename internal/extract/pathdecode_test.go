package extract_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/extract"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// TestPercentEncodedFileLink guards that a relative file link with a
// percent-encoded space (e.g. an image "My%20File.png") resolves to the real
// filesystem name with a space, not the literal "%20".
func TestPercentEncodedFileLink(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "My File.png"), []byte("x"), 0o644))

	fl, err := extract.ParseFile(filepath.Join(dir, "doc.md"), []byte("![img](My%20File.png)\n"), config.Resolved{})
	require.NoError(t, err)
	require.Len(t, fl.Targets, 1)
	assert.Equal(t, model.KindFileRel, fl.Targets[0].Kind)
	assert.Truef(t, strings.HasSuffix(fl.Targets[0].URL, string(filepath.Separator)+"My File.png"),
		"path must be percent-decoded, got %s", fl.Targets[0].URL)
}
