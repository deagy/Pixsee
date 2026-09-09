package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
)

// TestEphemeralServerCertificateIsUsableTLS13Server proves the no-CA path: an
// ephemeral self-signed certificate builds a working TLS 1.3 server that a
// client trusting that exact certificate can complete a full handshake and
// round trip through.
func TestEphemeralServerCertificateIsUsableTLS13Server(t *testing.T) {
	config, err := EphemeralServerTLSConfig()
	if err != nil {
		t.Fatalf("EphemeralServerTLSConfig: %v", err)
	}
	if config.MinVersion != tls.VersionTLS13 || config.MaxVersion != tls.VersionTLS13 {
		t.Fatal("ephemeral server config must be TLS 1.3 only")
	}
	if len(config.Certificates) != 1 || config.Certificates[0].PrivateKey == nil {
		t.Fatal("ephemeral server config must carry exactly one key-bearing certificate")
	}

	cert := config.Certificates[0]
	leaf := cert.Leaf
	if leaf == nil {
		parsed, perr := x509.ParseCertificate(cert.Certificate[0])
		if perr != nil {
			t.Fatal(perr)
		}
		leaf = parsed
	}
	clientConfig, err := ClientTLSConfigForCertificate("localhost", leaf)
	if err != nil {
		t.Fatalf("client config: %v", err)
	}

	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, config)
	clientTLS := tls.Client(clientRaw, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	serverResult := make(chan error, 1)
	var token [32]byte
	token[0] = 7
	go func() {
		if err := Handshake(ctx, serverTLS, time.Second); err != nil {
			serverResult <- err
			return
		}
		peer := NewPeer(serverTLS, protocol.RoleHost, protocol.DefaultLimits(), time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			serverResult <- err
			return
		}
		if _, err := peer.Receive(ctx); err != nil {
			serverResult <- err
			return
		}
		serverResult <- nil
	}()

	if err := Handshake(ctx, clientTLS, time.Second); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	client := NewPeer(clientTLS, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	if err := client.AuthenticateClient(ctx, token); err != nil {
		t.Fatalf("client auth: %v", err)
	}
	if err := client.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		t.Fatalf("client hello: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server result: %v", err)
	}
}

// TestEphemeralServerCertRejectedBySecureClient proves the ephemeral cert is a
// real self-signed cert the system pool does not trust: a secure client must
// reject the handshake.
func TestEphemeralServerCertRejectedBySecureClient(t *testing.T) {
	serverConfig, err := EphemeralServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, serverConfig)
	// Secure client: no roots, no fingerprint, no allow-insecure.
	clientConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		ServerName: "localhost",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- Handshake(ctx, serverTLS, time.Second) }()
	clientTLS := tls.Client(clientRaw, clientConfig)
	if err := Handshake(ctx, clientTLS, time.Second); err == nil {
		t.Fatal("secure client completed handshake against untrusted ephemeral host")
	}
	if err := <-serverResult; err == nil {
		t.Fatal("server handshake succeeded against a client that rejected its cert")
	}
}

// TestEphemeralServerCertAcceptedByInsecureClient proves the ephemeral cert is
// a real self-signed cert the system pool does not trust: an opt-in insecure
// client can complete the handshake.
func TestEphemeralServerCertAcceptedByInsecureClient(t *testing.T) {
	serverConfig, err := EphemeralServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, serverConfig)
	clientConfig, err := ClientTLSConfigForInsecure("localhost")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- Handshake(ctx, serverTLS, time.Second) }()
	clientTLS := tls.Client(clientRaw, clientConfig)
	if err := Handshake(ctx, clientTLS, time.Second); err != nil {
		t.Fatalf("insecure client failed handshake against ephemeral host: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server handshake failed: %v", err)
	}
}

// TestAuthenticateHostAcceptsAnyNonZeroTokenWhenNoToken proves the no-auth path:
// a host with a zero expected token accepts any non-zero client token.
func TestAuthenticateHostAcceptsAnyNonZeroTokenWhenNoToken(t *testing.T) {
	server, client := testTLSPeers(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var expected, supplied [32]byte
	// expected stays zero (no token configured on the host).
	supplied[0], supplied[1] = 1, 2

	result := make(chan error, 1)
	go func() { result <- server.AuthenticateHost(ctx, expected) }()
	if err := client.AuthenticateClient(ctx, supplied); err != nil {
		t.Fatalf("client auth: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("zero-token host rejected a non-zero client token: %v", err)
	}
}

// TestAuthenticateHostStillMatchesWhenTokenProvided is the regression guard for
// the provided-material path: a non-zero expected token still requires the
// matching client token.
func TestAuthenticateHostStillMatchesWhenTokenProvided(t *testing.T) {
	server, client := testTLSPeers(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var expected, supplied [32]byte
	expected[0] = 9
	supplied[0] = 9

	result := make(chan error, 1)
	go func() { result <- server.AuthenticateHost(ctx, expected) }()
	if err := client.AuthenticateClient(ctx, supplied); err != nil {
		t.Fatalf("client auth: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("matching token rejected: %v", err)
	}
}
