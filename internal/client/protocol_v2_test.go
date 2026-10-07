package client

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// TestClientAdvertisesV2AndPinsAfterServerHello drives the real client against
// a v2 host. It proves three things at once: the client advertises
// CLIENT_HELLO {1,2}; it accepts a v2 SERVER_HELLO envelope and pins to v2; and
// its later input rides a v2 envelope — the host's decoder is pinned to v2, so
// a v1-enveloped KEY would be rejected.
func TestClientAdvertisesV2AndPinsAfterServerHello(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	serverConn := tls.Server(serverRaw, serverTLS)
	clientConn := tls.Client(clientRaw, clientTLS)
	var token [32]byte
	token[0] = 11
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hostResult := make(chan error, 1)
	inputResult := make(chan protocol.Message, 1)
	go func() {
		if err := transport.Handshake(ctx, serverConn, time.Second); err != nil {
			hostResult <- err
			return
		}
		peer := transport.NewPeer(serverConn, protocol.RoleHost, protocol.DefaultLimits(), time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			hostResult <- err
			return
		}
		peer.SetVersionWindow(protocol.Version1, protocol.Version2)
		message, err := peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		hello, ok := message.(protocol.ClientHello)
		if !ok || hello.MinVersion != protocol.Version1 || hello.MaxVersion != protocol.Version2 {
			hostResult <- errUnexpected("CLIENT_HELLO {1,2}", message)
			return
		}
		peer.PinVersion(protocol.Version2)
		if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version2}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 2, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []protocol.Rectangle{{Width: 2, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(2, 1)}}}); err != nil {
			hostResult <- err
			return
		}
		message, err = peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		inputResult <- message
		hostResult <- peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &recordingRenderer{presented: make(chan Snapshot, 2)}
	states := &recordingObserver{}
	session, err := NewSession(Config{Token: token, TLSConfig: clientTLS, IOTimeout: time.Second}, renderer, states)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.ServeConn(ctx, clientConn) }()

	first := <-renderer.presented
	if first.Width != 2 || first.Generation != 1 {
		t.Fatalf("snapshot %#v", first)
	}
	session.Input().SetFocused(true)
	if err := session.Input().Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if got := (<-inputResult).(protocol.Key); got.Generation != 1 || got.Usage != 4 {
		t.Fatalf("input %#v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
	if !states.contains(StateConnected) {
		t.Fatalf("client never reached Connected: %v", states.states)
	}
}

// TestClientDowngradesToV1HostWithoutRetry drives the real client against a
// strictly v1.4.1-style host: a default v1-only host decoder. The client
// advertises {1,2}, the host answers v1, and the client's subsequent input
// must ride a v1 envelope (the host would reject a v2 envelope). No retry or
// reconnect is involved: one handshake, one downgrade.
func TestClientDowngradesToV1HostWithoutRetry(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	serverConn := tls.Server(serverRaw, serverTLS)
	clientConn := tls.Client(clientRaw, clientTLS)
	var token [32]byte
	token[0] = 12
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hostResult := make(chan error, 1)
	inputResult := make(chan protocol.Message, 1)
	go func() {
		if err := transport.Handshake(ctx, serverConn, time.Second); err != nil {
			hostResult <- err
			return
		}
		// A v1.4.1 host: default (v1-only) encoder and decoder, no pinning.
		peer := transport.NewPeer(serverConn, protocol.RoleHost, protocol.DefaultLimits(), time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			hostResult <- err
			return
		}
		message, err := peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		hello, ok := message.(protocol.ClientHello)
		if !ok || hello.MaxVersion != protocol.Version2 {
			hostResult <- errUnexpected("CLIENT_HELLO advertising v2", message)
			return
		}
		if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version1}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []protocol.Rectangle{{Width: 1, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(1, 1)}}}); err != nil {
			hostResult <- err
			return
		}
		message, err = peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		inputResult <- message
		hostResult <- peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &recordingRenderer{presented: make(chan Snapshot, 1)}
	states := &recordingObserver{}
	session, err := NewSession(Config{Token: token, TLSConfig: clientTLS, IOTimeout: time.Second}, renderer, states)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.ServeConn(ctx, clientConn) }()

	first := <-renderer.presented
	if first.Generation != 1 {
		t.Fatalf("snapshot %#v", first)
	}
	session.Input().SetFocused(true)
	if err := session.Input().Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if got := (<-inputResult).(protocol.Key); got.Generation != 1 || got.Usage != 4 {
		t.Fatalf("input %#v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
	if !states.contains(StateConnected) || !states.contains(StateClosed) {
		t.Fatalf("states %v", states.states)
	}
}

func errUnexpected(want string, got protocol.Message) error {
	return &unexpectedMessageError{want: want, got: got}
}

type unexpectedMessageError struct {
	want string
	got  protocol.Message
}

func (e *unexpectedMessageError) Error() string {
	return "expected " + e.want + ", got " + messageTypeName(e.got)
}

func messageTypeName(m protocol.Message) string {
	if m == nil {
		return "<nil>"
	}
	return m.Type().String()
}
