package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTokenAcceptsRawOrHexAndRejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	for name, content := range map[string][]byte{"raw": raw, "hex": []byte(hex.EncodeToString(raw) + "\n")} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadToken(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != [32]byte(raw) {
			t.Fatalf("%s token mismatch", name)
		}
	}
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(bad, []byte("short"), 0o600)
	if _, err := loadToken(bad); err == nil {
		t.Fatal("accepted short token")
	}
}

// TestLoadConfigTokenlessMintsRandomToken proves the tokenless path: with no
// -token the client mints a random non-zero bearer token so it can talk to a
// host in no-authentication mode, and a provided token is still honored.
func TestLoadConfigTokenlessMintsRandomToken(t *testing.T) {
	dir := t.TempDir()
	tokenPath := dir + "/token"
	if err := writeFile(tokenPath, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var fp [32]byte
	_, _ = rand.Read(fp[:])

	// Tokenless: no -token -> non-zero random token.
	cfg, err := loadConfig([]string{"-addr", "localhost:6511", "-fingerprint", hexString(fp)})
	if err != nil {
		t.Fatalf("loadConfig tokenless: %v", err)
	}
	if cfg.token == [32]byte{} {
		t.Fatal("expected a non-zero token in tokenless mode")
	}

	// Token-based: -token still honored.
	cfg, err = loadConfig([]string{"-addr", "localhost:6511", "-token", tokenPath, "-fingerprint", hexString(fp)})
	if err != nil {
		t.Fatalf("loadConfig with token: %v", err)
	}
	if cfg.token == [32]byte{} {
		t.Fatal("expected the provided token to be honored")
	}
}

func TestParseFingerprint(t *testing.T) {
	want := sha256.Sum256([]byte("certificate"))
	got, err := parseFingerprint(hex.EncodeToString(want[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("fingerprint mismatch")
	}
	if _, err := parseFingerprint("00"); err == nil {
		t.Fatal("accepted short fingerprint")
	}
}
