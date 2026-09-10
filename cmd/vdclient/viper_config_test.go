package main

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConfigPrecedenceFileThenEnvThenFlag proves the full vdclient
// configuration precedence: flag > env (VDCLIENT_*) > config file > default.
func TestConfigPrecedenceFileThenEnvThenFlag(t *testing.T) {
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	fpHex := hexString(fp)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "vdclient.yaml")

	// 1. Config file alone sets addr and allow-insecure, beating the
	// built-in defaults.
	if err := os.WriteFile(configPath, []byte("addr: file-host:6511\nallow-insecure: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatalf("loadConfig (file only): %v", err)
	}
	if cfg.addr != "file-host:6511" {
		t.Fatalf("addr = %q, want file-host:6511 from config file", cfg.addr)
	}

	// 2. An environment variable overrides the config file value.
	t.Setenv("VDCLIENT_ADDR", "env-host:6511")
	cfg, err = loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatalf("loadConfig (env over file): %v", err)
	}
	if cfg.addr != "env-host:6511" {
		t.Fatalf("addr = %q, want env-host:6511 from env (must beat config file)", cfg.addr)
	}

	// 3. An explicit flag overrides both the environment variable and the
	// config file.
	cfg, err = loadConfig([]string{"-config", configPath, "-addr", "flag-host:6511", "-fingerprint", fpHex})
	if err != nil {
		t.Fatalf("loadConfig (flag over env+file): %v", err)
	}
	if cfg.addr != "flag-host:6511" {
		t.Fatalf("addr = %q, want flag-host:6511 from flag (must beat env and config file)", cfg.addr)
	}
}

// TestConfigPrecedenceEnvDuration proves env vars work for non-string flag
// types (time.Duration), which Viper must coerce from the string env value.
func TestConfigPrecedenceEnvDuration(t *testing.T) {
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	t.Setenv("VDCLIENT_RECONNECT_DELAY", "3s")
	cfg, err := loadConfig([]string{"-fingerprint", hexString(fp)})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.reconnectDelay != 3*time.Second {
		t.Fatalf("reconnectDelay = %v, want 3s from env", cfg.reconnectDelay)
	}
}

// TestConfigDefaultsWithoutOverrides proves that with no file, env, or flag
// override, the flag's built-in default is used unchanged.
func TestConfigDefaultsWithoutOverrides(t *testing.T) {
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	cfg, err := loadConfig([]string{"-fingerprint", hexString(fp)})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != "localhost:6511" {
		t.Fatalf("addr = %q, want default localhost:6511", cfg.addr)
	}
	if cfg.ioTimeout != 10*time.Second {
		t.Fatalf("ioTimeout = %v, want default 10s", cfg.ioTimeout)
	}
}
