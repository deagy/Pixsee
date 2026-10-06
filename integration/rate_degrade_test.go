package integration

// R3 red-baseline test for the input rate-overrun degrade (finding F7,
// spec AC-10, owner decision Q5).
//
// At 3109652 a rate overrun terminates the WHOLE session: handleInput
// returns ErrInputRate, the input loop reports it, and Service.Run returns
// (internal/host/service.go, the `s.rateCount >= MaxInputEventsPerSecond`
// branch). A fast polling mouse legitimately exceeds the default 500/s, so
// one twitchy user kills the session. Q5 locks the fix shape: drop the
// event, count drops, log one line per burst — terminate ONLY on protocol
// violations. This test drives a limit+50% burst and asserts the session
// survives and keeps streaming: red at baseline (the service dies mid-burst).
// The drop counter assertion is added in the fix commit alongside the
// counter API itself (N5 precedent from R2).

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/host"
)

// rateHostService runs a full host service with an explicit input rate
// limit, mirroring the production wiring. Establishment is delegated to the
// shared, shipped path via hostServiceWithConfig (AC-9).
func rateHostService(ctx context.Context, conn net.Conn, serverTLS *tls.Config, token [32]byte, cap *fakeCapture, input *recordingInput, log *messageLog, maxInputEventsPerSecond int) (*host.Service, *recordingPeer, <-chan error) {
	return hostServiceWithConfig(ctx, conn, serverTLS, token, cap, input, log, host.Config{
		CaptureInterval:         time.Millisecond,
		KeyframeInterval:        time.Hour,
		MaxInputEventsPerSecond: maxInputEventsPerSecond,
		EnableInput:             true,
		HeartbeatInterval:       time.Hour, // no probes during this test window
		HeartbeatTimeout:        time.Hour,
	})
}

func TestInputRateOverrunDropsEventsButKeepsSession(t *testing.T) {
	const (
		rateLimit = 10
		burst     = 15 // limit + 50% (AC-10)
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cap1 := &fakeCapture{}
	cap1.setImage(8, 8, 0x10)
	in1 := &recordingInput{}
	log1 := &messageLog{}
	svcs := make(chan *host.Service, 1)
	svcExited := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			svcExited <- err
			return
		}
		svc, _, errc := rateHostService(ctx, c, serverTLS, token, cap1, in1, log1, rateLimit)
		svcs <- svc
		svcExited <- <-errc
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 10*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if !observer.contains(client.StateConnected) {
		t.Fatalf("client never reached Connected; states=%v", observer.states)
	}

	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatal(err)
	}

	// Capture the burst log signal: Q5 requires exactly ONE log line per
	// burst, not per dropped event.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	// The burst: limit + 50% pointer moves delivered well inside one rate
	// window — exactly the traffic shape of a fast polling mouse (F7).
	for i := 0; i < burst; i++ {
		if err := sess.Input().Pointer(float64(i%8)+0.5, float64((i/8)%8)+0.5, 8, 8); err != nil {
			t.Fatalf("pointer move %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The baseline termination surfaces within tens of milliseconds of the
	// 11th event; give it time, then require the session to still be up.
	select {
	case err := <-svcExited:
		t.Fatalf("rate overrun terminated the session (%v); AC-10/Q5 requires dropping events and keeping the session Active", err)
	case <-time.After(500 * time.Millisecond):
	}
	if state, ok := stateAfterConnected(observer); ok {
		t.Fatalf("session regressed to state %v after a rate overrun; AC-10 requires staying Connected", state)
	}

	// Q5 telemetry: the drops are counted, and the burst was announced with
	// exactly one log line (not per-event spam).
	if dropped := (<-svcs).InputDropped(); dropped == 0 {
		t.Fatal("AC-10: rate overrun must be counted via Service.InputDropped (Q5 counter)")
	}
	if n := strings.Count(logBuf.String(), "input rate limit exceeded"); n != 1 {
		t.Fatalf("burst log lines = %d, want exactly 1 per burst (Q5); log=%q", n, logBuf.String())
	}

	// Liveness in both directions: the host must still stream a new frame
	// after the burst (the first 10 events were genuinely injected too).
	frames := renderer.count()
	cap1.setImage(8, 8, 0x20)
	waitFor(t, 10*time.Second, "post-burst frame", func() bool { return renderer.count() > frames })
	if n, _ := in1.snapshot(); n < rateLimit {
		t.Fatalf("host injected %d events, want at least the %d-event allowance before drops begin", n, rateLimit)
	}
	select {
	case err := <-svcExited:
		t.Fatalf("session died at the end of the rate-overrun test: %v", err)
	default:
	}
	cancel()
	<-done
}
