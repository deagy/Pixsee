package transport

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"crypto/tls"

	"virtualdesktop/internal/protocol"
)

func TestTLSAuthenticationAndRoundTrip(t *testing.T) {
	certificate, leaf := testCertificate(t)
	serverConfig, err := ServerTLSConfig(certificate)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := ClientTLSConfigForCertificate("localhost", leaf)
	if err != nil {
		t.Fatal(err)
	}

	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, serverConfig)
	clientTLS := tls.Client(clientRaw, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	serverResult := make(chan error, 1)
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
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
		message, err := peer.Receive(ctx)
		if err == nil {
			if _, ok := message.(protocol.ClientHello); !ok {
				err = errors.New("expected CLIENT_HELLO")
			}
		}
		if err == nil {
			err = peer.Send(ctx, protocol.ServerHello{Version: protocol.Version1})
		}
		if err == nil {
			err = peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888})
		}
		if err == nil {
			message, err = peer.Receive(ctx)
		}
		if err == nil {
			_, ok := message.(protocol.Key)
			if !ok {
				err = errors.New("received unexpected message")
			}
		}
		serverResult <- err
	}()

	if err := Handshake(ctx, clientTLS, time.Second); err != nil {
		t.Fatal(err)
	}
	client := NewPeer(clientTLS, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	if err := client.AuthenticateClient(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(ctx, protocol.Key{Generation: 1, InputSequence: 1, Usage: 4, Action: protocol.ActionDown}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticationRejectsWrongTokenAndCloses(t *testing.T) {
	server, client := testTLSPeers(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var expected, supplied [32]byte
	expected[0], supplied[0] = 1, 2
	result := make(chan error, 1)
	go func() { result <- server.AuthenticateHost(ctx, expected) }()
	if err := client.AuthenticateClient(ctx, supplied); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrAuthentication) {
		t.Fatalf("got %v", err)
	}
	if err := client.Send(ctx, protocol.Ping{}); err == nil {
		t.Fatal("connection remained usable after failed authentication")
	}
}

func TestAuthenticationRejectsPlaintext(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	peer := NewPeer(left, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	if err := peer.AuthenticateClient(context.Background(), [32]byte{}); !errors.Is(err, ErrTLSRequired) {
		t.Fatalf("got %v", err)
	}
}

func TestClientTLSConfigForInsecureSkipsVerification(t *testing.T) {
	config, err := ClientTLSConfigForInsecure("localhost")
	if err != nil {
		t.Fatal(err)
	}
	if config == nil {
		t.Fatal("expected insecure config")
	}
	if !config.InsecureSkipVerify {
		t.Fatal("insecure config must disable certificate verification")
	}
	if config.MinVersion != tls.VersionTLS13 || config.MaxVersion != tls.VersionTLS13 {
		t.Fatal("insecure config must remain TLS 1.3")
	}

	if _, err := ClientTLSConfigForInsecure(""); err == nil {
		t.Fatal("expected error for empty server name")
	}
}

func TestInsecureClientHandshakesWithUntrustedSelfSignedServer(t *testing.T) {
	// The server presents a self-signed certificate that is NOT trusted by any
	// root. A secure client would reject the handshake; the insecure client
	// (InsecureSkipVerify) must still complete it.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certChain := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}

	serverConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certChain},
	}
	clientConfig, err := ClientTLSConfigForInsecure("localhost")
	if err != nil {
		t.Fatal(err)
	}

	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, serverConfig)
	clientTLS := tls.Client(clientRaw, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	serverResult := make(chan error, 1)
	go func() { serverResult <- Handshake(ctx, serverTLS, time.Second) }()
	if err := Handshake(ctx, clientTLS, time.Second); err != nil {
		t.Fatalf("insecure client failed handshake against untrusted self-signed server: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server handshake failed: %v", err)
	}
}

func TestSecureClientRejectsUntrustedSelfSignedServer(t *testing.T) {
	// The server presents a self-signed certificate the client has never seen
	// and does not trust via system roots. A secure client MUST reject the
	// handshake, proving the insecure path is meaningfully different.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certChain := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	serverConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certChain},
	}
	// No RootCAs: rely on the system pool, which will not contain our
	// throwaway self-signed certificate.
	clientConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		ServerName: "localhost",
	}

	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, serverConfig)
	clientTLS := tls.Client(clientRaw, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	serverResult := make(chan error, 1)
	go func() { serverResult <- Handshake(ctx, serverTLS, time.Second) }()
	if err := Handshake(ctx, clientTLS, time.Second); err == nil {
		t.Fatal("secure client completed handshake against untrusted self-signed server")
	}
	<-serverResult
}

