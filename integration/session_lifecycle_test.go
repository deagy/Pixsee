package integration

// R0 red-baseline tests for the remediation spec (2026-10-01).
//
//	TestOracleFullSessionReconnect         — AC-1/AC-2 (F1 reconnect, F3 input dead)
//	TestIdleSoakSurvivesQuietSession       — AC-3     (F2 keepalive dead code)
//	TestHeartbeatWatchdogProbesSilentPeer  — F2/AC-3  (watchdog owns dead peers)
//	TestClientKeepaliveProbesIdleHost      — D7 client PING (authored with the
//	                                        R1 fix; needs the client.Config
//	                                        heartbeat surface that R1 adds)
//
// The first three drive only the config surface that exists at the baseline
// commit f7bb427 (host.Config heartbeat fields, transport peer timeouts,
// client.Config IOTimeout) so they compile and fail at runtime against the
// baseline, not at compile time. Time values are short per spec D3 so the
// suite stays under the CI budget.

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
	"virtualdesktop/internal/transport"
)

// TestOracleFullSessionReconnect drives a real reconnect: connection 1 runs a
// full host service (DisplayConfig + keyframe delivered), then is killed. The
// client Session must reconnect to connection 2 (fresh host service), receive a
// new DisplayConfig/keyframe, reach StateConnected again, and still be able to
// send input.
//
// Lifted verbatim from the oracle reproduction harness
// (/home/deagy/.cache/scratch/opencode/oracle_review/oracle_reconnect_test.go)
// per spec D2: it fails at f7bb427 because the client Framebuffer keeps the
// previous session's generation while each host Service restarts at generation
// 1, so the second DISPLAY_CONFIG is rejected as a stale display generation
// (F1), and input would additionally be dead because ServeConn never calls
// input.Connect() (F3).
func TestOracleFullSessionReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()

	cap1 := &fakeCapture{}
	cap1.setImage(8, 8, 0x10)
	in1 := &recordingInput{}
	log1 := &messageLog{}

	// Connection 1: full host service. hostService blocks on the TLS handshake
	// until the client dials, so run it in a goroutine.
	c1c := make(chan net.Conn, 1)
	svc1Exited := make(chan error, 1)
	go func() {
		c := <-conns
		c1c <- c
		_, _, errc := hostService(ctx, c, serverTLS, token, cap1, in1, log1)
		svc1Exited <- <-errc
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 15*time.Second, "first keyframe", func() bool { return renderer.count() >= 1 })
	if !observer.contains(client.StateConnected) {
		t.Fatalf("never connected on first session; states=%v", observer.states)
	}

	// Kill connection 1 (simulate network drop).
	c1 := <-c1c
	c1.Close()
	select {
	case <-svc1Exited:
	case <-time.After(15 * time.Second):
		t.Fatal("host service 1 did not exit")
	}

	waitFor(t, 15*time.Second, "reconnecting", func() bool {
		return observer.contains(client.StateReconnecting)
	})

	// Connection 2: fresh host service on the reconnect.
	cap2 := &fakeCapture{}
	cap2.setImage(8, 8, 0x40)
	in2 := &recordingInput{}
	log2 := &messageLog{}
	go func() {
		c := <-conns
		hostService(ctx, c, serverTLS, token, cap2, in2, log2)
	}()

	// The client should re-reach Connected and present the new screen.
	reconnected := false
	deadline := time.After(20 * time.Second)
	for !reconnected {
		select {
		case <-deadline:
			observer.mu.Lock()
			states := append([]client.ConnectionState(nil), observer.states...)
			observer.mu.Unlock()
			t.Fatalf("client never re-established session after reconnect; states=%v rendererFrames=%d", states, renderer.count())
		case <-time.After(10 * time.Millisecond):
			observer.mu.Lock()
			connectedCount := 0
			for _, st := range observer.states {
				if st == client.StateConnected {
					connectedCount++
				}
			}
			observer.mu.Unlock()
			reconnected = connectedCount >= 2 && renderer.count() >= 2
		}
	}

	// Input must still work after the reconnect.
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatalf("SetFocused after reconnect: %v", err)
	}
	if err := sess.Input().Key(0x04, protocol.ActionDown, 0); err != nil {
		t.Fatalf("Key after reconnect: %v", err)
	}
	keysArrived := false
	dl := time.After(10 * time.Second)
	for !keysArrived {
		select {
		case <-dl:
			n, _ := in2.snapshot()
			t.Fatalf("input dead after reconnect: host2 received %d key events", n)
		case <-time.After(10 * time.Millisecond):
			n, _ := in2.snapshot()
			keysArrived = n >= 1
		}
	}

	// N2 (R3): the full DOWN/UP pair must survive the reconnect, not just the
	// first event — a key release has to reach the host too.
	if err := sess.Input().Key(0x04, protocol.ActionUp, 0); err != nil {
		t.Fatalf("Key UP after reconnect: %v", err)
	}
	upArrived := false
	uld := time.After(10 * time.Second)
	for !upArrived {
		select {
		case <-uld:
			n, _ := in2.snapshot()
			t.Fatalf("key UP did not reach the host after reconnect: host2 received %d key events (DOWN/UP pair incomplete)", n)
		case <-time.After(10 * time.Millisecond):
			n, _ := in2.snapshot()
			upArrived = n >= 2
		}
	}
	cancel()
	<-done
}

