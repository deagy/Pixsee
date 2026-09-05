package integration

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// TestReconnectAfterDisconnect verifies that when the host closes the
// connection, the client reports a reconnect attempt. A persistent host listens
// for multiple connections; the first is closed after auth so the client
// reconnects.
func TestReconnectAfterDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Persistent host: accept up to two connections.
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				serverConn := tls.Server(c, serverTLS.Clone())
				if err := transport.Handshake(ctx, serverConn, 5*time.Second); err != nil {
					return
				}
				peer := transport.NewPeerConn(serverConn, c, protocol.RoleHost, protocol.DefaultLimits(), 5*time.Second)
				if err := peer.AuthenticateHost(ctx, token); err != nil {
					return
				}
				if _, err := peer.Receive(ctx); err != nil {
					return
				}
				if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version1}); err != nil {
					return
				}
				// Close immediately after hello to force a client reconnect.
				_ = c.Close()
			}(conn)
		}
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "reconnect attempt", func() bool {
		return observer.contains(client.StateReconnecting)
	})
	cancel()
	<-done
}

// TestPingPongExchange verifies that the client responds to a host Ping with a
// Pong that the host receives.
func TestPingPongExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
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
		}, cap, input)
		serverConn := tls.Server(conn, serverTLS.Clone())
		if err := transport.Handshake(ctx, serverConn, 5*time.Second); err != nil {
			return
		}
		peer := recordingPeer{Peer: transport.NewPeerConn(serverConn, conn, protocol.RoleHost, protocol.DefaultLimits(), 5*time.Second), log: log}
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			return
		}
		if _, err := peer.Receive(ctx); err != nil {
			return
		}
		if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version1}); err != nil {
			return
		}
		runCtx, cancel := context.WithCancel(ctx)
		go func() {
			defer cancel()
			_ = svc.Run(runCtx, &peer)
		}()
		time.Sleep(50 * time.Millisecond)
		_ = peer.Send(ctx, protocol.Ping{Nonce: 0x1234})
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	waitFor(t, 5*time.Second, "pong received by host", func() bool {
		return log.clientKinds()["PONG"] >= 1
	})
	cancel()
	<-done
}

// TestMalformedMagicRejected verifies that a connection carrying a bad protocol
// magic is rejected: the client never reaches Connected.
func TestMalformedMagicRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

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
		tlsConn := tls.Server(conn, serverTLS.Clone())
		if err := transport.Handshake(ctx, tlsConn, 5*time.Second); err != nil {
			return
		}
		header := make([]byte, protocol.HeaderSize)
		copy(header[0:4], "XXXX") // bad magic
		_, _ = conn.Write(header)
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	// The bad magic arrives after the TLS handshake, so the client reports an
	// error. On a live context it retries forever, so verify the error state
	// and then cancel.
	waitFor(t, 5*time.Second, "error state", func() bool {
		return observer.contains(client.StateError)
	})
	if observer.contains(client.StateConnected) {
		t.Fatal("client reached Connected after bad magic")
	}
	cancel()
	<-done
}

// TestOversizedFrameRejected crafts a raw, properly-framed frame whose length
// field exceeds MaxPixelPayload. The decoder must reject it with
// ErrMessageTooLarge before allocating the payload, and the client peer must
// fail closed (closing the connection). This is a fail-closed check against a
// malicious host.
func TestOversizedFrameRejected(t *testing.T) {
	// Craft a frame header with an oversized length field.
	buf := make([]byte, protocol.HeaderSize)
	copy(buf[0:4], protocol.Magic)
	binary.BigEndian.PutUint16(buf[4:6], protocol.Version1)
	binary.BigEndian.PutUint16(buf[6:8], uint16(protocol.TypeFrame))
	binary.BigEndian.PutUint16(buf[8:10], 0) // flags
	binary.BigEndian.PutUint16(buf[10:12], 0) // reserved
	binary.BigEndian.PutUint32(buf[12:16], protocol.MaxPixelPayloadHard+1)
	binary.BigEndian.PutUint64(buf[16:24], 1) // sequence

	serverRaw, clientRaw := net.Pipe()
	// The decoder's sequence starts at 0, so the first message must be
	// sequence 1 (the oversized frame below carries sequence 1).
	peer := transport.NewPeerConn(clientRaw, clientRaw, protocol.RoleClient, protocol.DefaultLimits(), time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := peer.Receive(context.Background())
		done <- err
	}()

	_, err := serverRaw.Write(buf)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, protocol.ErrMessageTooLarge) {
			t.Fatalf("expected ErrMessageTooLarge, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peer did not reject oversized frame")
	}
}

// TestDirectionalInvalidMessageRejected verifies that a client sending a
// host-only message (FRAME) in the wrong direction is rejected by the peer's
// direction validation.
func TestDirectionalInvalidMessageRejected(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	defer clientRaw.Close()
	peer := transport.NewPeerConn(clientRaw, clientRaw, protocol.RoleClient, protocol.DefaultLimits(), time.Second)

	err := peer.Send(context.Background(), protocol.Frame{
		Generation: 1, FrameSequence: 1, BaseFrameSequence: 0,
		Keyframe: true, Rectangles: []protocol.Rectangle{
			{Width: 1, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: []byte{0, 0, 0, 0}},
		},
	})
	if !errors.Is(err, protocol.ErrDirection) {
		t.Fatalf("expected ErrDirection for client sending FRAME, got %v", err)
	}
}
