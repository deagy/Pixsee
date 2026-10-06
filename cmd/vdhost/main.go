// Command vdhost is the virtual desktop host. It listens for TLS 1.3 clients,
// authenticates them with a 32-byte token, captures the desktop, and streams
// pixel updates while forwarding only keyboard and pointer input from the
// client. It sends only session/control metadata and visual updates; it never
// sends clipboard, file, audio, or command channels.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"virtualdesktop/internal/cliconfig"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/host/capture"
	"virtualdesktop/internal/host/input"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/session"
	"virtualdesktop/internal/transport"
)

// configEnvPrefix is the prefix vdhost uses for configuration environment
// variables: every flag is also settable as VDHOST_<FLAG_NAME>, upper-cased
// with dashes turned into underscores (e.g. -capture-interval also reads
// VDHOST_CAPTURE_INTERVAL). See internal/cliconfig for full precedence
// rules (flag > env > config file > default).
const configEnvPrefix = "VDHOST"

// configFileName is the base name (without extension) vdhost looks for when
// -config is not given, searched for as vdhost.yaml/.yml/.json/... in the
// current directory, $HOME/.config/virtualdesktop, then /etc/virtualdesktop.
const configFileName = "vdhost"

// loadToken reads a 32-byte authentication token from a file. It accepts either
// 32 raw bytes or 64 hexadecimal characters. An empty path yields the zero
// token; validateAuthConfig then decides whether tokenless operation is
// allowed at all (loopback bind + explicit -no-auth, per F4/AC-6). A provided
// path is still strictly validated.
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
	var configFile string

	cmd := &cobra.Command{
		Use:   "vdhost",
		Short: "Virtual desktop host",
		Long: "vdhost listens for TLS 1.3 clients, authenticates them with a 32-byte\n" +
			"token, captures the desktop, and streams pixel updates while forwarding\n" +
			"only keyboard and pointer input from the client.\n\n" +
			"Configuration values are resolved with the following precedence (highest\n" +
			"first): command-line flag, environment variable (VDHOST_<FLAG_NAME>, e.g.\n" +
			"VDHOST_ADDR or VDHOST_CAPTURE_INTERVAL), YAML config file (-config, or\n" +
			"vdhost.yaml found in ., $HOME/.config/virtualdesktop, or\n" +
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
	cmd.SetErrPrefix("vdhost:")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &flagError{err}
	})

	fs := cmd.Flags()
	fs.StringVar(&configFile, "config", "", "path to a YAML config file (default: search for vdhost.yaml in ., $HOME/.config/virtualdesktop, /etc/virtualdesktop)")
	fs.String("addr", "127.0.0.1:6511", "host:port to listen on")
	fs.String("token", "", "path to the 32-byte authentication token (raw or hex); required for non-loopback binds and for loopback binds unless -no-auth is set")
	fs.String("ca", "", "path to a PEM certificate to serve as the host certificate")
	fs.String("key", "", "path to the PEM private key matching -ca")
	fs.Duration("capture-interval", time.Second/30, "capture period")
	fs.Duration("keyframe-interval", 10*time.Second, "periodic keyframe period")
	fs.Duration("heartbeat-interval", 30*time.Second, "idle period before a heartbeat PING probe to the client")
	fs.Duration("heartbeat-timeout", 30*time.Second, "silent period before a client is treated as gone")
	fs.Int("max-input-per-sec", 500, "max input events per second")
	fs.Bool("enable-input", true, "accept client input events")
	fs.Duration("timeout", 10*time.Second, "per-operation I/O deadline")
	fs.Bool("no-auth", false, "explicitly run without an authentication token; valid ONLY for loopback binds (127.0.0.0/8, ::1, localhost). A non-loopback -addr always requires -token.")

	return cmd
}

type appConfig struct {
	addr              string
	token             [32]byte
	tlsConfig         *tls.Config
	capture           host.Capture
	input             func() (host.Input, error)
	listen            func(ctx context.Context) (net.Listener, error)
	captureInterval   time.Duration
	keyframeInterval  time.Duration
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	maxInputEvents    int
	enableInput       bool
	ioTimeout         time.Duration

	// ephemeralCert is set when no -ca/-key pair was supplied and the host
	// serves a fresh self-signed certificate; certFingerprint is the SHA-256
	// of its leaf DER, published at startup so a client can pin it with
	// -fingerprint (F6 / AC-7).
	ephemeralCert   bool
	certFingerprint [sha256.Size]byte

	// admissions carries the single-active-session policy into
	// internal/session.Accept. The host permits exactly one active client
	// session at a time (docs §3, §5); a second authenticated connection is
	// rejected with ERROR_BUSY by the shared establishment path (F10/AC-9).
	admissions *session.Admissions
}

