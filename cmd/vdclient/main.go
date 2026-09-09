// Command vdclient is the virtual desktop client. It connects to a host over
// TLS 1.3, authenticates with a 32-byte token, and presents the remote display
// through the chosen renderer while forwarding only keyboard and pointer input.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/transport"
)

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vdclient:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "vdclient:", err)
		os.Exit(1)
	}
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

func loadConfig(args []string) (*appConfig, error) {
	fs := flag.NewFlagSet("vdclient", flag.ContinueOnError)
	var (
		addr          = fs.String("addr", "localhost:6511", "host:port to connect to")
		serverName    = fs.String("server-name", "", "TLS server name (defaults to the host part of -addr)")
		tokenPath     = fs.String("token", "", "path to the 32-byte authentication token (raw or hex)")
		fingerprint   = fs.String("fingerprint", "", "exact SHA-256 certificate fingerprint to pin (64 hex chars, colons optional)")
		caPath        = fs.String("ca", "", "path to a PEM CA bundle that signs the host certificate")
		allowInsecure = fs.Bool("allow-insecure", false, "skip host certificate verification (use only with self-signed/untrusted hosts)")
		timeout       = fs.Duration("timeout", 10*time.Second, "per-operation I/O deadline")
		reconnect     = fs.Duration("reconnect-delay", time.Second, "delay before a reconnect attempt")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *addr == "" {
		return nil, errors.New("client: -addr is required")
	}
	token, err := loadToken(*tokenPath)
	if err != nil {
		return nil, fmt.Errorf("client: token: %w", err)
	}
	tlsConfig, err := buildTLSConfig(*serverName, *addr, *fingerprint, *caPath, *allowInsecure)
	if err != nil {
		return nil, err
	}
	return &appConfig{
		addr:           *addr,
		serverName:     *serverName,
		token:          token,
		tlsConfig:      tlsConfig,
		ioTimeout:      *timeout,
		reconnectDelay: *reconnect,
	}, nil
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
