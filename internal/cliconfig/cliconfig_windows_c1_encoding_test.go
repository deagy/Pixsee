package cliconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"
)

// This file regression-tests the second round of the Windows config-encoding
// bug. After the UTF-16/BOM fix shipped in v1.3.1, pixsee_host_windows_arm64.exe
// still failed on Windows 11 ARM64 with
//
//	vdhost: config: While parsing config: yaml: control characters are not allowed
//
// The YAML decoder (go.yaml.in/yaml/v3, readerc.go) refuses more than the
// C0 range the first sanitizer stripped: DEL..U+0084, the C1 controls
// U+0086..U+009F, UTF-16 surrogates and the U+FFFE/U+FFFF non-characters.
// Those are all valid UTF-8, so the old byte-level filter left the file
// untouched and never retried. The concrete trigger was the em-dash in the
// shipped example configs going through a Latin-1 mis-decode.

// latin1DoubleEncode mimics a tool that decoded a UTF-8 file as ISO-8859-1
// (Latin-1) and re-encoded the result as UTF-8 -- what Windows PowerShell
// 5.1's Invoke-WebRequest does to a response without a charset, and what
// several editors do to "ANSI" text. Every multi-byte UTF-8 sequence turns
// into a run of U+00xx characters, some of which land in the C1 control
// range the YAML decoder refuses.
func latin1DoubleEncode(data []byte) []byte {
	runes := make([]rune, 0, len(data))
	for _, b := range data {
		runes = append(runes, rune(b))
	}
	return []byte(string(runes))
}

// TestNewParsesLatin1DoubleEncodedExampleConfig reproduces the reported
// failure: a config derived from configs/vdhost.example.yaml (which
// historically contained an em-dash, U+2014, encoded E2 80 94) went through
// a Latin-1 mis-decode, producing U+00E2 U+0080 U+0094. U+0080 is a C1
// control character, which the decoder rejects with exactly the reported
// error.
func TestNewParsesLatin1DoubleEncodedExampleConfig(t *testing.T) {
	src := []byte("# /etc/virtualdesktop — or you can point at an explicit file with -config.\r\n" +
		"addr: latin1-host:6511\r\n" +
		"timeout: 10s\r\n")
	raw := latin1DoubleEncode(src)
	require.NotEqual(t, src, raw)

	var probe map[string]any
	err := yaml.Unmarshal(raw, &probe)
	require.Error(t, err, "fixture must reproduce the raw control-character failure")
	assert.Contains(t, err.Error(), "control characters are not allowed")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "latin1.yaml")
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "latin1", EnvPrefix: "CLICONFIGLATIN1"}, configPath)
	require.NoError(t, err, "a Latin-1 double-encoded config must still parse")
	assert.Equal(t, "latin1-host:6511", v.GetString("addr"))
	assert.Equal(t, "10s", v.GetString("timeout"))
}

// TestNewParsesShippedExampleConfigsAfterLatin1DoubleEncode guards the
// shipped example files themselves: they must be pure ASCII (so no Windows
// re-encoding can introduce disallowed characters) and, belt and braces,
// must still load after a Latin-1 double-encode.
func TestNewParsesShippedExampleConfigsAfterLatin1DoubleEncode(t *testing.T) {
	for _, name := range []string{"vdhost", "vdclient"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "configs", name+".example.yaml"))
		require.NoError(t, err)
		for i, b := range src {
			require.Less(t, b, byte(0x80), "%s.example.yaml must be ASCII-only (non-ASCII byte at offset %d)", name, i)
		}

		dir := t.TempDir()
		configPath := filepath.Join(dir, name+".yaml")
		require.NoError(t, os.WriteFile(configPath, latin1DoubleEncode(src), 0o600))
		fs := newTestifyFlagSet()
		_, err = New(fs, Options{ConfigName: name, EnvPrefix: "CLICONFIGEXAMPLE"}, configPath)
		require.NoError(t, err, "%s.example.yaml must load after a Latin-1 double-encode", name)
	}
}

