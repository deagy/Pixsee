// Command vdhost is the virtual desktop host. It listens for TLS 1.3 clients,
// authenticates them with a 32-byte token, captures the desktop, and streams
// pixel updates while forwarding only keyboard and pointer input from the
// client. It sends only session/control metadata and visual updates; it never
// sends clipboard, file, audio, or command channels.
package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"virtualdesktop/internal/host"
	"virtualdesktop/internal/host/capture"
	"virtualdesktop/internal/host/input"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// loadToken reads a 32-byte authentication token from a file. It accepts either
// 32 raw bytes or 64 hexadecimal characters. An empty path yields the zero
// token, which the host treats as no-authentication mode (any non-zero client
// token is accepted). A provided path is still strictly validated.
func loadToken(path string) ([32]byte, error) {
	var token [32]byte
	if path == "" {
		return token, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return token, err
	}
	if len(data) == len(token) {
		copy(token[:], data)
		return token, nil
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) != len(token) {
		return token, errors.New("token file must contain exactly 32 raw bytes or 64 hexadecimal characters")
	}
	copy(token[:], decoded)
	return token, nil
}

func main() {
	cmd := newRootCmd()
	cmd.SetArgs(normalizeArgs(os.Args[1:]))
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "vdhost:", err)
		os.Exit(exitCode(err))
	}
}

// normalizeArgs rewrites single-dash long flags (e.g. "-addr") to their
// double-dash pflag/Cobra equivalent ("--addr"), preserving the historical
// Go flag package convention (where "-x" and "--x" are equivalent) that
// existing vdhost invocations, scripts, and tests rely on. Single-character
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

// exitCode preserves the historical vdhost exit-code convention: 2 for
// argument/flag errors, 1 for all other runtime errors.
func exitCode(err error) int {
	var fe *flagError
	if errors.As(err, &fe) {
		return 2
	}
	return 1
}

// flagError marks an error produced while parsing flags/config, as opposed to
// an error produced while running the host, so main can map it to the
// historical exit code 2 while still printing the original message unchanged.
type flagError struct{ err error }

func (e *flagError) Error() string { return e.err.Error() }
func (e *flagError) Unwrap() error { return e.err }

// newRootCmd builds the vdhost root Cobra command. It owns flag definitions
// and wires them into an appConfig identically to the pre-Cobra flag package
// implementation, so existing flags, defaults, and behavior are unchanged.
func newRootCmd() *cobra.Command {
	var (
		addr         string
		tokenPath    string
		caPath       string
		keyPath      string
		captureRate  time.Duration
		keyframeRate time.Duration
		maxInput     int
		enableInput  bool
		timeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "vdhost",
		Short: "Virtual desktop host",
		Long: "vdhost listens for TLS 1.3 clients, authenticates them with a 32-byte\n" +
			"token, captures the desktop, and streams pixel updates while forwarding\n" +
			"only keyboard and pointer input from the client.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := buildConfig(addr, tokenPath, caPath, keyPath, captureRate, keyframeRate, maxInput, enableInput, timeout)
			if err != nil {
				return &flagError{err}
			}
			if err := run(cfg); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.SetErrPrefix("vdhost:")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &flagError{err}
	})

	fs := cmd.Flags()
	fs.StringVar(&addr, "addr", "127.0.0.1:6511", "host:port to listen on")
	fs.StringVar(&tokenPath, "token", "", "path to the 32-byte authentication token (raw or hex)")
	fs.StringVar(&caPath, "ca", "", "path to a PEM certificate to serve as the host certificate")
	fs.StringVar(&keyPath, "key", "", "path to the PEM private key matching -ca")
	fs.DurationVar(&captureRate, "capture-interval", time.Second/30, "capture period")
	fs.DurationVar(&keyframeRate, "keyframe-interval", 10*time.Second, "periodic keyframe period")
	fs.IntVar(&maxInput, "max-input-per-sec", 500, "max input events per second")
	fs.BoolVar(&enableInput, "enable-input", true, "accept client input events")
	fs.DurationVar(&timeout, "timeout", 10*time.Second, "per-operation I/O deadline")

	return cmd
}

type appConfig struct {
	addr             string
	token            [32]byte
	tlsConfig        *tls.Config
	capture          host.Capture
	input            func() (host.Input, error)
	inputAdapter     host.Input
	listen           func(ctx context.Context) (net.Listener, error)
	captureInterval  time.Duration
	keyframeInterval time.Duration
	maxInputEvents   int
	enableInput      bool
	ioTimeout        time.Duration
}

