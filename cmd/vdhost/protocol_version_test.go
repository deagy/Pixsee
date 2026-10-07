package main

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// TestMaxProtocolVersionConfig pins the vdhost negotiation opt-in: the shipped
// default offers v2, an operator may pin down to v1, and any other value is
// rejected at config load.
func TestMaxProtocolVersionConfig(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    uint16
		wantErr bool
	}{
		{"default offers v2", []string{"-no-auth"}, protocol.Version2, false},
		{"explicit v1", []string{"-no-auth", "-max-protocol-version", "1"}, protocol.Version1, false},
		{"explicit v2", []string{"-no-auth", "-max-protocol-version", "2"}, protocol.Version2, false},
		{"unsupported version rejected", []string{"-no-auth", "-max-protocol-version", "3"}, 0, true},
		{"zero version rejected", []string{"-no-auth", "-max-protocol-version", "0"}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cfg=%+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.maxProtocolVersion != tc.want {
				t.Fatalf("maxProtocolVersion = %d, want %d", cfg.maxProtocolVersion, tc.want)
			}
		})
	}
}

// TestHandleConnectionNegotiatesV2WhenConfigured proves the shipped host path
// opts into v2: handleConnection passes the configured max into session.Accept,
// which answers a {1,2} client with a v2 SERVER_HELLO on a v2 record envelope.
func TestHandleConnectionNegotiatesV2WhenConfigured(t *testing.T) {
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
		tlsConfig:          serverTLS,
		captureInterval:    10 * time.Millisecond,
		keyframeInterval:   time.Hour,
		maxInputEvents:     1000,
		enableInput:        false,
		ioTimeout:          5 * time.Second,
		capture:            fakeHandleConnCapture{},
		input:              func() (host.Input, error) { return noopHandleConnInput{}, nil },
		maxProtocolVersion: protocol.Version2,
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

	rawConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer rawConn.Close()
	clientTLS := tls.Client(rawConn, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, InsecureSkipVerify: true})
	if err := transport.Handshake(ctx, clientTLS, 5*time.Second); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	peer := transport.NewPeerConn(clientTLS, rawConn, protocol.RoleClient, protocol.DefaultLimits(), 5*time.Second)

	var token [32]byte
	token[0] = 1
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		t.Fatalf("AuthenticateClient: %v", err)
	}
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version2}); err != nil {
		t.Fatalf("send CLIENT_HELLO: %v", err)
	}

	// Accept only a v2 envelope, so a v1 SERVER_HELLO would fail this receive.
	peer.SetVersionWindow(protocol.Version2, protocol.Version2)
	message, err := peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive SERVER_HELLO on v2 envelope: %v", err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok || hello.Version != protocol.Version2 {
		t.Fatalf("expected SERVER_HELLO v2, got %#v", message)
	}
	message, err = peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive DISPLAY_CONFIG: %v", err)
	}
	if _, ok := message.(protocol.DisplayConfig); !ok {
		t.Fatalf("expected DISPLAY_CONFIG after SERVER_HELLO, got %T", message)
	}

	cancel()
	<-acceptDone
}