// quietHostService runs a real host.Service like hostService, but with the
// given heartbeat configuration (spec D3: behavior tests drive the real
// config surface, here host.Config's heartbeat fields, which cmd/vdhost wires
// from -heartbeat-interval/-heartbeat-timeout). Establishment is delegated to
// the shared, shipped path via hostServiceWithConfig (AC-9).
func quietHostService(ctx context.Context, conn net.Conn, serverTLS *tls.Config, token [32]byte, cap *fakeCapture, input *recordingInput, log *messageLog, heartbeatInterval, heartbeatTimeout time.Duration) (*host.Service, *recordingPeer, <-chan error) {
	return hostServiceWithConfig(ctx, conn, serverTLS, token, cap, input, log, host.Config{
		CaptureInterval:         time.Millisecond,
		KeyframeInterval:        time.Hour,
		MaxInputEventsPerSecond: 1000,
		EnableInput:             true,
		HeartbeatInterval:       heartbeatInterval,
		HeartbeatTimeout:        heartbeatTimeout,
	})
}

// establishSilentClient completes the full client-side establishment against
// a host service and returns the raw connection plus its peer. The caller
// then does exactly nothing with it: the peer never reads another message and
// never replies to control traffic, reproducing a hard-silent client.
func establishSilentClient(ctx context.Context, clientTLS *tls.Config, token [32]byte, addr string, ioTimeout time.Duration) (net.Conn, *transport.Peer, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	tlsConn := tls.Client(conn, clientTLS.Clone())
	if err := transport.Handshake(ctx, tlsConn, ioTimeout); err != nil {
		conn.Close()
		return nil, nil, err
	}
	peer := transport.NewPeerConn(tlsConn, conn, protocol.RoleClient, protocol.DefaultLimits(), ioTimeout)
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if _, err := peer.Receive(ctx); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if _, err := peer.Receive(ctx); err != nil { // DISPLAY_CONFIG
		conn.Close()
		return nil, nil, err
	}
	return conn, peer, nil
}

