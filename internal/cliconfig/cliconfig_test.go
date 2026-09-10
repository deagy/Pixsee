package cliconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
)

func newTestFlagSet() *pflag.FlagSet {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("addr", "default-addr", "")
	fs.Int("count", 1, "")
	return fs
}

// TestPrecedenceDefaultOnly proves that with nothing else set, Get falls
// back to the flag's own default.
func TestPrecedenceDefaultOnly(t *testing.T) {
	fs := newTestFlagSet()
	v, err := New(fs, Options{ConfigName: "nonexistent-config-xyz", EnvPrefix: "CLICONFIGTEST"}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := v.GetString("addr"); got != "default-addr" {
		t.Fatalf("addr = %q, want default-addr", got)
	}
}

// TestPrecedenceConfigFileOverridesDefault proves a config file value beats
// the flag default when neither flag nor env is set.
func TestPrecedenceConfigFileOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(configPath, []byte("addr: file-addr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := newTestFlagSet()
	v, err := New(fs, Options{ConfigName: "test", EnvPrefix: "CLICONFIGTEST"}, configPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := v.GetString("addr"); got != "file-addr" {
		t.Fatalf("addr = %q, want file-addr", got)
	}
}

// TestPrecedenceEnvOverridesConfigFile proves an environment variable beats
// a config file value.
func TestPrecedenceEnvOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(configPath, []byte("addr: file-addr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLICONFIGTEST_ADDR", "env-addr")

	fs := newTestFlagSet()
	v, err := New(fs, Options{ConfigName: "test", EnvPrefix: "CLICONFIGTEST"}, configPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := v.GetString("addr"); got != "env-addr" {
		t.Fatalf("addr = %q, want env-addr", got)
	}
}

// TestPrecedenceFlagOverridesEnv proves an explicitly-set flag beats an
// environment variable, config file, and default alike.
func TestPrecedenceFlagOverridesEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(configPath, []byte("addr: file-addr\ncount: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLICONFIGTEST_ADDR", "env-addr")
	t.Setenv("CLICONFIGTEST_COUNT", "7")

	fs := newTestFlagSet()
	if err := fs.Set("addr", "flag-addr"); err != nil {
		t.Fatal(err)
	}
	v, err := New(fs, Options{ConfigName: "test", EnvPrefix: "CLICONFIGTEST"}, configPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := v.GetString("addr"); got != "flag-addr" {
		t.Fatalf("addr = %q, want flag-addr (flag set explicitly must win)", got)
	}
	// count was not set via flag, so env (7) must win over the file's 5.
	if got := v.GetInt("count"); got != 7 {
		t.Fatalf("count = %d, want 7 (env must win over config file)", got)
	}
}

// TestMissingConfigFileIsNotAnErrorWhenSearching proves that an absent
// config file in the default search paths is not an error: config files are
// optional.
func TestMissingConfigFileIsNotAnErrorWhenSearching(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	fs := newTestFlagSet()
	if _, err := New(fs, Options{ConfigName: "definitely-does-not-exist", EnvPrefix: "CLICONFIGTEST"}, ""); err != nil {
		t.Fatalf("New: unexpected error for missing config file: %v", err)
	}
}

// TestExplicitConfigFileMustExist proves that an explicitly-requested config
// file (e.g. via -config) that does not exist IS an error, unlike the
// optional default search.
func TestExplicitConfigFileMustExist(t *testing.T) {
	fs := newTestFlagSet()
	if _, err := New(fs, Options{ConfigName: "test", EnvPrefix: "CLICONFIGTEST"}, "/nonexistent/path/to/config.yaml"); err == nil {
		t.Fatal("expected error for explicit missing config file")
	}
}