// buildConfig performs the same validation and defaulting the pre-Cobra
// loadConfig used to perform, now over already-parsed flag values.
func buildConfig(addr, tokenPath, caPath, keyPath string, captureRate, keyframeRate time.Duration, maxInput int, enableInput bool, timeout time.Duration) (*appConfig, error) {
	cfg := &appConfig{
		addr:             addr,
		captureInterval:  captureRate,
		keyframeInterval: keyframeRate,
		maxInputEvents:   maxInput,
		enableInput:      enableInput,
		ioTimeout:        timeout,
	}
	token, err := loadToken(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("host: token: %w", err)
	}
	cfg.token = token
	if cfg.token == [32]byte{} {
		fmt.Fprintln(os.Stderr, "vdhost: WARNING: no -token supplied; accepting any client token (no authentication)")
	}
	tlsConfig, err := buildTLSConfig(caPath, keyPath)
	if err != nil {
		return nil, err
	}
	cfg.tlsConfig = tlsConfig
	cfg.listen = func(ctx context.Context) (net.Listener, error) {
		var d net.ListenConfig
		return d.Listen(ctx, "tcp", addr)
	}
	return cfg, nil
}

// loadConfig retains the pre-Cobra entrypoint signature for callers (and
// tests) that parse raw CLI args directly, without going through Cobra.
func loadConfig(args []string) (*appConfig, error) {
	cmd := newRootCmd()
	cmd.SetArgs(args)
	var cfg *appConfig
	cmd.RunE = nil
	fs := cmd.Flags()
	if err := fs.Parse(normalizeArgs(args)); err != nil {
		return nil, err
	}
	if err := cobra.NoArgs(cmd, fs.Args()); err != nil {
		return nil, err
	}
	addr, _ := fs.GetString("addr")
	tokenPath, _ := fs.GetString("token")
	caPath, _ := fs.GetString("ca")
	keyPath, _ := fs.GetString("key")
	captureRate, _ := fs.GetDuration("capture-interval")
	keyframeRate, _ := fs.GetDuration("keyframe-interval")
	maxInput, _ := fs.GetInt("max-input-per-sec")
	enableInput, _ := fs.GetBool("enable-input")
	timeout, _ := fs.GetDuration("timeout")
	cfg, err := buildConfig(addr, tokenPath, caPath, keyPath, captureRate, keyframeRate, maxInput, enableInput, timeout)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func buildTLSConfig(caPath, keyPath string) (*tls.Config, error) {
	if caPath == "" || keyPath == "" {
		// No certificate supplied: serve an ephemeral in-memory self-signed
		// cert so TLS 1.3 still runs. A client must opt in (allow-insecure or
		// fingerprint) to trust it.
		return transport.EphemeralServerTLSConfig()
	}
	cert, err := tls.LoadX509KeyPair(caPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("host: keypair: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}, nil
}

func run(cfg *appConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Wire the platform adapters. The capture adapter reads the desktop; the
	// input adapter injects keyboard and pointer events. Both are selected by
	// build tags behind the neutral capture and input packages, so this wiring
	// is platform-neutral: linux uses X11, windows uses SendInput, and darwin
	// uses CoreGraphics.
	cfg.capture = capture.New()

	var inputErr error
	var inputOnce sync.Once
	cfg.input = func() (host.Input, error) {
		inputOnce.Do(func() {
			inj, err := input.New()
			if err != nil {
				inputErr = err
				return
			}
			defer inj.Close()
			cfg.inputAdapter = inj
		})
		return cfg.inputAdapter, inputErr
	}

	ln, err := cfg.listen(ctx)
	if err != nil {
		return fmt.Errorf("host: listen: %w", err)
	}
	defer ln.Close()
	fmt.Printf("vdhost listening on %s\n", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(os.Stderr, "vdhost: accept: %v\n", err)
			continue
		}
		go handleConnection(ctx, cfg, conn)
	}
}

func handleConnection(ctx context.Context, cfg *appConfig, conn net.Conn) {
	tlsConn := tls.Server(conn, cfg.tlsConfig.Clone())
	if err := transport.Handshake(ctx, tlsConn, cfg.ioTimeout); err != nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "vdhost: handshake: %v\n", err)
		return
	}
	peer := transport.NewPeerConn(tlsConn, conn, protocol.RoleHost, protocol.DefaultLimits(), cfg.ioTimeout)
	if err := peer.AuthenticateHost(ctx, cfg.token); err != nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "vdhost: authentication failed: %v\n", err)
		return
	}
	fmt.Printf("vdhost: client authenticated from %s\n", conn.RemoteAddr())

	input, err := cfg.input()
	if err != nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "vdhost: input adapter: %v\n", err)
		return
	}
	defer input.ReleaseAll(context.WithoutCancel(ctx))

	svc := host.NewService(host.Config{
		CaptureInterval:         cfg.captureInterval,
		KeyframeInterval:        cfg.keyframeInterval,
		MaxInputEventsPerSecond: cfg.maxInputEvents,
		EnableInput:             cfg.enableInput,
	}, cfg.capture, input)
	if err := svc.Run(ctx, peer); err != nil {
		fmt.Fprintf(os.Stderr, "vdhost: session ended: %v\n", err)
	}
	_ = conn.Close()
}