// inputOwnership lazily creates the platform input adapter and owns closing
// it when the host shuts down. run() installs get() as cfg.input so every
// session injects through one shared adapter. Extracted verbatim from the
// inline wiring in run() so the adapter lifetime (F5 / AC-5) is testable
// against production code.
type inputOwnership struct {
	create func() (host.Input, error)

	once     sync.Once
	mu       sync.Mutex
	adapter  host.Input
	closer   func()
	err      error
	closeOne sync.Once
}

// newInputOwnership wires the platform adapter constructor (X11 on linux,
// SendInput on windows, CoreGraphics on darwin) into the ownership.
func newInputOwnership() *inputOwnership {
	return &inputOwnership{create: func() (host.Input, error) {
		inj, err := input.New()
		if err != nil {
			return nil, err
		}
		return inj, nil
	}}
}

// get creates the adapter once and hands the same live adapter to every
// caller. A successful create also records the adapter's shutdown closer.
func (o *inputOwnership) get() (host.Input, error) {
	o.once.Do(func() {
		inj, err := o.create()
		if err != nil {
			o.err = err
			return
		}
		closer, _ := inj.(interface{ Close() })
		o.mu.Lock()
		o.adapter = inj
		if closer != nil {
			o.closer = closer.Close
		}
		o.mu.Unlock()
		// F5 fix: nothing here closes the adapter. The baseline carried a
		// `defer closer.Close()` in this body, which ran the moment the Once
		// body returned — killing the injector (and every later injection and
		// ReleaseAll) against a dead socket. Shutdown owns the close.
	})
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.adapter, o.err
}

// close runs the recorded adapter close exactly once; safe when the adapter
// was never created.
func (o *inputOwnership) close() {
	o.closeOne.Do(func() {
		o.mu.Lock()
		c := o.closer
		o.mu.Unlock()
		if c != nil {
			c()
		}
	})
}

// maxHeartbeatFlag bounds -heartbeat-interval and -heartbeat-timeout: a
// heartbeat that cannot fit inside a day is a misconfiguration, not a
// remote-desktop keepalive policy (spec AC-4 "sane caps").
const maxHeartbeatFlag = 24 * time.Hour

// validateHeartbeatConfig enforces the startup invariant (AC-4 / D7): both
// heartbeat flags positive and capped, and HeartbeatInterval no larger than
// HeartbeatTimeout + transport.HeartbeatSlack. Without the invariant the
// keepalive watchdog's own window cannot fit its read deadline: a probe would
// only go out after the peer's reads were already expected to expire. The
// error names both offending values.
func validateHeartbeatConfig(interval, timeout time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("host: heartbeat-interval must be positive, got %v", interval)
	}
	if timeout <= 0 {
		return fmt.Errorf("host: heartbeat-timeout must be positive, got %v", timeout)
	}
	if interval > maxHeartbeatFlag || timeout > maxHeartbeatFlag {
		return fmt.Errorf("host: heartbeat-interval %v and heartbeat-timeout %v must not exceed %v", interval, timeout, maxHeartbeatFlag)
	}
	if slack := transport.HeartbeatSlack; interval > timeout+slack {
		return fmt.Errorf("host: heartbeat-interval %v exceeds heartbeat-timeout %v plus %v slack; the keepalive watchdog could not detect a dead peer before its own read deadline", interval, timeout, slack)
	}
	return nil
}

