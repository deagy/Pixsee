// Package cliconfig provides shared Viper wiring for the vdhost and vdclient
// command-line tools. It gives every configuration value the following
// precedence, highest to lowest:
//
//  1. command-line flag (e.g. -addr / --addr)
//  2. environment variable (see Options.EnvPrefix)
//  3. YAML config file (see Options.ConfigName and the default search paths)
//  4. the flag's built-in default value
//
// This ordering falls directly out of github.com/spf13/viper's own
// precedence rules once a flag set is bound with BindPFlags: an explicitly
// set flag always wins, an unset flag's registered default acts as the
// lowest-priority fallback, and environment/config values fill the gap
// between them.
package cliconfig

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Options configures how a command's Viper instance locates its
// configuration.
type Options struct {
	// ConfigName is the base file name (without extension) Viper looks for
	// when no explicit config file is given, e.g. "vdhost" resolves to
	// vdhost.yaml, vdhost.yml, vdhost.json, etc. in the default search
	// paths documented on New.
	ConfigName string

	// EnvPrefix is prepended (with an underscore) to every flag name,
	// upper-cased with dashes turned into underscores, to form its
	// environment variable. For example with EnvPrefix "VDHOST" the
	// "-addr" flag is also settable via VDHOST_ADDR, and
	// "-capture-interval" via VDHOST_CAPTURE_INTERVAL.
	EnvPrefix string
}

// New builds a Viper instance for fs (a parsed Cobra/pflag flag set),
// optionally loads a YAML config file, layers in environment variables under
// opts.EnvPrefix, and binds every flag in fs so the four-tier precedence
// documented on this package applies to subsequent Get calls.
//
// If configFile is non-empty it is used verbatim (typically sourced from a
// -config flag) and it is an error if it cannot be read. Otherwise New
// searches for opts.ConfigName(.yaml|.yml|.json|...) in, in order:
//
//  1. the current working directory
//  2. $HOME/.config/virtualdesktop
//  3. /etc/virtualdesktop
//
// In that case a missing config file is not an error: file-based
// configuration is entirely optional and flags/env alone are sufficient.
func New(fs *pflag.FlagSet, opts Options, configFile string) (*viper.Viper, error) {
	v := viper.New()

	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		v.SetConfigName(opts.ConfigName)
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(filepath.Join(home, ".config", "virtualdesktop"))
		}
		v.AddConfigPath("/etc/virtualdesktop")
	}

	// Environment variables: VDHOST_ADDR / VDCLIENT_ADDR-style, per opts.EnvPrefix.
	v.SetEnvPrefix(opts.EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if errors.As(err, &notFound) {
			// No config file found in the default search paths: fine, config
			// files are optional.
		} else {
			used := v.ConfigFileUsed()
			// Some Windows tools/editors (PowerShell redirection,
			// Set-Content without an explicit -Encoding, Latin-1
			// mis-decodes of the shipped example files, ...) write config
			// files as UTF-16, add byte-order marks, or leave stray
			// control characters in the text. The YAML decoder rejects
			// those outright with "control characters are not allowed"
			// even though the intended content is valid YAML. Before
			// giving up, try to recover by sanitizing the raw bytes and
			// re-parsing.
			var raw []byte
			if used != "" {
				if data, readErr := os.ReadFile(used); readErr == nil {
					raw = data
					sanitized := sanitizeConfigBytes(raw)
					if !bytes.Equal(sanitized, raw) {
						v.SetConfigType("yaml")
						if retryErr := v.ReadConfig(bytes.NewReader(sanitized)); retryErr == nil {
							err = nil
						}
					}
				}
			}
			if err != nil {
				return nil, describeConfigError(used, raw, err)
			}
		}
	}

	if err := v.BindPFlags(fs); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return v, nil
}

// describeConfigError wraps a config-file read/parse failure so the message
// names the file that was actually used (the default search may have picked
// it up from any of several directories) and, when the failure was caused by
// a character the YAML decoder refuses, says which character and where it
// is. That turns an opaque "control characters are not allowed" into
// something a user can act on with a hex dump.
func describeConfigError(path string, raw []byte, err error) error {
	if path == "" {
		return fmt.Errorf("config: %w", err)
	}
	if off, r, ok := firstDisallowedRune(raw); ok {
		line, col := lineCol(raw, off)
		return fmt.Errorf("config: reading %s: %w (first disallowed character U+%04X at line %d, column %d, byte offset %d)", path, err, r, line, col, off)
	}
	return fmt.Errorf("config: reading %s: %w", path, err)
}

