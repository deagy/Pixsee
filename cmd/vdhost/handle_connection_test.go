package main

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

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// fakeCapture serves a fixed solid image, enough for handleConnection to
// produce an initial keyframe without any platform capture dependency.
type fakeHandleConnCapture struct{}

func (fakeHandleConnCapture) Capture(context.Context, uint32) (damage.Image, error) {
	w, h := uint32(2), uint32(1)
	pixels := make([]byte, w*h*4)
	return damage.Image{Width: w, Height: h, Pixels: pixels}, nil
}

type noopHandleConnInput struct{}

func (noopHandleConnInput) Key(context.Context, uint16, protocol.Action, uint8) error { return nil }
func (noopHandleConnInput) Move(context.Context, uint32, uint32) error                { return nil }
func (noopHandleConnInput) Button(context.Context, protocol.Button, protocol.Action) error {
	return nil
}
func (noopHandleConnInput) Wheel(context.Context, int16, int16) error { return nil }
func (noopHandleConnInput) ReleaseAll(context.Context) error          { return nil }

func generateHandleConnCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestHandleConnectionPerformsHelloNegotiationBeforeServiceLoop is a
// regression test for the "no GUI + continuous error stream" bug: vdhost's
// handleConnection MUST exchange CLIENT_HELLO/SERVER_HELLO before invoking
// host.Service.Run, exactly as internal/client.Session.ServeConn expects.
// Without the exchange, the peer's Negotiating state rejects the service's
// first DISPLAY_CONFIG send as invalid-for-state, the connection is
// silently torn down, and the client spins on a "receive: context deadline
// exceeded" reconnect loop forever with no window ever appearing.
func TestHandleConnectionPerformsHelloNegotiationBeforeServiceLoop(t *testing.T) {
	cert := generateHandleConnCert(t)
	serverTLS, err := transport.ServerTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cfg := &appConfig{
		tlsConfig:        serverTLS,
		captureInterval:  10 * time.Millisecond,
		keyframeInterval: time.Hour,
		maxInputEvents:   1000,
		enableInput:      false,
		ioTimeout:        5 * time.Second,
		capture:          fakeHandleConnCapture{},
		input:            func() (host.Input, error) { return noopHandleConnInput{}, nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handleConnection(ctx, cfg, conn)
	}()

	// Client side: mirrors internal/client.Session.ServeConn's exact
	// sequence (TLS handshake, AUTH, CLIENT_HELLO, expect SERVER_HELLO then
	// DISPLAY_CONFIG) so this test fails the same way a real client would if
	// the host skips negotiation.
	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer rawConn.Close()
	clientTLS := tls.Client(rawConn, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	})
	if err := transport.Handshake(ctx, clientTLS, 5*time.Second); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	peer := transport.NewPeerConn(clientTLS, rawConn, protocol.RoleClient, protocol.DefaultLimits(), 5*time.Second)

	var token [32]byte
	token[0] = 1
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		t.Fatalf("AuthenticateClient: %v", err)
	}
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		t.Fatalf("send CLIENT_HELLO: %v", err)
	}

	message, err := peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive SERVER_HELLO: %v (this is the exact symptom of the regression: the client times out waiting for a hello reply)", err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok {
		t.Fatalf("expected SERVER_HELLO, got %T", message)
	}
	if hello.Version != protocol.Version1 {
		t.Fatalf("SERVER_HELLO version = %d, want %d", hello.Version, protocol.Version1)
	}

	message, err = peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive DISPLAY_CONFIG: %v", err)
	}
	if _, ok := message.(protocol.DisplayConfig); !ok {
		t.Fatalf("expected DISPLAY_CONFIG after SERVER_HELLO, got %T", message)
	}

	message, err = peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive initial FRAME: %v", err)
	}
	frame, ok := message.(protocol.Frame)
	if !ok || !frame.Keyframe {
		t.Fatalf("expected initial keyframe FRAME, got %#v", message)
	}

	cancel()
	<-acceptDone
}
