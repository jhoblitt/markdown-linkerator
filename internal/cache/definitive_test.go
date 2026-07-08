package cache

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// TestDefinitiveNeverCachesErrors guards that a result carrying an error (e.g. a
// canceled check whose State is the StateAlive zero value) is never cached.
func TestDefinitiveNeverCachesErrors(t *testing.T) {
	withErr := model.Result{State: model.StateAlive, StatusCode: 200, Err: errors.New("canceled")}
	assert.False(t, Definitive(withErr), "a result with an error must not be cached")

	clean := withErr
	clean.Err = nil
	assert.True(t, Definitive(clean), "the same alive result without an error is cacheable")
}
