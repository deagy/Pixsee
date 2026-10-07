package main

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// flowCapture serves one fixed capture to handleConnection.
type flowCapture struct{ image damage.Image }

func (c flowCapture) Capture(context.Context, uint32) (damage.Image, error) {
	out := c.image
	out.Pixels = append([]byte(nil), c.image.Pixels...)
	return out, nil
}

func flowNoisePixels(w, h int) []byte {
	p := make([]byte, w*h*4)
	var s uint64 = 0x9e3779b97f4a7c15
	for i := range p {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		p[i] = byte(s)
	}
	return p
}

// negotiateThroughHandleConnection runs a real TLS handshake and HELLO exchange
// against handleConnection and returns the negotiated SERVER_HELLO plus the
// DISPLAY_CONFIG and first visual message that follow it.
func negotiateThroughHandleConnection(t *testing.T, capture host.Capture, clientMin, clientMax, serverMax uint16) (protocol.ServerHello, protocol.DisplayConfig, protocol.Message) {
	t.Helper()
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
		capture:            capture,
		input:              func() (host.Input, error) { return noopHandleConnInput{}, nil },
		maxProtocolVersion: serverMax,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	peer := transport.NewPeerConn(clientTLS, rawConn, protocol.RoleClient, protocol.DefaultLimits(), 30*time.Second)

	var token [32]byte
	token[0] = 1
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		t.Fatalf("AuthenticateClient: %v", err)
	}
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: clientMin, MaxVersion: clientMax}); err != nil {
		t.Fatalf("send CLIENT_HELLO: %v", err)
	}
	// The client does not know the negotiated version until SERVER_HELLO
	// arrives, so accept the whole offered range while awaiting it.
	peer.SetVersionWindow(clientMin, clientMax)

	message, err := peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive SERVER_HELLO: %v", err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok {
		t.Fatalf("expected SERVER_HELLO, got %T", message)
	}
	peer.PinVersion(hello.Version)

	message, err = peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive DISPLAY_CONFIG: %v", err)
	}
	display, ok := message.(protocol.DisplayConfig)
	if !ok {
		t.Fatalf("expected DISPLAY_CONFIG, got %T", message)
	}
	visual, err := peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive first visual message: %v", err)
	}

	cancel()
	<-acceptDone
	return hello, display, visual
}

// TestHandleConnectionThreadsNegotiatedVersion proves cmd/vdhost passes the
// version actually negotiated for the session — not the server maximum — into
// host.Config: a v2 client keeps the native-resolution capture and gets
// FRAME_PARTs, which only happens when the service learns the negotiated v2.
func TestHandleConnectionThreadsNegotiatedVersion(t *testing.T) {
	native := damage.Image{Width: 3000, Height: 1500, Pixels: flowNoisePixels(3000, 1500)}
	hello, display, visual := negotiateThroughHandleConnection(t, flowCapture{image: native}, protocol.Version1, protocol.Version2, protocol.Version2)
	if hello.Version != protocol.Version2 {
		t.Fatalf("negotiated version = %d, want v2", hello.Version)
	}
	if display.Width != 3000 || display.Height != 1500 {
		t.Fatalf("v2 client received %dx%d DISPLAY_CONFIG; a v1-defaulting service would downscale it", display.Width, display.Height)
	}
	if _, ok := visual.(protocol.FramePart); !ok {
		t.Fatalf("v2 client first visual = %T, want FRAME_PART for a native 3000x1500 keyframe", visual)
	}
}

// TestHandleConnectionDownscalesForV1Client proves the negotiated v1 path: a v1
// client receives downscaled dimensions and an ordinary FRAME, never a
// FRAME_PART (which its envelope could not even decode).
func TestHandleConnectionDownscalesForV1Client(t *testing.T) {
	native := damage.Image{Width: 3000, Height: 1500, Pixels: flowNoisePixels(3000, 1500)}
	hello, display, visual := negotiateThroughHandleConnection(t, flowCapture{image: native}, protocol.Version1, protocol.Version1, protocol.Version2)
	if hello.Version != protocol.Version1 {
		t.Fatalf("negotiated version = %d, want v1", hello.Version)
	}
	if display.Width != 1500 || display.Height != 750 {
		t.Fatalf("v1 client display = %dx%d, want downscaled 1500x750", display.Width, display.Height)
	}
	if _, ok := visual.(protocol.Frame); !ok {
		t.Fatalf("v1 client first visual = %T, want an ordinary FRAME", visual)
	}
}
