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
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"virtualdesktop/internal/host"
	"virtualdesktop/internal/host/capture"
	"virtualdesktop/internal/host/input"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// loadToken reads a 32-byte authentication token from a file. It accepts either
// 32 raw bytes or 64 hexadecimal characters.
func loadToken(path string) ([32]byte, error) {
	var token [32]byte
	if path == "" {
		return token, errors.New("token path is required")
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
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vdhost:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "vdhost:", err)
		os.Exit(1)
	}
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

func loadConfig(args []string) (*appConfig, error) {
	fs := flag.NewFlagSet("vdhost", flag.ContinueOnError)
	var (
		addr         = fs.String("addr", "127.0.0.1:6511", "host:port to listen on")
		tokenPath    = fs.String("token", "", "path to the 32-byte authentication token (raw or hex)")
		caPath       = fs.String("ca", "", "path to a PEM certificate to serve as the host certificate")
		keyPath      = fs.String("key", "", "path to the PEM private key matching -ca")
		captureRate  = fs.Duration("capture-interval", time.Second/30, "capture period")
		keyframeRate = fs.Duration("keyframe-interval", 10*time.Second, "periodic keyframe period")
		maxInput     = fs.Int("max-input-per-sec", 500, "max input events per second")
		enableInput  = fs.Bool("enable-input", true, "accept client input events")
		timeout      = fs.Duration("timeout", 10*time.Second, "per-operation I/O deadline")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg := &appConfig{
		addr:             *addr,
		captureInterval:  *captureRate,
		keyframeInterval: *keyframeRate,
		maxInputEvents:   *maxInput,
		enableInput:      *enableInput,
		ioTimeout:        *timeout,
	}
	token, err := loadToken(*tokenPath)
	if err != nil {
		return nil, fmt.Errorf("host: token: %w", err)
	}
	cfg.token = token
	tlsConfig, err := buildTLSConfig(*caPath, *keyPath)
	if err != nil {
		return nil, err
	}
	cfg.tlsConfig = tlsConfig
	cfg.listen = func(ctx context.Context) (net.Listener, error) {
		var d net.ListenConfig
		return d.Listen(ctx, "tcp", *addr)
	}
	return cfg, nil
}

func buildTLSConfig(caPath, keyPath string) (*tls.Config, error) {
	if caPath == "" || keyPath == "" {
		return nil, errors.New("host: -ca and -key are required")
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
