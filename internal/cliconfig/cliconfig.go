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
		if !errors.As(err, &notFound) {
			// Some Windows tools/editors (notably PowerShell redirection
			// and Set-Content without an explicit -Encoding) write config
			// files as UTF-16 without a byte-order mark, or leave stray
			// control bytes in the file. gopkg's YAML decoder rejects
			// those bytes outright with "control characters are not
			// allowed", even though the file's intended content is
			// perfectly valid YAML. Before giving up, try to recover by
			// sanitizing the raw bytes (BOM stripping/UTF-16 transcoding,
			// disallowed control byte removal) and re-parsing.
			if used := v.ConfigFileUsed(); used != "" {
				if raw, readErr := os.ReadFile(used); readErr == nil {
					sanitized := sanitizeConfigBytes(raw)
					if !bytes.Equal(sanitized, raw) {
						v.SetConfigType("yaml")
						if retryErr := v.ReadConfig(bytes.NewReader(sanitized)); retryErr == nil {
							err = nil
						}
					}
				}
			}
		}
		if err != nil {
			if !errors.As(err, &notFound) {
				if configFile != "" {
					return nil, fmt.Errorf("config: reading %s: %w", configFile, err)
				}
				return nil, fmt.Errorf("config: %w", err)
			}
			// No config file found in the default search paths: fine, config
			// files are optional.
		}
	}

	if err := v.BindPFlags(fs); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return v, nil
}

// sanitizeConfigBytes normalizes raw config-file bytes so they parse as
// valid YAML regardless of the encoding quirks introduced by Windows tools
// that generated or transcribed the file (PowerShell redirection/Set-Content
// without an explicit encoding, Notepad's legacy "Unicode" save option,
// etc.):
//
//   - A UTF-16 (LE or BE) byte-order mark is stripped and the remainder is
//     transcoded to UTF-8.
//   - Content that looks like UTF-16 (LE or BE) but is missing its BOM is
//     detected heuristically and transcoded the same way.
//   - A UTF-8 byte-order mark is stripped.
//   - Any remaining bytes outside YAML's allowed character set (control
//     bytes other than tab/LF/CR) are dropped; these can only be introduced
//     by encoding corruption, since they never appear in a valid UTF-8
//     multi-byte sequence.
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
	return stripDisallowedControlBytes(data)
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

// stripDisallowedControlBytes removes bytes that the YAML 1.1 character
// set forbids outside of tab/LF/CR, without disturbing multi-byte UTF-8
// sequences: continuation and lead bytes for non-ASCII runes are always
// >= 0x80, so a byte-wise filter over the C0 control range and DEL is safe.
func stripDisallowedControlBytes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		switch {
		case b == 0x09 || b == 0x0A || b == 0x0D: // tab, LF, CR
			out = append(out, b)
		case b < 0x20 || b == 0x7F: // other C0 control bytes / DEL
			continue
		default:
			out = append(out, b)
		}
	}
	return out
}
