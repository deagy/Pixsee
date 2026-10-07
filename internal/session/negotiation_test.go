package session

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// acceptInBackground runs Accept in a goroutine and returns the result on one
// of two channels so a test can drive the client side in the foreground.
func acceptInBackground(ctx context.Context, conn net.Conn, cfg Config) (<-chan *Established, <-chan error) {
	accepted := make(chan *Established, 1)
	acceptErr := make(chan error, 1)
	go func() {
		established, err := Accept(ctx, conn, cfg)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- established
	}()
	return accepted, acceptErr
}

// dialSessionPeer runs the client half of establishment: TLS handshake and
// AUTH, leaving the peer ready to send CLIENT_HELLO.
func dialSessionPeer(t *testing.T, ctx context.Context, clientTLS *tls.Config, clientRaw net.Conn, token [32]byte) *transport.Peer {
	t.Helper()
	clientConn := tls.Client(clientRaw, clientTLS)
	if err := transport.Handshake(ctx, clientConn, time.Second); err != nil {
		t.Fatal(err)
	}
	peer := transport.NewPeerConn(clientConn, clientRaw, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		t.Fatal(err)
	}
	return peer
}

func waitEstablished(t *testing.T, ctx context.Context, accepted <-chan *Established, acceptErr <-chan error) *Established {
	t.Helper()
	select {
	case established := <-accepted:
		return established
	case err := <-acceptErr:
		t.Fatalf("accept failed: %v", err)
	case <-ctx.Done():
		t.Fatal("accept did not return")
	}
	return nil
}

// TestAcceptNegotiatesV1ForV2ClientHello is the v1.4.1-style downgrade: a
// deployed v1 host (ServerMaxVersion zero, i.e. v1) receives a v2 offer {1,2},
// clamps it to v1 on the first handshake, and answers with a v1 SERVER_HELLO.
// No retry is involved.
func TestAcceptNegotiatesV1ForV2ClientHello(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	var token [32]byte
	token[0] = 7
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	accepted, acceptErr := acceptInBackground(ctx, serverRaw, Config{Token: token, TLSConfig: serverTLS, IOTimeout: time.Second})
	peer := dialSessionPeer(t, ctx, clientTLS, clientRaw, token)
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version2}); err != nil {
		t.Fatal(err)
	}
	message, err := peer.Receive(ctx) // default v1-only decoder: the host must answer v1
	if err != nil {
		t.Fatal(err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok || hello.Version != protocol.Version1 {
		t.Fatalf("expected SERVER_HELLO v1, got %#v", message)
	}
	established := waitEstablished(t, ctx, accepted, acceptErr)
	defer established.Peer.Close()
	if established.Version != protocol.Version1 {
		t.Fatalf("negotiated version = %d, want 1", established.Version)
	}
}

// TestAcceptNegotiatesV2WhenOptedIn proves a host with ServerMaxVersion v2
// answers a {1,2} offer with a v2 SERVER_HELLO on a v2 record envelope (the
// client decoder here accepts only v2, so a v1 envelope would fail).
func TestAcceptNegotiatesV2WhenOptedIn(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	var token [32]byte
	token[0] = 8
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	accepted, acceptErr := acceptInBackground(ctx, serverRaw, Config{Token: token, TLSConfig: serverTLS, IOTimeout: time.Second, ServerMaxVersion: protocol.Version2})
	peer := dialSessionPeer(t, ctx, clientTLS, clientRaw, token)
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version2}); err != nil {
		t.Fatal(err)
	}
	peer.SetVersionWindow(protocol.Version2, protocol.Version2)
	message, err := peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive SERVER_HELLO on v2 envelope: %v", err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok || hello.Version != protocol.Version2 {
		t.Fatalf("expected SERVER_HELLO v2, got %#v", message)
	}
	established := waitEstablished(t, ctx, accepted, acceptErr)
	defer established.Peer.Close()
	if established.Version != protocol.Version2 {
		t.Fatalf("negotiated version = %d, want 2", established.Version)
	}
}

// TestAcceptV2HostDowngradesV1Client is the mixed-session case: a v2-capable
// host still serves a v1 client on v1, so the Phase 2 downscale fallback has a
// v1 session to serve.
func TestAcceptV2HostDowngradesV1Client(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	var token [32]byte
	token[0] = 9
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	accepted, acceptErr := acceptInBackground(ctx, serverRaw, Config{Token: token, TLSConfig: serverTLS, IOTimeout: time.Second, ServerMaxVersion: protocol.Version2})
	peer := dialSessionPeer(t, ctx, clientTLS, clientRaw, token)
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		t.Fatal(err)
	}
	message, err := peer.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok || hello.Version != protocol.Version1 {
		t.Fatalf("expected SERVER_HELLO v1, got %#v", message)
	}
	established := waitEstablished(t, ctx, accepted, acceptErr)
	defer established.Peer.Close()
	if established.Version != protocol.Version1 {
		t.Fatalf("negotiated version = %d, want 1", established.Version)
	}
}

// TestAcceptRejectsNoCommonVersion proves a v2-only client against a v1 host
// fails closed with ErrNoCommonVersion rather than silently proceeding.
func TestAcceptRejectsNoCommonVersion(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	var token [32]byte
	token[0] = 10
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, acceptErr := acceptInBackground(ctx, serverRaw, Config{Token: token, TLSConfig: serverTLS, IOTimeout: time.Second})
	peer := dialSessionPeer(t, ctx, clientTLS, clientRaw, token)
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version2, MaxVersion: protocol.Version2}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acceptErr:
		if !errors.Is(err, protocol.ErrNoCommonVersion) {
			t.Fatalf("got %v, want ErrNoCommonVersion", err)
		}
	case <-ctx.Done():
		t.Fatal("accept did not reject the v2-only client")
	}
}

func sessionTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	server, err := transport.ServerTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	client, err := transport.ClientTLSConfigForCertificate("localhost", leaf)
	if err != nil {
		t.Fatal(err)
	}
	return server, client
}