// TestNewParsesConfigWithC1AndNonCharacterRunes covers the rest of the
// character set the YAML decoder refuses but the old sanitizer ignored:
// DEL..U+0084, C1 controls other than NEL, and the U+FFFE/U+FFFF
// non-characters. All must be stripped while ordinary non-ASCII text
// survives. (NEL, U+0085, is allowed by the decoder but acts as a line
// break, so it is deliberately absent from this fixture.)
func TestNewParsesConfigWithC1AndNonCharacterRunes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "c1.yaml")
	content := "# caf\u00e9 \u007f\u0084\u009f ok\n" +
		"addr: c1-host:6511\n" +
		"token: /p\uffffath/tok\u00efen\n"
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o600))

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "c1", EnvPrefix: "CLICONFIGC1"}, configPath)
	require.NoError(t, err)
	assert.Equal(t, "c1-host:6511", v.GetString("addr"))
	assert.Equal(t, "/path/tok\u00efen", v.GetString("token"), "allowed non-ASCII characters must survive")
}

// TestNewParsesConfigWithInvalidUTF8Bytes covers a config saved as
// Windows-1252 "ANSI" text: a lone 0x97 (cp1252 em-dash) is not valid
// UTF-8 and must be dropped rather than aborting the load.
func TestNewParsesConfigWithInvalidUTF8Bytes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ansi.yaml")
	content := []byte("# /etc/virtualdesktop \x97 or -config\r\naddr: ansi-host:6511\r\n")
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	fs := newTestifyFlagSet()
	v, err := New(fs, Options{ConfigName: "ansi", EnvPrefix: "CLICONFIGANSI"}, configPath)
	require.NoError(t, err)
	assert.Equal(t, "ansi-host:6511", v.GetString("addr"))
}

// TestYAMLAllowsRuneMatchesDecoder pins yamlAllowsRune to the decoder's
// actual behaviour for the boundary characters, so the two cannot drift.
// NEL (U+0085) is accepted by the decoder but is a line break, so it is
// checked against yamlAllowsRune only.
func TestYAMLAllowsRuneMatchesDecoder(t *testing.T) {
	assert.True(t, yamlAllowsRune(0x85))
	for _, r := range []rune{0x08, 0x0B, 0x1F, 0x7F, 0x84, 0x86, 0x9F, 0xFFFE, 0xFFFF} {
		assert.False(t, yamlAllowsRune(r), "U+%04X", r)
		var probe map[string]any
		assert.Error(t, yaml.Unmarshal([]byte("addr: a"+string(r)+"b\n"), &probe), "decoder must reject U+%04X", r)
	}
	for _, r := range []rune{0x09, 0x20, 0x7E, 0xA0, 0xD7FF, 0xE000, 0xFFFD, 0x10000, 0x10FFFF} {
		assert.True(t, yamlAllowsRune(r), "U+%04X", r)
		var probe map[string]any
		assert.NoError(t, yaml.Unmarshal([]byte("addr: a"+string(r)+"b\n"), &probe), "decoder must accept U+%04X", r)
	}
}

// TestNewConfigErrorNamesFileAndOffendingCharacter proves that when a
// config still cannot be parsed, the error names the file that was used
// and, when a disallowed character is present, pinpoints it.
func TestNewConfigErrorNamesFileAndOffendingCharacter(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "broken.yaml")
	// Genuinely malformed YAML that survives sanitization unchanged.
	require.NoError(t, os.WriteFile(configPath, []byte("addr: [unterminated\n"), 0o600))
	fs := newTestifyFlagSet()
	_, err := New(fs, Options{ConfigName: "broken", EnvPrefix: "CLICONFIGBROKEN"}, configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), configPath, "error must name the config file")
	assert.NotContains(t, err.Error(), "disallowed character")

	raw := []byte("addr: x\n# \u0080\n")
	off, r, ok := firstDisallowedRune(raw)
	require.True(t, ok)
	assert.Equal(t, 10, off)
	assert.Equal(t, rune(0x80), r)
	line, col := lineCol(raw, off)
	assert.Equal(t, 2, line)
	assert.Equal(t, 3, col)
	assert.Contains(t, describeConfigError(configPath, raw, errors.New("boom")).Error(),
		"first disallowed character U+0080 at line 2, column 3, byte offset 10")

	_, _, ok = firstDisallowedRune([]byte("addr: clean\n"))
	assert.False(t, ok)
}

// TestNewDefaultSearchErrorNamesFile proves the default-search branch (no
// explicit config path) also reports which file it tripped over, since
// Viper may have picked it up from any of several directories.
func TestNewDefaultSearchErrorNamesFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vdsearch.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("addr: [unterminated\n"), 0o600))
	t.Chdir(dir)

	fs := newTestifyFlagSet()
	_, err := New(fs, Options{ConfigName: "vdsearch", EnvPrefix: "CLICONFIGSEARCH"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vdsearch.yaml")
}
