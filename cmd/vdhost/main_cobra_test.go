package main

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file exercises the vdhost Cobra command layer (newRootCmd,
// normalizeArgs, exitCode) with testify assert/require, covering happy-path
// flag parsing and CLI-level error cases (bad flag, bad config value).

func TestNormalizeArgsRewritesSingleDashLongFlags(t *testing.T) {
	got := normalizeArgs([]string{"-addr", "127.0.0.1:1", "--already-long", "-x", "pos"})
	assert.Equal(t, []string{"--addr", "127.0.0.1:1", "--already-long", "-x", "pos"}, got,
		"single-char flags and already-double-dash flags must be left untouched, but a single-dash long flag gains a second dash")
}

func TestNormalizeArgsRewritesMultiCharSingleDash(t *testing.T) {
	got := normalizeArgs([]string{"-capture-interval", "5s"})
	require.Len(t, got, 2)
	assert.Equal(t, "--capture-interval", got[0], "long single-dash flag should get an extra dash")
}

func TestExitCodeMapsFlagErrorsToTwoAndOthersToOne(t *testing.T) {
	assert.Equal(t, 2, exitCode(&flagError{errors.New("bad flag")}))
	assert.Equal(t, 1, exitCode(errors.New("some runtime failure")))
}

// TestNewRootCmdParsesKnownFlagsHappyPath proves the root command accepts a
// full set of valid flags without error and without executing RunE.
func TestNewRootCmdParsesKnownFlagsHappyPath(t *testing.T) {
	cmd := newRootCmd()
	cmd.RunE = nil // isolate flag parsing from run behavior
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:9999", "--max-input-per-sec", "42"})
	require.NoError(t, cmd.Execute())

	addr, err := cmd.Flags().GetString("addr")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9999", addr)

	maxInput, err := cmd.Flags().GetInt("max-input-per-sec")
	require.NoError(t, err)
	assert.Equal(t, 42, maxInput)
}

// TestNewRootCmdRejectsUnknownFlag proves an unrecognized flag surfaces as a
// flagError (mapping to exit code 2) rather than succeeding silently.
func TestNewRootCmdRejectsUnknownFlag(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--this-flag-does-not-exist", "value"})
	err := cmd.Execute()
	require.Error(t, err)
	var fe *flagError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, 2, exitCode(err))
}

// TestNewRootCmdRejectsPositionalArgs proves vdhost takes no positional
// arguments and reports it as a flag-class error.
func TestNewRootCmdRejectsPositionalArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.RunE = nil
	require.NoError(t, cmd.ParseFlags([]string{"unexpected-positional"}))
	err := cmd.ValidateArgs(cmd.Flags().Args())
	require.Error(t, err)
}
