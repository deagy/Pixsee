package integration

// Phase 4 protocol-v2 integration evidence. These tests drive the REAL
// client.Session/renderer against the shared establishment path
// (internal/session.Accept) and the REAL host.Service, over a real TLS 1.3
// connection, rather than a scripted reimplementation of either half.
//
// Tests 1-3 exercise the complete shipped path: session.Accept negotiates the
// version and host.Service packs the frame, with the negotiated version
// threaded into host.Config exactly as cmd/vdhost's handleConnection does.
//
// Tests 4-5 script the visual stream directly on a session.Accept peer. They
// still use the shared real establishment (TLS/AUTH/busy/HELLO) and the real
// client.Session; only host.Service is bypassed, because it cannot be asked to
// deterministically disconnect in the middle of a FRAME_PART sequence (test 4)
// or interleave a control PING between two parts (test 5). That is a limit of
// the end-to-end evidence and is called out here rather than hidden.

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/session"
	"virtualdesktop/internal/transport"
)

// v2PartLimits forces one rectangle per FRAME_PART for the tiny 2x1 test
// rectangles (each part payload is 31 + 22 + 8 = 61 bytes).
func v2PartLimits() protocol.Limits { return protocol.Limits{MaxPixelPayload: 61} }

// v2NoisePixels returns a deterministic, high-entropy BGRA buffer that zlib
// cannot shrink, so an oversized v2 frame stays raw and must be split.
func v2NoisePixels(w, h uint32, seed uint64) []byte {
	p := make([]byte, int(w)*int(h)*4)
	s := seed | 1
	for i := range p {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		p[i] = byte(s)
	}
	return p
}

// setCaptureNoise replaces the fake capture's image with incompressible noise.
func setCaptureNoise(c *fakeCapture, w, h uint32, seed uint64) {
	image := damage.Image{Width: w, Height: h, Pixels: v2NoisePixels(w, h, seed)}
	c.mu.Lock()
	c.image = image
	c.mu.Unlock()
}

// coordInput is a host.Input that records the coordinates the host injected, so
// a test can prove the v1 downscale pointer remap reached the platform adapter.
type coordInput struct {
	mu       sync.Mutex
	moves    [][2]uint32
	releases int
}

func (i *coordInput) Key(context.Context, uint16, protocol.Action, uint8) error { return nil }
func (i *coordInput) Move(_ context.Context, x, y uint32) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.moves = append(i.moves, [2]uint32{x, y})
	return nil
}
func (i *coordInput) Button(context.Context, protocol.Button, protocol.Action) error { return nil }
func (i *coordInput) Wheel(context.Context, int16, int16) error                      { return nil }
func (i *coordInput) ReleaseAll(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.releases++
	return nil
}
func (i *coordInput) lastMove() (uint32, uint32, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.moves) == 0 {
		return 0, 0, false
	}
	m := i.moves[len(i.moves)-1]
	return m[0], m[1], true
}

// startV2Host starts a real host listener that accepts through the shared
// internal/session.Accept and runs host.Service with the version actually
// negotiated for the connection (mirroring cmd/vdhost's handleConnection).
func startV2Host(t *testing.T, ctx context.Context, serverTLS *tls.Config, token [32]byte, capture host.Capture, input host.Input, log *messageLog, serverMax uint16, cfg host.Config) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				established, err := session.Accept(ctx, c, session.Config{
					Token:            token,
					TLSConfig:        serverTLS,
					Limits:           protocol.DefaultLimits(),
					IOTimeout:        5 * time.Second,
					ServerMaxVersion: serverMax,
				})
				if err != nil {
					return
				}
				defer established.Release()
				defer func() { _ = established.Peer.Close() }()
				// The negotiated version, not the server maximum, drives the
				// service: a v1 client gets downscaled FRAMEs, a v2 client
				// gets native-resolution FRAME_PARTs.
				cfg.ProtocolVersion = established.Version
				if cfg.CaptureInterval <= 0 {
					cfg.CaptureInterval = 20 * time.Millisecond
				}
				if cfg.KeyframeInterval <= 0 {
					cfg.KeyframeInterval = time.Hour
				}
				if cfg.MaxInputEventsPerSecond <= 0 {
					cfg.MaxInputEventsPerSecond = 1000
				}
				cfg.EnableInput = true
				svc := host.NewService(cfg, capture, input)
				peer := &recordingPeer{Peer: established.Peer, log: log}
				_ = svc.Run(ctx, peer)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestIntegrationV2FourKKeyframeAndDelta is the headline v2 end-to-end proof:
