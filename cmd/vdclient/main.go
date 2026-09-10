// Command vdclient is the virtual desktop client. It connects to a host over
// TLS 1.3, authenticates with a 32-byte token, and presents the remote display
// through the chosen renderer while forwarding only keyboard and pointer input.
//
// Without -token the client runs in tokenless mode: it generates a random
// non-zero bearer token to send on the wire (the wire invariant forbids a zero
// token), which a host in no-authentication mode accepts. Both modes remain
// supported; a host that requires a specific token still needs -token.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"virtualdesktop/internal/cliconfig"
	"virtualdesktop/internal/client"
	"virtualdesktop/internal/transport"
)

// configEnvPrefix is the prefix vdclient uses for configuration environment
// variables: every flag is also settable as VDCLIENT_<FLAG_NAME>, upper-cased
// with dashes turned into underscores (e.g. -allow-insecure also reads
// VDCLIENT_ALLOW_INSECURE). See internal/cliconfig for full precedence rules
// (flag > env > config file > default).
const configEnvPrefix = "VDCLIENT"

// configFileName is the base name (without extension) vdclient looks for
// when -config is not given, searched for as vdclient.yaml/.yml/.json/...
// in the current directory, $HOME/.config/virtualdesktop, then
// /etc/virtualdesktop.
const configFileName = "vdclient"

func main() {
	cmd := newRootCmd()
	cmd.SetArgs(normalizeArgs(os.Args[1:]))
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "vdclient:", err)
		os.Exit(exitCode(err))
	}
}

// normalizeArgs rewrites single-dash long flags (e.g. "-addr") to their
// double-dash pflag/Cobra equivalent ("--addr"), preserving the historical Go
// flag package convention (where "-x" and "--x" are equivalent) that existing
// vdclient invocations, scripts, and tests rely on. Single-character
// flags/shorthands (e.g. "-h") and non-flag arguments are left untouched.
func normalizeArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			out[i] = "-" + a
		} else {
			out[i] = a
		}
	}
	return out
}

// exitCode preserves the historical vdclient exit-code convention: 2 for
// argument/flag errors, 1 for all other runtime errors.
func exitCode(err error) int {
	var fe *flagError
	if errors.As(err, &fe) {
		return 2
	}
	return 1
}

// flagError marks an error produced while parsing flags/config, as opposed to
// an error produced while running the client, so main can map it to the
// historical exit code 2 while still printing the original message unchanged.
type flagError struct{ err error }

func (e *flagError) Error() string { return e.err.Error() }
func (e *flagError) Unwrap() error { return e.err }

// newRootCmd builds the vdclient root Cobra command. It owns flag definitions
// and wires them into an appConfig identically to the pre-Cobra flag package
// implementation, so existing flags, defaults, and behavior are unchanged.
func newRootCmd() *cobra.Command {
	var configFile string

	cmd := &cobra.Command{
		Use:   "vdclient",
		Short: "Virtual desktop client",
		Long: "vdclient connects to a host over TLS 1.3, authenticates with a 32-byte\n" +
			"token, and presents the remote display while forwarding only keyboard\n" +
			"and pointer input.\n\n" +
			"Configuration values are resolved with the following precedence (highest\n" +
			"first): command-line flag, environment variable (VDCLIENT_<FLAG_NAME>, e.g.\n" +
			"VDCLIENT_ADDR or VDCLIENT_ALLOW_INSECURE), YAML config file (-config, or\n" +
			"vdclient.yaml found in ., $HOME/.config/virtualdesktop, or\n" +
			"/etc/virtualdesktop), then the flag's default.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := cliconfig.New(cmd.Flags(), cliconfig.Options{ConfigName: configFileName, EnvPrefix: configEnvPrefix}, configFile)
			if err != nil {
				return &flagError{err}
			}
			cfg, err := buildConfigFromViper(v)
			if err != nil {
				return &flagError{err}
			}
			if err := run(cfg); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.SetErrPrefix("vdclient:")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &flagError{err}
	})

	fs := cmd.Flags()
	fs.StringVar(&configFile, "config", "", "path to a YAML config file (default: search for vdclient.yaml in ., $HOME/.config/virtualdesktop, /etc/virtualdesktop)")
	fs.String("addr", "localhost:6511", "host:port to connect to")
	fs.String("server-name", "", "TLS server name (defaults to the host part of -addr)")
	fs.String("token", "", "path to the 32-byte authentication token (raw or hex)")
	fs.String("fingerprint", "", "exact SHA-256 certificate fingerprint to pin (64 hex chars, colons optional)")
	fs.String("ca", "", "path to a PEM CA bundle that signs the host certificate")
	fs.Bool("allow-insecure", false, "skip host certificate verification (use only with self-signed/untrusted hosts)")
	fs.Duration("timeout", 10*time.Second, "per-operation I/O deadline")
	fs.Duration("reconnect-delay", time.Second, "delay before a reconnect attempt")

	return cmd
}

