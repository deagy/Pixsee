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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
			if configFile != "" {
				return nil, fmt.Errorf("config: reading %s: %w", configFile, err)
			}
			return nil, fmt.Errorf("config: %w", err)
		}
		// No config file found in the default search paths: fine, config
		// files are optional.
	}

	if err := v.BindPFlags(fs); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return v, nil
}