// a real client and a real v2 host move an incompressible 3840x2160 (31.6 MiB)
// initial keyframe — larger than the 16 MiB pixel-payload budget, so it MUST
// split — over real TLS. The client receives an ordered FRAME_PART sequence,
// commits exactly once, and presents a byte-exact framebuffer. A subsequent
// full-image change is a large (also multi-part) delta, again committed and
// presented exactly once, byte-exact.
func TestIntegrationV2FourKKeyframeAndDelta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 31

	const w, h = uint32(3840), uint32(2160)
	expectedA := v2NoisePixels(w, h, 0xABCDEF01)
	expectedB := v2NoisePixels(w, h, 0x10203040)

	cap := &fakeCapture{}
	setCaptureNoise(cap, w, h, 0xABCDEF01)
	input := &coordInput{}
	log := &messageLog{}
	// DirtyRatio 1.0 keeps a full-image change on the delta path instead of
	// promoting it to a keyframe, so the second frame is a genuine large delta.
	addr, stop := startV2Host(t, ctx, serverTLS, token, cap, input, log, protocol.Version2, host.Config{
		CaptureInterval: 250 * time.Millisecond,
		Damage:          damage.Config{DirtyRatio: 1.0},
	})
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	// A larger establishment IOTimeout than the other tests: the host encodes
	// the full 32 MiB keyframe (zlib attempt + banding) between SERVER_HELLO and
	// DISPLAY_CONFIG, and under -race instrumentation that can exceed the 5s
	// default establishment deadline. The steady-state (Active) deadline still
	// derives from the heartbeat config.
	sess, err := client.NewSession(client.Config{
		Token:          token,
		TLSConfig:      clientTLS,
		IOTimeout:      120 * time.Second,
		ReconnectDelay: 50 * time.Millisecond,
		Dial:           func(context.Context) (net.Conn, error) { return net.Dial("tcp", addr) },
		Input:          client.NewInputState(nil),
	}, renderer, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()
	defer sess.Input().Disconnect()

	waitFor(t, 180*time.Second, "4K keyframe presented", func() bool { return renderer.count() >= 1 })
	first := renderer.last()
	if first.Width != w || first.Height != h {
		t.Fatalf("initial snapshot size = %dx%d, want %dx%d", first.Width, first.Height, w, h)
	}
	if first.Generation != 1 || first.FrameSequence != 1 {
		t.Fatalf("initial generation/sequence = %d/%d, want 1/1", first.Generation, first.FrameSequence)
	}
	if !bytes.Equal(first.Pixels, expectedA) {
		t.Fatalf("initial framebuffer is not byte-exact (%d bytes, want %d)", len(first.Pixels), len(expectedA))
	}
	kinds := log.hostKinds()
	if kinds["DISPLAY_CONFIG"] != 1 {
		t.Fatalf("host DISPLAY_CONFIG count = %d, want 1", kinds["DISPLAY_CONFIG"])
	}
	if kinds["FRAME_PART"] < 2 {
		t.Fatalf("4K keyframe used %d FRAME_PARTs, want >= 2 for a >16MiB frame", kinds["FRAME_PART"])
	}
	if kinds["FRAME"] != 0 {
		t.Fatalf("oversized 4K keyframe was sent as %d plain FRAME(s)", kinds["FRAME"])
	}
	partsAtKeyframe := kinds["FRAME_PART"]

	// A large follow-up change: the whole image becomes different noise.
	setCaptureNoise(cap, w, h, 0x10203040)
	waitFor(t, 180*time.Second, "large delta presented", func() bool { return renderer.count() >= 2 })
	if got := renderer.count(); got != 2 {
		t.Fatalf("presented %d snapshots, want exactly 2 (no partial/duplicate present)", got)
	}
	second := renderer.last()
	if second.Generation != 1 || second.FrameSequence != 2 {
		t.Fatalf("delta generation/sequence = %d/%d, want 1/2", second.Generation, second.FrameSequence)
	}
	if !bytes.Equal(second.Pixels, expectedB) {
		t.Fatalf("delta framebuffer is not byte-exact (%d bytes, want %d)", len(second.Pixels), len(expectedB))
	}
	kinds = log.hostKinds()
	if kinds["FRAME"] != 0 {
		t.Fatalf("large delta was sent as %d plain FRAME(s), want a FRAME_PART sequence", kinds["FRAME"])
	}
	if kinds["FRAME_PART"] <= partsAtKeyframe {
		t.Fatalf("large delta added no FRAME_PARTs (before=%d after=%d)", partsAtKeyframe, kinds["FRAME_PART"])
	}
	if !observer.contains(client.StateConnected) {
		t.Fatalf("client never reached Connected; states=%v", observer.states)
	}

	cancel()
	<-done
}