type appConfig struct {
	addr           string
	serverName     string
	token          [32]byte
	tlsConfig      *tls.Config
	Dial           func(context.Context) (net.Conn, error)
	ioTimeout      time.Duration
	reconnectDelay time.Duration
}

// buildConfigFromViper performs the same validation and defaulting the
// pre-Cobra loadConfig used to perform, now reading already-resolved values
// (flag > env > config file > default) from v.
func buildConfigFromViper(v *viper.Viper) (*appConfig, error) {
	addr := v.GetString("addr")
	serverName := v.GetString("server-name")
	tokenPath := v.GetString("token")
	fingerprint := v.GetString("fingerprint")
	caPath := v.GetString("ca")
	allowInsecure := v.GetBool("allow-insecure")
	timeout := v.GetDuration("timeout")
	reconnect := v.GetDuration("reconnect-delay")

	if addr == "" {
		return nil, errors.New("client: -addr is required")
	}
	token, err := loadToken(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("client: token: %w", err)
	}
	if token == [32]byte{} {
		// No -token supplied: tokenless mode. The wire invariant forbids a
		// zero token, so mint a random non-zero bearer token. A host in
		// no-authentication mode accepts any non-zero token, mirroring the
		// host's own tokenless mode.
		if _, err := rand.Read(token[:]); err != nil {
			return nil, fmt.Errorf("client: token: %w", err)
		}
		fmt.Fprintln(os.Stderr, "vdclient: WARNING: no -token supplied; using a random token (tokenless mode against a no-authentication host)")
	}
	tlsConfig, err := buildTLSConfig(serverName, addr, fingerprint, caPath, allowInsecure)
	if err != nil {
		return nil, err
	}
	return &appConfig{
		addr:           addr,
		serverName:     serverName,
		token:          token,
		tlsConfig:      tlsConfig,
		ioTimeout:      timeout,
		reconnectDelay: reconnect,
	}, nil
}

// loadConfig retains the pre-Cobra entrypoint signature for callers (and
// tests) that parse raw CLI args directly, without going through Cobra. It
// still applies the full flag > env (VDCLIENT_*) > config file > default
// precedence via internal/cliconfig.
func loadConfig(args []string) (*appConfig, error) {
	cmd := newRootCmd()
	cmd.RunE = nil
	fs := cmd.Flags()
	if err := fs.Parse(normalizeArgs(args)); err != nil {
		return nil, err
	}
	if err := cobra.NoArgs(cmd, fs.Args()); err != nil {
		return nil, err
	}
	configFile, _ := fs.GetString("config")
	v, err := cliconfig.New(fs, cliconfig.Options{ConfigName: configFileName, EnvPrefix: configEnvPrefix}, configFile)
	if err != nil {
		return nil, err
	}
	return buildConfigFromViper(v)
}

func (c *appConfig) title() string { return "Virtual Desktop — " + c.addr }

func buildTLSConfig(serverName, addr, fingerprint, caPath string, allowInsecure bool) (*tls.Config, error) {
	if serverName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			serverName = host
		} else {
			serverName = addr
		}
	}
	switch {
	case allowInsecure:
		return transport.ClientTLSConfigForInsecure(serverName)
	case fingerprint != "":
		fp, err := parseFingerprint(fingerprint)
		if err != nil {
			return nil, fmt.Errorf("client: fingerprint: %w", err)
		}
		return transport.ClientTLSConfigForFingerprint(fp)
	case caPath != "":
		roots, err := loadCAPool(caPath)
		if err != nil {
			return nil, err
		}
		return transport.ClientTLSConfig(serverName, roots)
	default:
		return nil, errors.New("client: one of -allow-insecure, -fingerprint, or -ca is required")
	}
}

func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("client: ca: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("client: ca: no certificates found in " + path)
	}
	return roots, nil
}

func defaultDial(addr string, tlsConfig *tls.Config) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func run(cfg *appConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A single InputState is shared between the renderer (which captures input)
	// and the session (which delivers it to the host).
	input := client.NewInputState(nil)
	host := newRendererHost(cfg.title(), input)
	go host.Run()
	defer host.Quit()

	observer := &loggingObserver{}
	if cfg.Dial == nil {
		cfg.Dial = defaultDial(cfg.addr, cfg.tlsConfig)
	}
	session, err := client.NewSession(client.Config{
		Token:          cfg.token,
		TLSConfig:      cfg.tlsConfig,
		IOTimeout:      cfg.ioTimeout,
		ReconnectDelay: cfg.reconnectDelay,
		Dial:           cfg.Dial,
		Input:          input,
	}, host, observer)
	if err != nil {
		return err
	}

	observer.log("connecting to %s", cfg.addr)
	if err := session.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			observer.log("shutting down")
			return nil
		}
		return err
	}
	return nil
}
