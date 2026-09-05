package integration

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

// startHostWithLimit starts a real host service with a specific
// MaxInputEventsPerSecond rate limit.
func startHostWithLimit(t *testing.T, ctx context.Context, serverTLS *tls.Config, token [32]byte, cap *fakeCapture, input *recordingInput, maxInput int) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		svc := host.NewService(host.Config{
			CaptureInterval:         time.Millisecond,
			KeyframeInterval:        time.Hour,
			MaxInputEventsPerSecond: maxInput,
			EnableInput:             true,
		}, cap, input)
		serverConn := tls.Server(conn, serverTLS.Clone())
		if err := transport.Handshake(ctx, serverConn, 5*time.Second); err != nil {
			return
		}
		peer := transport.NewPeerConn(serverConn, conn, protocol.RoleHost, protocol.DefaultLimits(), 5*time.Second)
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
			_ = svc.Run(runCtx, peer)
		}()
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestThrottlingUnderSlowClient verifies that the host rate-limits input and
// stops forwarding events once MaxInputEventsPerSecond is exceeded. A real
// client floods the host with key events; the host input adapter must receive
// exactly MaxInputEventsPerSecond events and then stop, confirming the rate
// limiter rejects the excess.
func TestThrottlingUnderSlowClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	// Very low limit so a burst of input trips the rate limiter.
	const maxInput = 10
	addr, stop := startHostWithLimit(t, ctx, serverTLS, token, cap, input, maxInput)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })

	// Focus the input state, then flood it with key events. The client's
	// InputState dedups identical repeated key transitions (pressing a held key
	// again is a no-op), so flood with alternating down/up on the same usage so
	// each event is a distinct state change that actually reaches the host.
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		action := protocol.ActionDown
		if i%2 == 1 {
			action = protocol.ActionUp
		}
		_ = sess.Input().Key(0x04, action, 0)
	}

	// The host must forward exactly maxInput key events to the adapter, then
	// reject the rest. Give the input loop a moment to process the burst.
	waitFor(t, 5*time.Second, "rate limiter engaged", func() bool {
		n, _ := input.snapshot()
		return n >= maxInput
	})
	// Drain any in-flight events, then confirm the count is capped at maxInput.
	waitFor(t, 2*time.Second, "adapter idle", func() bool {
		n, _ := input.snapshot()
		return n == maxInput
	})
	if n, _ := input.snapshot(); n != maxInput {
		t.Fatalf("host adapter received %d key events; want exactly %d (rate limited)", n, maxInput)
	}

	cancel()
	<-done
}
