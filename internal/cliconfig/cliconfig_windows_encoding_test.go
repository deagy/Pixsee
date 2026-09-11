package cliconfig

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"
)

// This file regression-tests the fix for:
//
//	vdclient: config: While parsing config: yaml: control characters are not allowed
//
// which was observed loading a config file generated on Windows ARM64. The
// root cause is that some Windows tools (PowerShell's ">"/Out-File
// redirection, Set-Content without an explicit -Encoding, Notepad's legacy
// "Unicode" save option) write text files as UTF-16 -- with or without a
// byte-order mark -- rather than UTF-8. gopkg.in/yaml.v3 requires UTF-8 (or a
// BOM it recognizes) and rejects the embedded NUL/control bytes a raw UTF-16
// file contains when misread as UTF-8, exactly reproducing this error.
//
// New must recover from that by sanitizing the raw bytes (BOM stripping and
// UTF-16 transcoding, including the BOM-less case) before handing them to
// Viper/yaml, so a config file generated on Windows always loads correctly
// regardless of which platform runs vdclient/vdhost.

func utf16LEBytes(s string, withBOM bool) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2+2)
	if withBOM {
		buf = append(buf, 0xFF, 0xFE)
	}
	tmp := make([]byte, 2)
	for _, u := range units {
		binary.LittleEndian.PutUint16(tmp, u)
		buf = append(buf, tmp...)
	}
	return buf
}

func utf16BEBytes(s string, withBOM bool) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2+2)
	if withBOM {
		buf = append(buf, 0xFE, 0xFF)
	}
	tmp := make([]byte, 2)
	for _, u := range units {
		binary.BigEndian.PutUint16(tmp, u)
		buf = append(buf, tmp...)
	}
	return buf
}

// TestNewParsesWindowsUTF16ConfigWithBOM proves a config file written as
// UTF-16LE with a byte-order mark -- what PowerShell's Out-File/Set-Content
// produce by default on Windows, including Windows ARM64 -- parses
// successfully.
func TestNewParsesWindowsUTF16ConfigWithBOM(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "win-bom.yaml")
	content := "addr: windows-host:6511\r\nallow-insecure: true\r\n"
	require.NoError(t, os.WriteFile(configPath, utf16LEBytes(content, true), 0o600))

	fs := newTestifyFlagSet()
	fs.Bool("allow-insecure", false, "")
	v, err := New(fs, Options{ConfigName: "win-bom", EnvPrefix: "CLICONFIGWINBOM"}, configPath)
	require.NoError(t, err, "a BOM-marked UTF-16LE config file generated on Windows must parse")
	assert.Equal(t, "windows-host:6511", v.GetString("addr"))
	assert.True(t, v.GetBool("allow-insecure"))
}

// TestNewParsesWindowsUTF16BEConfigWithBOM covers the big-endian UTF-16 BOM
// case for completeness.
func TestNewParsesWindowsUTF16BEConfigWithBOM(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "win-bom-be.yaml")
	content := "addr: windows-host-be:6511\r\n"
	require.NoError(t, os.WriteFile(configPath, utf16BEBytes(content, true), 0o600))

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "win-bom-be", EnvPrefix: "CLICONFIGWINBOMBE"}, configPath)
	require.NoError(t, err, "a BOM-marked UTF-16BE config file must parse")
	assert.Equal(t, "windows-host-be:6511", v.GetString("addr"))
}

// TestNewParsesWindowsUTF16ConfigWithoutBOM reproduces the exact reported
// failure: this is the byte sequence that made gopkg.in/yaml.v3 return
// "control characters are not allowed" before the fix, since without a BOM
// the raw NUL bytes of BOM-less UTF-16LE are indistinguishable from control
// characters to a decoder assuming UTF-8.
func TestNewParsesWindowsUTF16ConfigWithoutBOM(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "win-nobom.yaml")
	content := "addr: arm64-host:6511\r\ntoken: /path/to/token\r\n"
	raw := utf16LEBytes(content, false)
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))

	// Sanity-check the fixture actually reproduces the historical bug
	// mechanism directly against the vendored YAML decoder, independent of
	// our fix, so this test can't silently stop testing what it claims to.
	var probe map[string]any
	require.Error(t, yaml.Unmarshal(raw, &probe), "fixture must reproduce the raw control-character failure")

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "win-nobom", EnvPrefix: "CLICONFIGWINNOBOM"}, configPath)
	require.NoError(t, err, "a BOM-less UTF-16LE config file generated on Windows must still parse")
	assert.Equal(t, "arm64-host:6511", v.GetString("addr"))
	assert.Equal(t, "/path/to/token", v.GetString("token"))
}

// TestNewParsesConfigWithStrayControlBytes proves that stray, genuinely
// invalid control bytes embedded in an otherwise-UTF-8 file (e.g. from a
// corrupted transfer or a buggy templating step) are stripped rather than
// aborting config loading outright.
func TestNewParsesConfigWithStrayControlBytes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "stray-control.yaml")
	content := []byte("addr: stray-host:6511\x00\r\nallow-insecure: true\r\n")
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	fs := newTestifyFlagSet()
	fs.Bool("allow-insecure", false, "")
	v, err := New(fs, Options{ConfigName: "stray-control", EnvPrefix: "CLICONFIGSTRAY"}, configPath)
	require.NoError(t, err, "stray control bytes must be sanitized rather than rejected")
	assert.Equal(t, "stray-host:6511", v.GetString("addr"))
	assert.True(t, v.GetBool("allow-insecure"))
}

// TestNewParsesUTF8BOMConfig proves a UTF-8 BOM (which some Windows editors
// still add, e.g. Notepad's "UTF-8" option) is stripped and does not affect
// parsing.
func TestNewParsesUTF8BOMConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "utf8-bom.yaml")
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte("addr: utf8bom-host:6511\r\n")...)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "utf8-bom", EnvPrefix: "CLICONFIGUTF8BOM"}, configPath)
	require.NoError(t, err)
	assert.Equal(t, "utf8bom-host:6511", v.GetString("addr"))
}

// TestSanitizeConfigBytesIsNoopForCleanUTF8 proves the sanitizer leaves
// already-clean UTF-8 content completely untouched, so the common case pays
// no cost and is not at risk of being mangled.
func TestSanitizeConfigBytesIsNoopForCleanUTF8(t *testing.T) {
	clean := []byte("addr: localhost:6511\nallow-insecure: true\n")
	assert.Equal(t, clean, sanitizeConfigBytes(clean))
}