// TestIntegrationV2ClientDowngradesToV1Host proves the v2 client ↔ v1 host
// downgrade on the first hello through real TLS and host.Service: the client
// advertises {1,2}, a v1-only host answers v1, the display is advertised
// downscaled, only ordinary FRAMEs go out, and an advertised edge pointer
// coordinate lands on the native edge coordinate.
func TestIntegrationV2ClientDowngradesToV1Host(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 32

	cap := &fakeCapture{}
	cap.setImage(3000, 1500, 0x20)
	input := &coordInput{}
	log := &messageLog{}
	// serverMax v1: a deployed v1.4.1-style host, no v2 capability offered.
	addr, stop := startV2Host(t, ctx, serverTLS, token, cap, input, log, protocol.Version1, host.Config{})
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 15*time.Second, "downscaled v1 keyframe", func() bool { return renderer.count() >= 1 })
	snap := renderer.last()
	if snap.Width != 1500 || snap.Height != 750 {
		t.Fatalf("v1 downgrade display = %dx%d, want downscaled 1500x750", snap.Width, snap.Height)
	}
	kinds := log.hostKinds()
	if kinds["DISPLAY_CONFIG"] != 1 {
		t.Fatalf("host DISPLAY_CONFIG count = %d, want 1", kinds["DISPLAY_CONFIG"])
	}
	if kinds["FRAME"] < 1 {
		t.Fatalf("v1 downgrade sent %d FRAMEs, want >= 1", kinds["FRAME"])
	}
	if kinds["FRAME_PART"] != 0 {
		t.Fatalf("v1 downgrade sent %d FRAME_PARTs; a v1 envelope cannot carry them", kinds["FRAME_PART"])
	}
	if !observer.contains(client.StateConnected) {
		t.Fatalf("client never reached Connected; states=%v", observer.states)
	}

	// Advertised-space edge (1499,749) must reach native (2999,1499).
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatalf("SetFocused: %v", err)
	}
	if err := sess.Input().Pointer(1499, 749, 1500, 750); err != nil {
		t.Fatalf("Pointer: %v", err)
	}
	waitFor(t, 10*time.Second, "remapped v1 pointer", func() bool {
		x, y, ok := input.lastMove()
		return ok && x == 2999 && y == 1499
	})

	cancel()
	<-done
}

// TestIntegrationV2HostServesV1ShapedClient is the mirror case: a v2-capable
// host (session.Accept ServerMaxVersion v2, host.Service v2) receives a
// v1-shaped CLIENT_HELLO {1,1}. It negotiates v1, auto-downscales the oversized
// capture, emits an ordinary FRAME, and remaps the v1 client's advertised edge
// pointer back to the native edge at the platform adapter.
func TestIntegrationV2HostServesV1ShapedClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 33

	cap := &fakeCapture{}
	cap.setImage(3000, 1500, 0x30)
	input := &coordInput{}
	log := &messageLog{}
	addr, stop := startV2Host(t, ctx, serverTLS, token, cap, input, log, protocol.Version2, host.Config{})
	defer stop()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tlsConn := tls.Client(raw, clientTLS.Clone())
	if err := transport.Handshake(ctx, tlsConn, 5*time.Second); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	peer := transport.NewPeerConn(tlsConn, raw, protocol.RoleClient, protocol.DefaultLimits(), 5*time.Second)
	if err := peer.AuthenticateClient(ctx, token); err != nil {
		t.Fatalf("AuthenticateClient: %v", err)
	}
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		t.Fatalf("send CLIENT_HELLO: %v", err)
	}
	message, err := peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive SERVER_HELLO: %v", err)
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok || hello.Version != protocol.Version1 {
		t.Fatalf("expected SERVER_HELLO v1, got %#v", message)
	}
	message, err = peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive DISPLAY_CONFIG: %v", err)
	}
	display, ok := message.(protocol.DisplayConfig)
	if !ok {
		t.Fatalf("expected DISPLAY_CONFIG, got %T", message)
	}
	if display.Width != 1500 || display.Height != 750 {
		t.Fatalf("v1-shaped client display = %dx%d, want downscaled 1500x750", display.Width, display.Height)
	}
	message, err = peer.Receive(ctx)
	if err != nil {
		t.Fatalf("receive first visual: %v", err)
	}
	if _, ok := message.(protocol.Frame); !ok {
		t.Fatalf("v1-shaped client first visual = %T, want an ordinary FRAME", message)
	}

	// Advertised-space edge (1499,749) must reach native (2999,1499).
	if err := peer.Send(ctx, protocol.PointerMove{Generation: 1, InputSequence: 1, X: 1499, Y: 749}); err != nil {
		t.Fatalf("send POINTER_MOVE: %v", err)
	}
	waitFor(t, 10*time.Second, "remapped v1 pointer", func() bool {
		x, y, ok := input.lastMove()
		return ok && x == 2999 && y == 1499
	})
}

