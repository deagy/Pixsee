package main

// Real end-to-end harness: real host.Service + real X11 capture + real client
// session over TLS 1.3, driven over TCP loopback. Retries past the flaky
// screenshot library NumActiveDisplays() in headless sandboxes.
//
// Build: go build -o /tmp/e2e ./cmd/e2e
// Run:   DISPLAY=:99 /tmp/e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"

	capture_x11 "virtualdesktop/internal/host/capture/x11"
)

// --- test doubles ------------------------------------------------------------

type stateRecorder struct {
	states []client.ConnectionState
}

func (s *stateRecorder) ConnectionState(st client.ConnectionState, _ error) {
	s.states = append(s.states, st)
}
func (s *stateRecorder) contains(st client.ConnectionState) bool {
	for _, x := range s.states {
		if x == st {
			return true
		}
	}
	return false
}

// snapRenderer is a client.Renderer that records the last frame geometry.
type snapRenderer struct {
	lastW, lastH uint32
}

func (r *snapRenderer) Present(s client.Snapshot) {
	r.lastW, r.lastH = s.Width, s.Height
}

type recInput struct {
	keys     int
	releases int
}

func (i *recInput) Key(context.Context, uint16, protocol.Action, uint8) error   { i.keys++; return nil }
func (i *recInput) Move(context.Context, uint32, uint32) error                  { return nil }
func (i *recInput) Button(context.Context, protocol.Button, protocol.Action) error { return nil }
func (i *recInput) Wheel(context.Context, int16, int16) error                   { return nil }
func (i *recInput) ReleaseAll(context.Context) error                            { i.releases++; return nil }

// --- TLS ---------------------------------------------------------------------

func genTLS(t *testing.T) (serverTLS, clientTLS *tls.Config) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{ca.Raw}, PrivateKey: key}},
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	clientTLS, err = transport.ClientTLSConfig("localhost", roots)
	if err != nil {
		t.Fatal(err)
	}
	return serverTLS, clientTLS
}

// --- real capture with retry past flaky NumActiveDisplays --------------------

type realCapture struct {
	c *capture_x11.Capture
}

func (r *realCapture) Capture(ctx context.Context, displayID uint32) (damage.Image, error) {
	return r.c.Capture(ctx, displayID)
}

// waitCapture retries until the real X11 capture returns a non-zero image,
// working around the flaky screenshot library NumActiveDisplays() in headless
// sandboxes where it intermittently returns 0.
func waitCapture(t *testing.T) *realCapture {
	t.Helper()
	c := capture_x11.New()
	deadline := time.Now().Add(30 * time.Second)
	for {
		img, err := c.Capture(context.Background(), 0)
		if err == nil && img.Width > 0 && img.Height > 0 {
			return &realCapture{c: c}
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture never succeeded (NumActiveDisplays flaky); last err=%v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- host service side -------------------------------------------------------

func startHost(t *testing.T, ctx context.Context, serverTLS *tls.Config, token [32]byte, cap *realCapture, input *recInput) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		svc := host.NewService(host.Config{
			CaptureInterval:         time.Millisecond,
			KeyframeInterval:        time.Hour,
			MaxInputEventsPerSecond: 1000,
			EnableInput:             true,
		}, cap, input)
		serverConn := tls.Server(conn, serverTLS.Clone())
		if err := transport.Handshake(ctx, serverConn, 5*time.Second); err != nil {
			_ = conn.Close()
			return
		}
		peer := transport.NewPeerConn(serverConn, conn, protocol.RoleHost, protocol.DefaultLimits(), 5*time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			_ = conn.Close()
			return
		}
		if _, err := peer.Receive(ctx); err != nil {
			_ = conn.Close()
			return
		}
		_ = peer.Send(ctx, protocol.ServerHello{Version: protocol.Version1})
		runCtx, cancel := context.WithCancel(ctx)
		go func() {
			defer cancel()
			_ = svc.Run(runCtx, peer)
		}()
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func main() {
	t := &testing.T{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS := genTLS(t)
	var token [32]byte
	token[0] = 9

	cap := waitCapture(t)
	input := &recInput{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input)
	defer stop()

	renderer := &snapRenderer{}
	obs := &stateRecorder{}
	sess, err := client.NewSession(client.Config{
		Token:          token,
		TLSConfig:      clientTLS,
		IOTimeout:      5 * time.Second,
		ReconnectDelay: 50 * time.Millisecond,
		Dial:           func(context.Context) (net.Conn, error) { return net.Dial("tcp", addr) },
		Input:          client.NewInputState(nil),
	}, renderer, obs)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()

	// Wait for Connected.
	deadline := time.Now().Add(5 * time.Second)
	for !obs.contains(client.StateConnected) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Connected")
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap := sess.Snapshot()
	if snap.Width == 0 || snap.Height == 0 {
		t.Fatalf("no pixels after connect: %dx%d", snap.Width, snap.Height)
	}

	// Keyboard input.
	_ = sess.Input().SetFocused(true)
	_ = sess.Input().Key(0x04, protocol.ActionDown, 0)
	w := time.Now().Add(3 * time.Second)
	for input.keys == 0 {
		if time.Now().After(w) {
			t.Fatal("host never injected key")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = sess.Input().Key(0x04, protocol.ActionUp, 0)

	// Disconnect cleanup.
	sess.Input().Disconnect()
	cancel()
	<-done

	if input.keys < 1 {
		t.Fatal("expected key injection")
	}
	if input.releases < 1 {
		t.Fatal("expected ReleaseAll on disconnect")
	}
	println("E2E OK: connected, captured", snap.Width, "x", snap.Height, "pixels, injected", input.keys, "key events, released", input.releases)
}