// TestIdleSoakSurvivesQuietSession asserts AC-3: with the test-parameterized
// short heartbeat flags (interval 4s, timeout 6s, per spec D3) a session must
// survive at least 2x(HeartbeatInterval+HeartbeatTimeout) while BOTH sides are
// quiet — the client sends zero input events and the host streams a static
// screen (no frames after the initial keyframe). Control-plane keepalive
// (PING/PONG) is the only traffic allowed to cross.
//
// This is the acceptance trap for F2: at f7bb427 the keepalive cannot save a
// quiet session — the baseline host probes only on its full-interval tick and
// the client never probes at all, so the gaps between host PINGs exceed the
// client's per-Receive IOTimeout and the read deadline tears the session down
// (and the baseline watchdog reaps it soon after). The test must NOT keep the
// session alive by typing or by changing the captured image.
func TestIdleSoakSurvivesQuietSession(t *testing.T) {
	// D3 short flags. interval < timeout so the watchdog's probe-and-give-up
	// windows (and the client's PONG tolerance) sit strictly inside the read
	// deadlines: a healthy round-trip can never be starved into a false reap
	// by -race scheduling jitter, while at the baseline commit the same flags
	// still kill the quiet session (re-verified red at the R0 tree with these
	// exact values: the client's 5s per-Receive deadline expires in the
	// baseline's ~8s probe gaps). The 2x invariant soak (20s) stays under
	// D3's 30s budget.
	const (
		heartbeatInterval = 4 * time.Second
		heartbeatTimeout  = 6 * time.Second
	)
	soak := 2 * (heartbeatInterval + heartbeatTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), soak+30*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()

	// Static screen: the image is set once and never changed again, so the
	// host streams exactly one keyframe and nothing else.
	cap1 := &fakeCapture{}
	cap1.setImage(8, 8, 0x10)
	in1 := &recordingInput{}
	log1 := &messageLog{}
	svcExited := make(chan error, 1)
	go func() {
		c := <-conns
		_, _, errc := quietHostService(ctx, c, serverTLS, token, cap1, in1, log1, heartbeatInterval, heartbeatTimeout)
		svcExited <- <-errc
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 15*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if !observer.contains(client.StateConnected) {
		t.Fatalf("client never reached Connected; states=%v", observer.states)
	}

	// The soak: neither side does any work. Any host-service exit, any client
	// state regression past Connected, is a failure of AC-3.
	start := time.Now()
	for time.Since(start) < soak {
		select {
		case err := <-svcExited:
			t.Fatalf("idle session died after %v: host service exited with %v; AC-3 requires survival for >= %v of quiet", time.Since(start), err, soak)
		default:
		}
		if state, ok := stateAfterConnected(observer); ok {
			t.Fatalf("idle session regressed to state %v after %v; AC-3 requires the session to stay Connected", state, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}
	survived := time.Since(start)

	// Both directions were quiet for the whole soak. The client sent no input
	// or request traffic (keepalive replies are control plane, not input),
	// and the host streamed no frames beyond the initial keyframe.
	for _, forbidden := range []string{"KEY", "POINTER_MOVE", "POINTER_BUTTON", "POINTER_WHEEL", "FOCUS_LOST", "KEYFRAME_REQUEST"} {
		if n := log1.clientKinds()[forbidden]; n != 0 {
			t.Fatalf("client sent %d %v messages during a zero-input soak", n, forbidden)
		}
	}
	if n := log1.hostKinds()["FRAME"]; n != 1 {
		t.Fatalf("static screen must produce exactly 1 FRAME (the keyframe), got %d", n)
	}
	if n := log1.hostKinds()["DISPLAY_CONFIG"]; n != 1 {
		t.Fatalf("static screen must produce exactly 1 DISPLAY_CONFIG, got %d", n)
	}
	if renderer.count() != 1 {
		t.Fatalf("client presented %d snapshots for a static screen, want 1", renderer.count())
	}
	// What DID cross the wire is the documented keepalive: the host probed
	// the silence with PING and the client answered with PONG. Survival with
	// zero control traffic would mean the read deadline is doing the work by
	// accident, not the heartbeat (F2's dead-code probe must be alive).
	if n := log1.hostKinds()["PING"]; n < 1 {
		t.Fatal("host sent no keepalive PING during the idle soak (F2: dead-code keepalive)")
	}
	if n := log1.clientKinds()["PONG"]; n < 1 {
		t.Fatal("client never answered a keepalive PING with a PONG")
	}

	select {
	case err := <-svcExited:
		t.Fatalf("host service exited at the end of the soak: %v", err)
	default:
	}
	t.Logf("idle session survived %v of quiet soak at heartbeat %v/%v", survived, heartbeatInterval, heartbeatTimeout)
	cancel()
	<-done
}

// stateAfterConnected reports the first observed state after the first
// Connected, if any. Establishing the session legitimately passes through
// Connecting/Authenticating/Negotiating; anything recorded after Connected
// means the established session moved.
func stateAfterConnected(observer *stateRecorder) (client.ConnectionState, bool) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for i, st := range observer.states {
		if st == client.StateConnected && i+1 < len(observer.states) {
			return observer.states[i+1], true
		}
	}
	return 0, false
}

// TestHeartbeatWatchdogProbesSilentPeer asserts that the heartbeat watchdog —
// not the per-operation I/O deadline — owns dead-peer detection (F2/D7):
// against a peer that establishes a session and then goes hard silent, the
// host must (a) send the documented unsolicited PING probe BEFORE closing,
// (b) close the session with ErrHeartbeatTimeout, and (c) run ReleaseAll
// against the input adapter.
//
// At f7bb427 the probe never goes out: with interval == timeout the heartbeat
// loop hits its give-up branch on the first tick, and at production defaults
// the read deadline would kill the session before the 30s PING could fire at
// all. Failing "closed a silent peer without ever probing it" is exactly the
// dead-code keepalive the finding describes.
func TestHeartbeatWatchdogProbesSilentPeer(t *testing.T) {
	const (
		heartbeatInterval = 4 * time.Second
		heartbeatTimeout  = 4 * time.Second
		clientIOTimeout   = 10 * time.Second
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
	svcExited := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			svcExited <- err
			return
		}
		_, _, errc := quietHostService(ctx, c, serverTLS, token, cap1, in1, log1, heartbeatInterval, heartbeatTimeout)
		svcExited <- <-errc
	}()

	// A real client peer establishes the full session, then goes silent: it
	// never reads again and never answers any probe.
	conn, _, err := establishSilentClient(ctx, clientTLS, token, ln.Addr().String(), clientIOTimeout)
	if err != nil {
		t.Fatalf("establish silent client: %v", err)
	}
	defer conn.Close()

	var svcErr error
	select {
	case svcErr = <-svcExited:
	case <-time.After(20 * time.Second):
		t.Fatal("host never closed the hard-silent peer")
	}
	if !errors.Is(svcErr, host.ErrHeartbeatTimeout) {
		t.Fatalf("silent peer was reaped by %v, want host.ErrHeartbeatTimeout: the heartbeat watchdog must own dead-peer detection (D7)", svcErr)
	}

	if n := log1.hostKinds()["PING"]; n < 1 {
		t.Fatalf("watchdog closed the silent peer after %v with ZERO PING probes; the documented keepalive probe is dead code (F2)", heartbeatTimeout)
	}
	if _, releases := in1.snapshot(); releases < 1 {
		t.Fatal("host must ReleaseAll when the heartbeat watchdog closes the session")
	}
	cancel()
}

// TestClientKeepaliveProbesIdleHost covers the new client half of D7: both
// sides send unsolicited PING after HeartbeatInterval/2 of idle. The host is
// left at production-default heartbeats (30/30), so for the whole test window
// only the CLIENT probes — at symmetric short client flags (4s/4s, the D3
// surface). The host answers PONG, the client's steady-state read deadline
// (interval + timeout + slack) never expires, and the session stays Connected
// with zero input events and a static screen.
//
// Authored in R1 alongside the client.Config heartbeat surface it drives: no
// pre-fix configuration existed to configure this direction, so the red anchor
// for F2 remains the soak (b) and the watchdog probe test (c) above.
func TestClientKeepaliveProbesIdleHost(t *testing.T) {
	const (
		clientHeartbeatInterval = 4 * time.Second
		clientHeartbeatTimeout  = 6 * time.Second
		hostHeartbeatInterval   = 30 * time.Second
		hostHeartbeatTimeout    = 30 * time.Second
	)
	// Watch 8s: long enough for the first client PING tick (interval/2 = 2s)
	// with a generous watchdog margin (give-up requires a 6s-unanswered
	// probe), comfortably inside the steady-state read deadline (4+6+5 =
	// 15s), so the only way this passes is the client-initiated probe keeping
	// the host responsive. (The 2x quiet-window invariant itself is asserted
	// by TestIdleSoakSurvivesQuietSession.)
	soak := 8 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), soak+30*time.Second)
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
	svcExited := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			svcExited <- err
			return
		}
		_, _, errc := quietHostService(ctx, c, serverTLS, token, cap1, in1, log1, hostHeartbeatInterval, hostHeartbeatTimeout)
		svcExited <- <-errc
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, err := client.NewSession(client.Config{
		Token:             token,
		TLSConfig:         clientTLS,
		IOTimeout:         5 * time.Second,
		ReconnectDelay:    50 * time.Millisecond,
		HeartbeatInterval: clientHeartbeatInterval,
		HeartbeatTimeout:  clientHeartbeatTimeout,
		Dial:              func(context.Context) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) },
		Input:             client.NewInputState(nil),
	}, renderer, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()
	defer sess.Input().Disconnect()

	waitFor(t, 15*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if !observer.contains(client.StateConnected) {
		t.Fatalf("client never reached Connected; states=%v", observer.states)
	}

	start := time.Now()
	for time.Since(start) < soak {
		select {
		case err := <-svcExited:
			t.Fatalf("idle session died after %v: host service exited with %v", time.Since(start), err)
		default:
		}
		if state, ok := stateAfterConnected(observer); ok {
			t.Fatalf("idle session regressed to state %v after %v", state, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The client's first probe fires around interval/2 = 4s; poll for it
	// (plus a tick of grace) rather than sampling exactly at the window edge.
	waitFor(t, 10*time.Second, "client-initiated PING", func() bool {
		return log1.clientKinds()["PING"] >= 1
	})
	if n := log1.hostKinds()["PONG"]; n < 1 {
		t.Fatal("host never answered the client's PING with a PONG")
	}
	for _, forbidden := range []string{"KEY", "POINTER_MOVE", "POINTER_BUTTON", "POINTER_WHEEL", "FOCUS_LOST"} {
		if n := log1.clientKinds()[forbidden]; n != 0 {
			t.Fatalf("client sent %d %v messages during a zero-input soak", n, forbidden)
		}
	}
	if n := log1.hostKinds()["FRAME"]; n != 1 {
		t.Fatalf("static screen must produce exactly 1 FRAME, got %d", n)
	}
	select {
	case err := <-svcExited:
		t.Fatalf("host service exited at the end of the soak: %v", err)
	default:
	}
	cancel()
	<-done
}