// v2TestRect returns a raw BGRA rectangle filled with a single byte in every
// channel, so the expected reassembled framebuffer is easy to state exactly.
func v2TestRect(x, y, w, h uint32, fill byte) protocol.Rectangle {
	p := make([]byte, w*h*4)
	for i := range p {
		p[i] = fill
	}
	return protocol.Rectangle{X: x, Y: y, Width: w, Height: h, Encoding: protocol.EncodingRawBGRA, Pixels: p}
}

// v2TinyFrame tiles a 4x2 display with four non-overlapping 2x1 rectangles.
func v2TinyFrame(generation, sequence uint64) protocol.Frame {
	return protocol.Frame{Generation: generation, FrameSequence: sequence, Keyframe: true, Rectangles: []protocol.Rectangle{
		v2TestRect(0, 0, 2, 1, 0x11),
		v2TestRect(2, 0, 2, 1, 0x22),
		v2TestRect(0, 1, 2, 1, 0x33),
		v2TestRect(2, 1, 2, 1, 0x44),
	}}
}

// v2Expected4x2 is the byte-exact 4x2 BGRA framebuffer v2TinyFrame reassembles
// to after all four parts commit.
func v2Expected4x2() []byte {
	out := make([]byte, 4*2*4)
	fill := func(x, y int, v byte) {
		off := (y*4 + x) * 4
		for c := 0; c < 4; c++ {
			out[off+c] = v
		}
	}
	for x := 0; x < 2; x++ {
		fill(x, 0, 0x11)
		fill(x, 1, 0x33)
	}
	for x := 2; x < 4; x++ {
		fill(x, 0, 0x22)
		fill(x, 1, 0x44)
	}
	return out
}

