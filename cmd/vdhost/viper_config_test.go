package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConfigPrecedenceFileThenEnvThenFlag proves the full vdhost
// configuration precedence: flag > env (VDHOST_*) > config file > default.
func TestConfigPrecedenceFileThenEnvThenFlag(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vdhost.yaml")

	// 1. Config file alone sets addr and max-input-per-sec, beating the
	// built-in defaults.
	if err := os.WriteFile(configPath, []byte("addr: 127.0.0.1:7000\nmax-input-per-sec: 111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatalf("loadConfig (file only): %v", err)
	}
	if cfg.addr != "127.0.0.1:7000" {
		t.Fatalf("addr = %q, want 127.0.0.1:7000 from config file", cfg.addr)
	}
	if cfg.maxInputEvents != 111 {
		t.Fatalf("maxInputEvents = %d, want 111 from config file", cfg.maxInputEvents)
	}

	// 2. An environment variable overrides the config file value.
	t.Setenv("VDHOST_ADDR", "127.0.0.1:8000")
	cfg, err = loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatalf("loadConfig (env over file): %v", err)
	}
	if cfg.addr != "127.0.0.1:8000" {
		t.Fatalf("addr = %q, want 127.0.0.1:8000 from env (must beat config file)", cfg.addr)
	}
	// max-input-per-sec still comes from the file since no env/flag overrides it.
	if cfg.maxInputEvents != 111 {
		t.Fatalf("maxInputEvents = %d, want 111 from config file (unaffected by unrelated env var)", cfg.maxInputEvents)
	}

	// 3. An explicit flag overrides both the environment variable and the
	// config file.
	cfg, err = loadConfig([]string{"-config", configPath, "-addr", "127.0.0.1:9000"})
	if err != nil {
		t.Fatalf("loadConfig (flag over env+file): %v", err)
	}
	if cfg.addr != "127.0.0.1:9000" {
		t.Fatalf("addr = %q, want 127.0.0.1:9000 from flag (must beat env and config file)", cfg.addr)
	}
}

// TestConfigPrecedenceEnvDuration proves env vars work for non-string flag
// types (time.Duration), which Viper must coerce from the string env value.
func TestConfigPrecedenceEnvDuration(t *testing.T) {
	t.Setenv("VDHOST_TIMEOUT", "45s")
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ioTimeout != 45*time.Second {
		t.Fatalf("ioTimeout = %v, want 45s from env", cfg.ioTimeout)
	}
}

// TestConfigDefaultsWithoutOverrides proves that with no file, env, or flag
// override, the flag's built-in default is used unchanged.
func TestConfigDefaultsWithoutOverrides(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != "127.0.0.1:6511" {
		t.Fatalf("addr = %q, want default 127.0.0.1:6511", cfg.addr)
	}
	if cfg.ioTimeout != 10*time.Second {
		t.Fatalf("ioTimeout = %v, want default 10s", cfg.ioTimeout)
	}
}