func TestTLSIsVersion13OnlyAndFingerprintPinned(t *testing.T) {
	certificate, leaf := testCertificate(t)
	serverConfig, err := ServerTLSConfig(certificate)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	clientConfig, err := ClientTLSConfigForFingerprint(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if serverConfig.MinVersion != tls.VersionTLS13 || serverConfig.MaxVersion != tls.VersionTLS13 || clientConfig.MinVersion != tls.VersionTLS13 || clientConfig.MaxVersion != tls.VersionTLS13 {
		t.Fatal("TLS configuration permits a version other than TLS 1.3")
	}

	wrongFingerprint := fingerprint
	wrongFingerprint[0] ^= 0xff
	wrongConfig, err := ClientTLSConfigForFingerprint(wrongFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	defer clientRaw.Close()
	serverTLS := tls.Server(serverRaw, serverConfig)
	clientTLS := tls.Client(clientRaw, wrongConfig)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- Handshake(ctx, serverTLS, time.Second) }()
	if err := Handshake(ctx, clientTLS, time.Second); !errors.Is(err, ErrCertificatePin) {
		t.Fatalf("got %v", err)
	}
	<-serverResult
}

func TestCloseTerminatesConnectionAndMarksPeerClosed(t *testing.T) {
	left, right := net.Pipe()
	peer := NewPeer(left, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if peer.State() != protocol.StateClosed {
		t.Fatalf("got state %v", peer.State())
	}
	if _, err := right.Write([]byte{1}); err == nil {
		t.Fatal("connection remained open")
	}
}

func TestReceiveTimesOutAndClosesConnection(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	peer := NewPeer(left, protocol.RoleClient, protocol.DefaultLimits(), 10*time.Millisecond)
	if _, err := peer.Receive(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if _, err := right.Write([]byte{1}); err == nil {
		t.Fatal("connection remained open after read timeout")
	}
}

func TestPeerEnforcesDirection(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	client := NewPeer(left, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	if err := client.Send(context.Background(), protocol.Frame{}); !errors.Is(err, protocol.ErrDirection) {
		t.Fatalf("got %v", err)
	}
}

func TestPeerRejectsApplicationMessageBeforeAuthentication(t *testing.T) {
	server, client := testTLSPeers(t)
	message := protocol.Key{Generation: 1, InputSequence: 1, Usage: 4, Action: protocol.ActionDown}
	if err := client.Send(context.Background(), message); !errors.Is(err, protocol.ErrState) {
		t.Fatalf("got %v", err)
	}
	if server.State() != protocol.StateAuthenticating || client.State() != protocol.StateAuthenticating {
		t.Fatal("rejected message changed peer state")
	}
}

func TestPeerNegotiatesBeforeApplicationMessages(t *testing.T) {
	server, client := testTLSPeers(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var token [32]byte
	token[0] = 1

	hostResult := make(chan error, 1)
	go func() { hostResult <- server.AuthenticateHost(ctx, token) }()
	if err := client.AuthenticateClient(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}

	hostResult = make(chan error, 1)
	go func() {
		message, err := server.Receive(ctx)
		if err == nil {
			if _, ok := message.(protocol.ClientHello); !ok {
				err = errors.New("expected CLIENT_HELLO")
			}
		}
		hostResult <- err
	}()
	if err := client.Send(ctx, protocol.ClientHello{MinVersion: 1, MaxVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}

	clientResult := make(chan error, 1)
	go func() {
		_, err := client.Receive(ctx)
		if err == nil {
			_, err = client.Receive(ctx)
		}
		clientResult <- err
	}()
	if err := server.Send(ctx, protocol.ServerHello{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := server.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	if err := <-clientResult; err != nil {
		t.Fatal(err)
	}
	if server.State() != protocol.StateActive || client.State() != protocol.StateActive {
		t.Fatalf("states: server=%v client=%v", server.State(), client.State())
	}
}

func TestReceiveHonorsCancellation(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	peer := NewPeer(left, protocol.RoleClient, protocol.DefaultLimits(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := peer.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("canceled receive did not return promptly")
	}
}

func TestBoundedQueueRejectsOverflowAndTerminates(t *testing.T) {
	queue, err := NewQueue[protocol.Message](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.TrySend(protocol.Ping{Nonce: 1}); err != nil {
		t.Fatal(err)
	}
	if err := queue.TrySend(protocol.Ping{Nonce: 2}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("got %v", err)
	}
	queue.Close()
	if _, ok := <-queue.Receive(); !ok {
		t.Fatal("queued item was discarded")
	}
	if _, ok := <-queue.Receive(); ok {
		t.Fatal("closed queue did not terminate")
	}
	if err := queue.TrySend(protocol.Ping{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func testTLSPeers(t *testing.T) (*Peer, *Peer) {
	t.Helper()
	certificate, leaf := testCertificate(t)
	serverConfig, err := ServerTLSConfig(certificate)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := ClientTLSConfigForCertificate("localhost", leaf)
	if err != nil {
		t.Fatal(err)
	}
	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, serverConfig)
	clientTLS := tls.Client(clientRaw, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	serverResult := make(chan error, 1)
	go func() { serverResult <- Handshake(ctx, serverTLS, time.Second) }()
	if err := Handshake(ctx, clientTLS, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	server := NewPeer(serverTLS, protocol.RoleHost, protocol.DefaultLimits(), time.Second)
	client := NewPeer(clientTLS, protocol.RoleClient, protocol.DefaultLimits(), time.Second)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return server, client
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}