// TestIntegrationV2ReconnectMidPartsDropsStaleStaging proves a disconnect while
// only part 0 of a logical frame has arrived never presents partial pixels, and
// that the reconnect starts with clean framebuffer state: without
// Framebuffer.Reset the second connection's fresh DISPLAY_CONFIG generation 1
// would be rejected as stale and no frame would ever present.
func TestIntegrationV2ReconnectMidPartsDropsStaleStaging(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 34

	limits := v2PartLimits()
	frame := v2TinyFrame(1, 1)
	parts, err := protocol.SplitFrame(frame, limits)
	if err != nil {
		t.Fatalf("SplitFrame: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("frame split into %d parts, want >= 2", len(parts))
	}
	expected := v2Expected4x2()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var connIndex int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			idx := atomic.AddInt32(&connIndex, 1) - 1
			go func(c net.Conn, idx int32) {
				established, err := session.Accept(ctx, c, session.Config{
					Token:            token,
					TLSConfig:        serverTLS,
					Limits:           limits,
					IOTimeout:        3 * time.Second,
					ServerMaxVersion: protocol.Version2,
				})
				if err != nil {
					return
				}
				defer established.Release()
				peer := established.Peer
				defer func() { _ = peer.Close() }()
				if idx == 0 {
					// Announce the display and send only part 0, then drop the
					// connection mid-sequence.
					_ = peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888})
					_ = peer.Send(ctx, parts[0])
					return
				}
				// Reconnect: a fresh service restarts at generation 1 and
				// delivers the complete keyframe.
				if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
					return
				}
				for _, part := range parts {
					if err := peer.Send(ctx, part); err != nil {
						return
					}
				}
				_ = peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
			}(conn, idx)
		}
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	dialCount := 0
	sess, err := client.NewSession(client.Config{
		Token:          token,
		TLSConfig:      clientTLS,
		Limits:         limits,
		IOTimeout:      3 * time.Second,
		ReconnectDelay: 20 * time.Millisecond,
		Dial: func(context.Context) (net.Conn, error) {
			dialCount++
			return net.Dial("tcp", ln.Addr().String())
		},
		Input: client.NewInputState(nil),
	}, renderer, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()
	defer sess.Input().Disconnect()

	waitFor(t, 20*time.Second, "full keyframe after mid-part reconnect", func() bool { return renderer.count() >= 1 })
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v; the session should end cleanly after the reconnect", err)
	}
	if renderer.count() != 1 {
		t.Fatalf("presented %d snapshots, want exactly 1 (the dropped connection must never present partial pixels)", renderer.count())
	}
	snap := renderer.last()
	if snap.Generation != 1 || snap.FrameSequence != 1 || snap.Width != 4 || snap.Height != 2 {
		t.Fatalf("reconnected snapshot header = %#v", snap)
	}
	if !bytes.Equal(snap.Pixels, expected) {
		t.Fatalf("reconnected framebuffer is not byte-exact")
	}
	if dialCount < 2 {
		t.Fatalf("client dialed %d times, want at least 2 (a reconnect)", dialCount)
	}
	if !observer.contains(client.StateReconnecting) || !observer.contains(client.StateConnected) || !observer.contains(client.StateClosed) {
		t.Fatalf("client states missing reconnect/connect/close: %v", observer.states)
	}
}

// TestIntegrationV2PingBetweenParts proves a control PING interleaved between
// FRAME_PARTs is answered with a PONG and does not disturb the in-progress
// reassembly: the client presents exactly once, byte-exact, only when the final
// part arrives.
func TestIntegrationV2PingBetweenParts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 35

	limits := v2PartLimits()
	frame := v2TinyFrame(1, 1)
	parts, err := protocol.SplitFrame(frame, limits)
	if err != nil {
		t.Fatalf("SplitFrame: %v", err)
	}
	if len(parts) != 4 {
		t.Fatalf("frame split into %d parts, want 4", len(parts))
	}
	expected := v2Expected4x2()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	pongSeen := make(chan uint64, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		established, err := session.Accept(ctx, conn, session.Config{
			Token:            token,
			TLSConfig:        serverTLS,
			Limits:           limits,
			IOTimeout:        3 * time.Second,
			ServerMaxVersion: protocol.Version2,
		})
		if err != nil {
			return
		}
		defer established.Release()
		peer := established.Peer
		defer func() { _ = peer.Close() }()
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			return
		}
		for _, part := range parts[:len(parts)-1] {
			if err := peer.Send(ctx, part); err != nil {
				return
			}
		}
		if err := peer.Send(ctx, protocol.Ping{Nonce: 9}); err != nil {
			return
		}
		message, err := peer.Receive(ctx)
		if err != nil {
			return
		}
		pong, ok := message.(protocol.Pong)
		if !ok || pong.Nonce != 9 {
			pongSeen <- ^uint64(0)
			return
		}
		pongSeen <- pong.Nonce
		if err := peer.Send(ctx, parts[len(parts)-1]); err != nil {
			return
		}
		_ = peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &snapshotRenderer{}
	sess, err := client.NewSession(client.Config{
		Token:     token,
		TLSConfig: clientTLS,
		Limits:    limits,
		IOTimeout: 3 * time.Second,
		Dial:      func(context.Context) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) },
		Input:     client.NewInputState(nil),
	}, renderer, &stateRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()
	defer sess.Input().Disconnect()

	select {
	case nonce := <-pongSeen:
		if nonce != 9 {
			t.Fatalf("host received PONG nonce %d, want 9", nonce)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host never received a PONG for the interleaved PING")
	}

	waitFor(t, 10*time.Second, "final part presented", func() bool { return renderer.count() >= 1 })
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if renderer.count() != 1 {
		t.Fatalf("presented %d snapshots, want exactly 1", renderer.count())
	}
	if !bytes.Equal(renderer.last().Pixels, expected) {
		t.Fatalf("framebuffer presented after interleaved PING is not byte-exact")
	}
}
