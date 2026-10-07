package integration

// R3 AC-8 (finding F9): busy rejection verified against the SHIPPED host
// acceptance path. F9's lesson was a test that "passes" against a harness
// re-implementation of the host while the real host diverged; after the AC-9
// extraction there is only one establishment implementation, so this test
// drives internal/session.Accept with a shared Admissions exactly the way
// cmd/vdhost's handleConnection does. It asserts the second client receives
// ERROR{protocol.ErrorBusy} and is closed TWICE — once while the first
// session is idle and once while it is actively streaming frames — and that
// the first session keeps receiving real frames across both rejections.
//
// Coverage addition, no red anchor: busy rejection already behaved correctly
// at 3109652 (the N5 precedent from R2 — this documents the pre-existing
// behavior against the shared code path rather than claiming red).

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/session"
	"virtualdesktop/internal/transport"
)

// connectExpectingBusy authenticates against the shared-acceptance host and
// reads the early ERROR_BUSY response. Admission is checked immediately after
// AUTH, before CLIENT_HELLO, so sending a hello here races the host's close.
func connectExpectingBusy(ctx context.Context, clientTLS *tls.Config, token [32]byte, addr string) (protocol.ErrorMessage, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return protocol.ErrorMessage{}, err
	}
	tlsConn := tls.Client(conn, clientTLS.Clone())
	if err := transport.Handshake(ctx, tlsConn, 5*time.Second); err != nil {
		conn.Close()
		return protocol.ErrorMessage{}, err
	}
	peer := transport.NewPeerConn(tlsConn, conn, protocol.RoleClient, protocol.DefaultLimits(), 5*time.Second)
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		conn.Close()
		return protocol.ErrorMessage{}, err
	}
	msg, err := peer.Receive(ctx)
	conn.Close()
	if err != nil {
		return protocol.ErrorMessage{}, err
	}
	e, ok := msg.(protocol.ErrorMessage)
	if !ok {
		return protocol.ErrorMessage{}, errors.New("expected ERROR, got " + msg.Type().String())
	}
	return e, nil
}

func TestBusySecondClientRejectedFirstKeepsStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	admissions := &session.Admissions{}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cap1 := &fakeCapture{}
	cap1.setImage(8, 8, 0x10)
	in1 := &recordingInput{}
	log1 := &messageLog{}
	acceptErrs := make(chan error, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				established, err := session.Accept(ctx, c, session.Config{
					Token:      token,
					TLSConfig:  serverTLS,
					Limits:     protocol.DefaultLimits(),
					IOTimeout:  5 * time.Second,
					Admissions: admissions,
				})
				if err != nil {
					acceptErrs <- err
					return
				}
				defer established.Release()
				defer func() { _ = established.Peer.Close() }()
				svc := host.NewService(host.Config{
					CaptureInterval:         time.Millisecond,
					KeyframeInterval:        time.Hour,
					MaxInputEventsPerSecond: 1000,
					EnableInput:             true,
				}, cap1, in1)
				peer := &recordingPeer{Peer: established.Peer, log: log1}
				_ = svc.Run(ctx, peer)
			}(conn)
		}
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 10*time.Second, "first session keyframe", func() bool { return renderer.count() >= 1 })
	if !observer.contains(client.StateConnected) {
		t.Fatalf("first client never connected; states=%v", observer.states)
	}

	// Rejection #1 while the first session is idle.
	e1, err := connectExpectingBusy(ctx, clientTLS, token, ln.Addr().String())
	if err != nil {
		t.Fatalf("busy rejection #1 wire: %v", err)
	}
	if e1.Code != protocol.ErrorBusy {
		t.Fatalf("busy rejection #1 code = %d, want protocol.ErrorBusy (%d)", e1.Code, protocol.ErrorBusy)
	}

	// The first session must keep STREAMING across the rejection: a screen
	// change has to reach the renderer after the busy peer was turned away.
	framesBefore := renderer.count()
	cap1.setImage(8, 8, 0x20)
	waitFor(t, 10*time.Second, "first session frames after busy rejection", func() bool {
		return renderer.count() > framesBefore
	})
	if state, ok := stateAfterConnected(observer); ok {
		t.Fatalf("first session regressed to %v while holding the slot", state)
	}

	// Rejection #2 while the first session is actively streaming.
	e2, err := connectExpectingBusy(ctx, clientTLS, token, ln.Addr().String())
	if err != nil {
		t.Fatalf("busy rejection #2 wire: %v", err)
	}
	if e2.Code != protocol.ErrorBusy {
		t.Fatalf("busy rejection #2 code = %d, want protocol.ErrorBusy (%d)", e2.Code, protocol.ErrorBusy)
	}

	// Server-side, the shared acceptance observed both rejections as ErrBusy.
	deadline := time.After(5 * time.Second)
	busySeen := 0
	for busySeen < 2 {
		select {
		case aerr := <-acceptErrs:
			if !errors.Is(aerr, session.ErrBusy) {
				t.Fatalf("accept error = %v, want session.ErrBusy", aerr)
			}
			busySeen++
		case <-deadline:
			t.Fatalf("server-side acceptance observed only %d busy rejections, want 2", busySeen)
		}
	}

	// And streaming continued after rejection #2 as well.
	framesBefore = renderer.count()
	cap1.setImage(8, 8, 0x30)
	waitFor(t, 10*time.Second, "first session still streaming after both rejections", func() bool {
		return renderer.count() > framesBefore
	})

	cancel()
	<-done
}
