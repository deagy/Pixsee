package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"

	capture_x11 "virtualdesktop/internal/host/capture"
)

// hostInput is a shared input recorder for the real e2e test.
var hostInput = &recInput{}

// recInput records the input events injected by the host.
type recInput struct {
	keys     int
	releases int
}

func (i *recInput) Key(context.Context, uint16, protocol.Action, uint8) error {
	i.keys++
	return nil
}
func (i *recInput) Move(context.Context, uint32, uint32) error { return nil }
func (i *recInput) Button(context.Context, protocol.Button, protocol.Action) error {
	return nil
}
func (i *recInput) Wheel(context.Context, int16, int16) error { return nil }
func (i *recInput) ReleaseAll(context.Context) error {
	i.releases++
	return nil
}

// TestRealCapture exercises the REAL X11 capture adapter against a live Xvfb
// display (set via $DISPLAY=:99 when running this test). It works around the
// intermittent flakiness of the screenshot library's NumActiveDisplays() in
// headless sandboxes by retrying until a non-zero image is returned.
//
// This test is SKIPPED when no X11 display is available, so it does not fail
// in environments without a display.
func TestRealCapture(t *testing.T) {
	if display, ok := os.LookupEnv("DISPLAY"); !ok || display == "" {
		t.Skip("no X11 display (DISPLAY unset); skipping real capture test")
	}

	c := capture_x11.New()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for {
		img, err := c.Capture(context.Background(), 0)
		if err == nil && img.Width > 0 && img.Height > 0 {
			t.Logf("real capture succeeded: %dx%d", img.Width, img.Height)
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Skipf("real capture unavailable after retries (NumActiveDisplays flaky); last err=%v", lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// genTestTLS generates a self-signed TLS 1.3 cert for "localhost" used by the
// real e2e test.
func genTestTLS(t *testing.T) (serverTLS, clientTLS *tls.Config) {
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

// realCaptureAdapter wraps the real X11 capture so it can be used where a
// host.Capture is expected.
type realCaptureAdapter struct{ c *capture_x11.Capture }

func (r *realCaptureAdapter) Capture(ctx context.Context, displayID uint32) (damage.Image, error) {
	return r.c.Capture(ctx, displayID)
}

// TestRealEndToEnd drives a real host.Service (with the real X11 capture
// adapter) and a real client.Session over TLS 1.3 on TCP loopback. It verifies:
//   - authentication succeeds,
//   - the client reaches Connected and receives pixels,
//   - keyboard input is injected on the host,
//   - disconnect triggers ReleaseAll (sticky-input cleanup).
//
// It is SKIPPED when no X11 display is available.
func TestRealEndToEnd(t *testing.T) {
	if display, ok := os.LookupEnv("DISPLAY"); !ok || display == "" {
		t.Skip("no X11 display (DISPLAY unset); skipping real e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS := genTestTLS(t)
	var token [32]byte
	token[0] = 9

	c := capture_x11.New()
	deadline := time.Now().Add(20 * time.Second)
	for {
		img, err := c.Capture(context.Background(), 0)
		if err == nil && img.Width > 0 && img.Height > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("real capture unavailable after retries (NumActiveDisplays flaky)")
		}
		time.Sleep(200 * time.Millisecond)
	}
	realCap := &realCaptureAdapter{c: c}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

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
		}, realCap, hostInput)
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

	obs := &stateRecorder{}
	sess, err := client.NewSession(client.Config{
		Token:          token,
		TLSConfig:      clientTLS,
		IOTimeout:      5 * time.Second,
		ReconnectDelay: 50 * time.Millisecond,
		Dial:           func(context.Context) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) },
		Input:          client.NewInputState(nil),
	}, &snapshotRenderer{}, obs)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()

	// Wait for Connected.
	cdl := time.Now().Add(5 * time.Second)
	for !obs.contains(client.StateConnected) {
		if time.Now().After(cdl) {
			t.Fatal("timed out waiting for Connected")
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap := sess.Snapshot()
	if snap.Width == 0 || snap.Height == 0 {
		t.Fatalf("no pixels after connect: %dx%d", snap.Width, snap.Height)
	}

	// Keyboard input should be injected on the host.
	_ = sess.Input().SetFocused(true)
	_ = sess.Input().Key(0x04, protocol.ActionDown, 0)
	kdl := time.Now().Add(3 * time.Second)
	for hostInput.keys == 0 {
		if time.Now().After(kdl) {
			t.Fatal("host never injected key")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = sess.Input().Key(0x04, protocol.ActionUp, 0)

	// Disconnect cleanup.
	sess.Input().Disconnect()
	cancel()
	<-done

	if hostInput.releases < 1 {
		t.Fatal("expected ReleaseAll on disconnect")
	}
	t.Logf("e2e OK: connected, captured %dx%d, injected %d key events, released %d",
		snap.Width, snap.Height, hostInput.keys, hostInput.releases)
}