// buildConfigFromViper performs the same validation and defaulting the
// pre-Cobra loadConfig used to perform, now reading already-resolved values
// (flag > env > config file > default) from v.
func buildConfigFromViper(v *viper.Viper) (*appConfig, error) {
	addr := v.GetString("addr")
	tokenPath := v.GetString("token")
	caPath := v.GetString("ca")
	keyPath := v.GetString("key")
	captureRate := v.GetDuration("capture-interval")
	keyframeRate := v.GetDuration("keyframe-interval")
	heartbeatInterval := v.GetDuration("heartbeat-interval")
	heartbeatTimeout := v.GetDuration("heartbeat-timeout")
	maxInput := v.GetInt("max-input-per-sec")
	enableInput := v.GetBool("enable-input")
	timeout := v.GetDuration("timeout")
	noAuth := v.GetBool("no-auth")

	cfg := &appConfig{
		addr:              addr,
		captureInterval:   captureRate,
		keyframeInterval:  keyframeRate,
		heartbeatInterval: heartbeatInterval,
		heartbeatTimeout:  heartbeatTimeout,
		maxInputEvents:    maxInput,
		enableInput:       enableInput,
		ioTimeout:         timeout,
	}
	if err := validateHeartbeatConfig(cfg.heartbeatInterval, cfg.heartbeatTimeout); err != nil {
		return nil, err
	}
	if err := checkTokenFilePermissions(tokenPath); err != nil {
		return nil, err
	}
	token, err := loadToken(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("host: token: %w", err)
	}
	cfg.token = token
	// F4 / AC-6, owner decision Q2: the documented auth gate is enforced here,
	// at the shared startup config surface, not deferred to connection
	// handling. A non-loopback bind always requires a configured token;
	// tokenless operation on loopback requires the explicit -no-auth opt-in.
	if err := validateAuthConfig(addr, cfg.token, noAuth); err != nil {
		return nil, err
	}
	if cfg.token == ([32]byte{}) {
		// Tokenless mode is only reachable through the explicit -no-auth
		// opt-in on a loopback bind (validated above); keep it loud (D5).
		fmt.Fprintln(os.Stderr, "vdhost: WARNING: -no-auth explicitly accepted: running WITHOUT authentication; any non-zero client token is admitted (loopback bind only)")
	}
	tlsConfig, err := buildTLSConfig(caPath, keyPath)
	if err != nil {
		return nil, err
	}
	cfg.tlsConfig = tlsConfig
	if caPath == "" || keyPath == "" {
		// Ephemeral self-signed mode: compute the pin a client needs for
		// -fingerprint; it is announced at startup (F6 / AC-7).
		fp, err := ephemeralFingerprint(tlsConfig)
		if err != nil {
			return nil, err
		}
		cfg.ephemeralCert, cfg.certFingerprint = true, fp
	}
	cfg.listen = func(ctx context.Context) (net.Listener, error) {
		var d net.ListenConfig
		return d.Listen(ctx, "tcp", addr)
	}
	cfg.admissions = &session.Admissions{}
	return cfg, nil
}

// ephemeralCertValidity documents how long the self-signed ephemeral
// certificate served in the no-CA path stays valid (spec Q3 decision: kept
// at 1 h; renewal/TOFU pinning deferred out of R2).
const ephemeralCertValidity = time.Hour

// isLoopbackBind reports whether addr binds only to loopback interfaces:
// 127.0.0.0/8, ::1, or the literal host "localhost" (AC-6 / Q2). An empty
// host (":port"), the wildcards 0.0.0.0 / ::, and any routable address are
// NOT loopback.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateAuthConfig enforces the fail-closed startup gate (F4 / AC-6): a
// non-zero token always unlocks the host; without one, a non-loopback bind
// is refused naming the address and the fix (set -token), and a loopback
// bind is refused unless -no-auth was passed explicitly. -no-auth does NOT
// unlock non-loopback binds (Q2: remote operation always requires real
// authentication material).
func validateAuthConfig(addr string, token [32]byte, noAuth bool) error {
	if token != ([32]byte{}) {
		return nil
	}
	if !isLoopbackBind(addr) {
		return fmt.Errorf("host: refusing to start: non-loopback bind %v without authentication material; set -token to a 32-byte token file (a tokenless host may bind only to loopback; -no-auth does not unlock non-loopback binds)", addr)
	}
	if !noAuth {
		return fmt.Errorf("host: refusing to start: loopback bind %v without a -token; pass -no-auth explicitly to run tokenless on loopback, or set -token", addr)
	}
	return nil
}

// checkTokenFilePermissions refuses a file-backed token that is accessible
// beyond its owner: mode bits with any group/other permission leak a bearer
// credential to the rest of the machine (AC-6).
func checkTokenFilePermissions(path string) error {
	if path == "" {
		return nil
	}
	if runtime.GOOS == "windows" {
		// Windows authorizes through ACLs; the POSIX mode bits Go reports
		// there are synthetic and would reject every file.
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil // loadToken surfaces the canonical read error
	}
	if m := fi.Mode(); m&0o077 != 0 {
		return fmt.Errorf("host: token file %s is accessible beyond its owner (mode %o); run chmod 0600 %s", path, m.Perm(), path)
	}
	return nil
}

