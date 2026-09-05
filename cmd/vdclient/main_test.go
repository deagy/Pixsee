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

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

func TestBuildTLSConfigFingerprint(t *testing.T) {
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	cfg, err := buildTLSConfig("", "localhost:6511", hexString(fp), "")
	if err != nil {
		t.Fatalf("buildTLSConfig fingerprint: %v", err)
	}
	if cfg == nil || cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("expected TLS 1.3 fingerprint config, got %+v", cfg)
	}
}

func TestBuildTLSConfigCA(t *testing.T) {
	dir := t.TempDir()
	caCert, _ := generateSelfSigned(t)
	path := dir + "/ca.pem"
	writePEM(t, path, "CERTIFICATE", caCert)

	cfg, err := buildTLSConfig("", "localhost:6511", "", path)
	if err != nil {
		t.Fatalf("buildTLSConfig ca: %v", err)
	}
	if cfg == nil || cfg.ServerName != "localhost" {
		t.Fatalf("expected server-name localhost, got %+v", cfg)
	}
}

func TestBuildTLSConfigRequiresTrustMaterial(t *testing.T) {
	_, err := buildTLSConfig("", "localhost:6511", "", "")
	if err == nil {
		t.Fatal("expected error when neither fingerprint nor ca provided")
	}
}

func TestBuildTLSConfigBadFingerprint(t *testing.T) {
	_, err := buildTLSConfig("", "localhost:6511", "not-hex", "")
	if err == nil {
		t.Fatal("expected error for bad fingerprint")
	}
}

func TestDefaultDial(t *testing.T) {
	ln, err := listenTCP(t)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	dial := defaultDial(ln.Addr().String(), nil)
	conn, err := dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if conn.RemoteAddr().String() == "" {
		t.Fatal("expected remote addr")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	tokenPath := t.TempDir() + "/token"
	if err := writeFile(tokenPath, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	cfg, err := loadConfig([]string{"-addr", "example.com:6511", "-token", tokenPath, "-fingerprint", hexString(fp)})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != "example.com:6511" {
		t.Fatalf("addr = %q", cfg.addr)
	}
	if cfg.ioTimeout != 10*time.Second {
		t.Fatalf("ioTimeout = %v", cfg.ioTimeout)
	}
	if cfg.reconnectDelay != time.Second {
		t.Fatalf("reconnectDelay = %v", cfg.reconnectDelay)
	}
}

func TestLoadConfigRejectsMissingAddr(t *testing.T) {
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	_, err := loadConfig([]string{"-fingerprint", hexString(fp)})
	if err == nil {
		t.Fatal("expected error when addr missing")
	}
}

func TestLoadConfigRejectsBadToken(t *testing.T) {
	var fp [32]byte
	_, _ = rand.Read(fp[:])
	_, err := loadConfig([]string{"-addr", "example.com:6511", "-token", "/nonexistent/token", "-fingerprint", hexString(fp)})
	if err == nil {
		t.Fatal("expected error for bad token")
	}
}

func TestConnectAndAuthSucceeds(t *testing.T) {
	caCert, caKey := generateSelfSigned(t)
	dir := t.TempDir()
	caPath := dir + "/ca.pem"
	writePEM(t, caPath, "CERTIFICATE", caCert)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go serveFakeHost(t, ln, caCert, caKey, 9)

	cfg := &appConfig{
		addr:           ln.Addr().String(),
		token:          func() [32]byte { var t [32]byte; t[0] = 9; return t }(),
		tlsConfig:      clientTLSForCA(t, caCert),
		ioTimeout:      5 * time.Second,
		reconnectDelay: 100 * time.Millisecond,
	}
	if err := run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func clientTLSForCA(t *testing.T, cert *x509.Certificate) *tls.Config {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	cfg, err := transport.ClientTLSConfig("localhost", roots)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func serveFakeHost(t *testing.T, ln net.Listener, caCert *x509.Certificate, caKey interface{}, tokenByte byte) {
	t.Helper()
	serverTLS, err := transport.ServerTLSConfig(tls.Certificate{
		Certificate: [][]byte{caCert.Raw},
		PrivateKey:  caKey,
	})
	if err != nil {
		return
	}
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	tlsConn := tls.Server(conn, serverTLS)
	if err := transport.Handshake(context.Background(), tlsConn, 5*time.Second); err != nil {
		return
	}
	peer := transport.NewPeer(tlsConn, protocol.RoleHost, protocol.DefaultLimits(), 5*time.Second)
	var token [32]byte
	token[0] = tokenByte
	if err := peer.AuthenticateHost(context.Background(), token); err != nil {
		return
	}
	if _, err := peer.Receive(context.Background()); err != nil {
		return
	}
	_ = peer.Send(context.Background(), protocol.ServerHello{Version: 1})
	_ = peer.Send(context.Background(), protocol.DisplayConfig{
		Generation: 1, Width: 2, Height: 1, PixelFormat: protocol.PixelBGRA8888,
	})
	pixels := make([]byte, 2*1*4)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	_ = peer.Send(context.Background(), protocol.Frame{
		Generation: 1, FrameSequence: 1, Keyframe: true,
		Rectangles: []protocol.Rectangle{
			{Width: 2, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels},
		},
	})
	time.Sleep(200 * time.Millisecond)
	_ = peer.Send(context.Background(), protocol.Close{Code: 0, Reason: "done"})
}

func hexString(b [32]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i := 0; i < 32; i++ {
		out[i*2] = hexdigits[b[i]>>4]
		out[i*2+1] = hexdigits[b[i]&0xf]
	}
	return string(out)
}

func generateSelfSigned(t *testing.T) (*x509.Certificate, interface{}) {
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func writePEM(t *testing.T, path, blockType string, cert *x509.Certificate) {
	t.Helper()
	data := pemEncode(blockType, cert.Raw)
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
}

func listenTCP(t *testing.T) (net.Listener, error) {
	t.Helper()
	return net.Listen("tcp", "127.0.0.1:0")
}

var _ = tls.VersionTLS13
var _ = transport.ServerTLSConfig
