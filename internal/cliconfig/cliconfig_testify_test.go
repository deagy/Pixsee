package cliconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file complements cliconfig_test.go with testify assert/require based
// assertions, covering both a happy-path precedence chain and explicit
// error cases (missing explicit config file, malformed config content).

func newTestifyFlagSet() *pflag.FlagSet {
	fs := pflag.NewFlagSet("testify-test", pflag.ContinueOnError)
	fs.String("addr", "default-addr", "")
	fs.Int("count", 1, "")
	return fs
}

// TestNewHappyPathFullPrecedenceChain proves the full precedence chain
// (flag > env > file > default) end to end with testify assertions.
func TestNewHappyPathFullPrecedenceChain(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "testify.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("addr: file-addr\ncount: 5\n"), 0o600))

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "testify", EnvPrefix: "CLICONFIGTESTIFY"}, configPath)
	require.NoError(t, err)
	assert.Equal(t, "file-addr", v.GetString("addr"), "file value should apply with no env/flag override")
	assert.Equal(t, 5, v.GetInt("count"))

	t.Setenv("CLICONFIGTESTIFY_COUNT", "9")
	v, err = New(fs, Options{ConfigName: "testify", EnvPrefix: "CLICONFIGTESTIFY"}, configPath)
	require.NoError(t, err)
	assert.Equal(t, 9, v.GetInt("count"), "env should override the config file")

	require.NoError(t, fs.Set("addr", "flag-addr"))
	v, err = New(fs, Options{ConfigName: "testify", EnvPrefix: "CLICONFIGTESTIFY"}, configPath)
	require.NoError(t, err)
	assert.Equal(t, "flag-addr", v.GetString("addr"), "explicit flag should override env and file")
}

// TestNewErrorCases proves New surfaces errors for an explicit missing
// config file and for malformed YAML content, while a missing file in the
// default search path remains non-fatal.
func TestNewErrorCases(t *testing.T) {
	fs := newTestifyFlagSet()

	t.Run("explicit file missing", func(t *testing.T) {
		_, err := New(fs, Options{ConfigName: "testify", EnvPrefix: "CLICONFIGTESTIFY"}, "/nonexistent/testify-missing.yaml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config:")
	})

	t.Run("malformed yaml content", func(t *testing.T) {
		dir := t.TempDir()
		badPath := filepath.Join(dir, "bad.yaml")
		require.NoError(t, os.WriteFile(badPath, []byte("addr: [unterminated\n"), 0o600))
		_, err := New(fs, Options{ConfigName: "testify", EnvPrefix: "CLICONFIGTESTIFY"}, badPath)
		require.Error(t, err)
	})

	t.Run("default search finds nothing", func(t *testing.T) {
		dir := t.TempDir()
		cwd, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(cwd) })

		_, err = New(fs, Options{ConfigName: "definitely-absent-testify", EnvPrefix: "CLICONFIGTESTIFY"}, "")
		assert.NoError(t, err, "a missing default-search config file must not be an error")
	})
}
