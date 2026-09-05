package main

import (
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