// lineCol converts a byte offset into a 1-based line and column, counting
// bytes as columns (adequate for pointing at a hex dump).
func lineCol(data []byte, off int) (line, col int) {
	line, col = 1, 1
	for i := 0; i < off && i < len(data); i++ {
		if data[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// sanitizeConfigBytes normalizes raw config-file bytes so they parse as
// valid YAML regardless of the encoding quirks introduced by Windows tools
// that generated or transcribed the file (PowerShell redirection/Set-Content
// without an explicit encoding, Notepad's legacy "Unicode" save option,
// Latin-1 mis-decodes that turn multi-byte UTF-8 into C1 control
// characters, etc.):
//
//   - A UTF-16 (LE or BE) byte-order mark is stripped and the remainder is
//     transcoded to UTF-8.
//   - Content that looks like UTF-16 (LE or BE) but is missing its BOM is
//     detected heuristically and transcoded the same way.
//   - A UTF-8 byte-order mark is stripped.
//   - Every remaining character the YAML decoder refuses (see
//     yamlAllowsRune) is dropped, as is every byte that is not valid UTF-8.
//     Neither can appear in a correctly encoded config file, so removing
//     them can only turn a guaranteed parse failure into a best-effort
//     success.
//
// A file that is already clean UTF-8 YAML is returned unchanged (byte for
// byte), so callers can cheaply detect "no sanitization was needed" via
// bytes.Equal.
func sanitizeConfigBytes(data []byte) []byte {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}): // UTF-16LE BOM
		data = utf16ToUTF8(data[2:], binary.LittleEndian)
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}): // UTF-16BE BOM
		data = utf16ToUTF8(data[2:], binary.BigEndian)
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}): // UTF-8 BOM
		data = data[3:]
	case looksLikeUTF16(data, binary.LittleEndian):
		data = utf16ToUTF8(data, binary.LittleEndian)
	case looksLikeUTF16(data, binary.BigEndian):
		data = utf16ToUTF8(data, binary.BigEndian)
	}
	return stripDisallowedRunes(data)
}

// looksLikeUTF16 heuristically detects BOM-less UTF-16 text: in that
// encoding every ASCII character (the overwhelming majority of bytes in a
// YAML config file) is followed or preceded by a zero byte, depending on
// endianness, while the opposite half-word is essentially never zero for
// realistic config content.
func looksLikeUTF16(data []byte, order binary.ByteOrder) bool {
	if len(data) < 8 || len(data)%2 != 0 {
		return false
	}
	units := len(data) / 2
	asciiLike := 0
	for i := 0; i < len(data); i += 2 {
		var lo, hi byte
		if order == binary.LittleEndian {
			lo, hi = data[i], data[i+1]
		} else {
			hi, lo = data[i], data[i+1]
		}
		// A code unit whose high byte is zero and low byte is a printable
		// ASCII byte or common whitespace is what a plain-ASCII YAML file
		// looks like when misread as UTF-16.
		if hi == 0 && (lo >= 0x20 && lo < 0x7F || lo == 0x09 || lo == 0x0A || lo == 0x0D) {
			asciiLike++
		}
	}
	// Require the vast majority of code units to look like ASCII-in-UTF-16;
	// genuine UTF-8/ASCII text misread this way would instead show zero
	// bytes at only half the *byte* positions with no such pattern here.
	return asciiLike >= units*9/10
}

// utf16ToUTF8 decodes data (a whole number of 16-bit code units, BOM
// already stripped) as UTF-16 in the given byte order and re-encodes it as
// UTF-8. Trailing odd bytes, if any, are ignored rather than causing a
// panic since they cannot form a valid code unit.
func utf16ToUTF8(data []byte, order binary.ByteOrder) []byte {
	n := len(data) / 2
	units := make([]uint16, n)
	for i := 0; i < n; i++ {
		units[i] = order.Uint16(data[i*2:])
	}
	return []byte(string(utf16.Decode(units)))
}

// yamlAllowsRune reports whether the YAML decoder (go.yaml.in/yaml/v3,
// readerc.go) accepts r in a document. This is the YAML 1.1 printable set:
//
//	#x9 | #xA | #xD | [#x20-#x7E] | #x85 | [#xA0-#xD7FF] | [#xE000-#xFFFD]
//	| [#x10000-#x10FFFF]
//
// Everything else -- C0 controls, DEL and the C1 range (U+007F-U+009F other
// than NEL), surrogates and the U+FFFE/U+FFFF non-characters -- makes the
// decoder fail with "control characters are not allowed".
func yamlAllowsRune(r rune) bool {
	switch {
	case r == 0x09, r == 0x0A, r == 0x0D:
		return true
	case r >= 0x20 && r <= 0x7E:
		return true
	case r == 0x85:
		return true
	case r >= 0xA0 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	}
	return false
}

// firstDisallowedRune scans data as UTF-8 and returns the byte offset and
// value of the first character the YAML decoder would refuse. An invalid
// UTF-8 byte is reported as utf8.RuneError.
func firstDisallowedRune(data []byte) (offset int, r rune, ok bool) {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return i, r, true
		}
		if !yamlAllowsRune(r) {
			return i, r, true
		}
		i += size
	}
	return 0, 0, false
}

// stripDisallowedRunes removes every character the YAML decoder refuses
// (see yamlAllowsRune) and every byte that is not valid UTF-8, keeping all
// other characters -- including multi-byte ones -- intact. Valid input is
// returned as a fresh but byte-identical slice.
func stripDisallowedRunes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // invalid byte: drop it
			continue
		}
		if yamlAllowsRune(r) {
			out = append(out, data[i:i+size]...)
		}
		i += size
	}
	return out
}
