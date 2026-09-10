package main

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file exercises the vdclient Cobra command layer (newRootCmd,
// normalizeArgs, exitCode) with testify assert/require, covering happy-path
// flag parsing and CLI-level error cases.

func TestVdclientNormalizeArgsRewritesSingleDashLongFlags(t *testing.T) {
	got := normalizeArgs([]string{"-addr", "example.com:1", "--already-long", "-x", "pos"})
	assert.Equal(t, []string{"--addr", "example.com:1", "--already-long", "-x", "pos"}, got)
}

func TestVdclientExitCodeMapsFlagErrorsToTwoAndOthersToOne(t *testing.T) {
	assert.Equal(t, 2, exitCode(&flagError{errors.New("bad flag")}))
	assert.Equal(t, 1, exitCode(errors.New("some runtime failure")))
}

// TestVdclientNewRootCmdParsesKnownFlagsHappyPath proves the root command
// accepts a full set of valid flags without error.
func TestVdclientNewRootCmdParsesKnownFlagsHappyPath(t *testing.T) {
	cmd := newRootCmd()
	cmd.RunE = nil
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--addr", "example.com:6511", "--allow-insecure"})
	require.NoError(t, cmd.Execute())

	addr, err := cmd.Flags().GetString("addr")
	require.NoError(t, err)
	assert.Equal(t, "example.com:6511", addr)

	allowInsecure, err := cmd.Flags().GetBool("allow-insecure")
	require.NoError(t, err)
	assert.True(t, allowInsecure)
}

// TestVdclientNewRootCmdRejectsUnknownFlag proves an unrecognized flag
// surfaces as a flagError (mapping to exit code 2).
func TestVdclientNewRootCmdRejectsUnknownFlag(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--no-such-flag"})
	err := cmd.Execute()
	require.Error(t, err)
	var fe *flagError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, 2, exitCode(err))
}

// TestVdclientLoadConfigRejectsMissingTrustMaterial proves the CLI-level
// error path when neither -allow-insecure, -fingerprint, nor -ca is given.
func TestVdclientLoadConfigRejectsMissingTrustMaterial(t *testing.T) {
	_, err := loadConfig([]string{"-addr", "example.com:6511"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow-insecure")
}
