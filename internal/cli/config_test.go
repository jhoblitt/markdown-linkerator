package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAliveCodesAdditive guards that --alive extends the alive set rather than
// replacing it: a healthy 200 must survive when a user adds 206/429.
func TestAliveCodesAdditive(t *testing.T) {
	t.Setenv("LINKERATOR_ALIVE_CODES", "")
	cmd, _ := NewRootCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--alive", "206,429"}))
	r, err := buildResolved(cmd)
	require.NoError(t, err)
	assert.True(t, r.AliveStatusCodes[200], "default 200 must survive --alive")
	assert.True(t, r.AliveStatusCodes[206])
	assert.True(t, r.AliveStatusCodes[429])
}

// TestAliveCodesEnvAdditive covers the same additive contract via the env var.
func TestAliveCodesEnvAdditive(t *testing.T) {
	t.Setenv("LINKERATOR_ALIVE_CODES", "206")
	cmd, _ := NewRootCmd()
	require.NoError(t, cmd.ParseFlags(nil))
	r, err := buildResolved(cmd)
	require.NoError(t, err)
	assert.True(t, r.AliveStatusCodes[200])
	assert.True(t, r.AliveStatusCodes[206])
}

func TestAliveCodesDefault(t *testing.T) {
	t.Setenv("LINKERATOR_ALIVE_CODES", "")
	cmd, _ := NewRootCmd()
	require.NoError(t, cmd.ParseFlags(nil))
	r, err := buildResolved(cmd)
	require.NoError(t, err)
	assert.True(t, r.AliveStatusCodes[200])
	assert.False(t, r.AliveStatusCodes[206])
}