// ephemeralFingerprint returns the SHA-256 of the leaf DER the host serves —
// exactly the value transport.ClientTLSConfigForFingerprint compares against
// the wire (PeerCertificates[0].Raw), so a client can paste the printed hex
// into -fingerprint verbatim.
func ephemeralFingerprint(cfg *tls.Config) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if cfg == nil || len(cfg.Certificates) == 0 || len(cfg.Certificates[0].Certificate) == 0 {
		return zero, errors.New("host: ephemeral certificate configuration serves no certificate")
	}
	return sha256.Sum256(cfg.Certificates[0].Certificate[0]), nil
}

// announceEphemeralCert publishes the ephemeral certificate's pin at startup
// (F6 / AC-7) and points the operator at the client flag that consumes it, on
// a single line so the copy-pasteable value never splits across reads. The
// private key is never logged; only the public fingerprint.
func announceEphemeralCert(w io.Writer, fingerprint [sha256.Size]byte) {
	fmt.Fprintf(w, "vdhost: ephemeral certificate SHA-256 fingerprint: %x (self-signed, valid %v; connect a client with -fingerprint %x)\n", fingerprint, ephemeralCertValidity, fingerprint)
}

// loadConfig retains the pre-Cobra entrypoint signature for callers (and
// tests) that parse raw CLI args directly, without going through Cobra. It
// still applies the full flag > env (VDHOST_*) > config file > default
// precedence via internal/cliconfig.
func loadConfig(args []string) (*appConfig, error) {
	cmd := newRootCmd()
	cmd.SetArgs(normalizeArgs(args))
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
	return serve(ctx, cfg)
}

// serve runs the host until ctx is canceled: it wires the platform adapters,
// opens the listener, and accepts connections. Cancellation must both unblock
// the blocking Accept and leave no listener behind, so serve registers a
// context callback that closes the listener when ctx is done. Signal handling
// itself stays in run, keeping this shutdown path testable by canceling a
// context directly instead of delivering a real signal.
func serve(ctx context.Context, cfg *appConfig) error {
	// Wire the platform adapters. The capture adapter reads the desktop; the
	// input adapter injects keyboard and pointer events. Both are selected by
	// build tags behind the neutral capture and input packages, so this wiring
	// is platform-neutral: linux uses X11, windows uses SendInput, and darwin
	// uses CoreGraphics.
	cfg.capture = capture.New()

	// The input adapter is created lazily on the first session and lives as
	// long as the host process: the adapter is closed exactly once, at
	// shutdown, by inputOwnership.close (F5). The wiring is extracted from
	// run()'s former inline block as inputOwnership so adapter lifetime is
	// covered by tests of the production path (AC-5).
	ownership := newInputOwnership()
	cfg.input = ownership.get
	defer ownership.close()

	ln, err := cfg.listen(ctx)
	if err != nil {
		return fmt.Errorf("host: listen: %w", err)
	}
	defer ln.Close()

	// Cancellation must unblock the Accept below: ctx alone does not close the
	// listener, so register a callback that does. stopClose unregisters the
	// callback if it has not started; if cancellation already started it, that
	// callback may still call Close concurrently with the deferred Close.
	// Listener methods support concurrent calls, and any redundant-Close error
	// is ignored by both call sites.
	stopClose := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stopClose()

	fmt.Printf("vdhost listening on %s\n", ln.Addr())
	if cfg.ephemeralCert {
		// Publish the pin a client must use with -fingerprint (F6 / AC-7).
		announceEphemeralCert(os.Stdout, cfg.certFingerprint)
	}

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

// handleConnection owns one inbound connection end to end. Session
// establishment itself (TLS accept, AUTH, busy admission, HELLO) lives in
// internal/session and is shared verbatim with the integration harness
// (F10/AC-9); this function is pure wiring: accept, adapters, service.
func handleConnection(ctx context.Context, cfg *appConfig, conn net.Conn) {
	established, err := session.Accept(ctx, conn, session.Config{
		Token:      cfg.token,
		TLSConfig:  cfg.tlsConfig,
		Limits:     protocol.DefaultLimits(),
		IOTimeout:  cfg.ioTimeout,
		Admissions: cfg.admissions,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vdhost: %v\n", err)
		return
	}
	defer established.Release()
	fmt.Printf("vdhost: client authenticated from %s (protocol v%d)\n", conn.RemoteAddr(), established.Version)

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
		HeartbeatInterval:       cfg.heartbeatInterval,
		HeartbeatTimeout:        cfg.heartbeatTimeout,
	}, cfg.capture, input)
	if err := svc.Run(ctx, established.Peer); err != nil {
		fmt.Fprintf(os.Stderr, "vdhost: session ended: %v\n", err)
	}
	_ = conn.Close()
}
