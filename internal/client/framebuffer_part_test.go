package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"runtime/debug"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// partLimits is a small per-message pixel budget that forces a 4x2 keyframe
// built from 2x1 rectangles (each part payload is 31 + 22 + 8 = 61 bytes) into
// one rectangle per part, so every frame in these tests reassembles from
// multiple FRAME_PART messages without a large allocation.
func partLimits() protocol.Limits { return protocol.Limits{MaxPixelPayload: 61} }

func rawRect(x, y, width, height uint32, seed byte) protocol.Rectangle {
	return protocol.Rectangle{
		X: x, Y: y, Width: width, Height: height,
		Encoding: protocol.EncodingRawBGRA, Pixels: pixels(int(width*height), seed),
	}
}

// keyframeRects4x2 tiles a 4x2 display with four non-overlapping 2x1 rectangles.
func keyframeRects4x2() []protocol.Rectangle {
	return []protocol.Rectangle{
		rawRect(0, 0, 2, 1, 1),
		rawRect(2, 0, 2, 1, 10),
		rawRect(0, 1, 2, 1, 20),
		rawRect(2, 1, 2, 1, 30),
	}
}

// forcedRects4x2 tiles the same display with a distinct color pattern from
// keyframeRects4x2, so a test can tell a forced keyframe's committed pixels
// apart from an earlier baseline.
func forcedRects4x2() []protocol.Rectangle {
	return []protocol.Rectangle{
		rawRect(0, 0, 2, 1, 101),
		rawRect(2, 0, 2, 1, 110),
		rawRect(0, 1, 2, 1, 120),
		rawRect(2, 1, 2, 1, 130),
	}
}

func mustSplit(t *testing.T, frame protocol.Frame, limits protocol.Limits) []protocol.FramePart {
	t.Helper()
	parts, err := protocol.SplitFrame(frame, limits)
	if err != nil {
		t.Fatalf("SplitFrame: %v", err)
	}
	return parts
}

// applyParts feeds every part and asserts exactly the final part commits.
func applyParts(t *testing.T, fb *Framebuffer, frame protocol.Frame, limits protocol.Limits) {
	t.Helper()
	parts := mustSplit(t, frame, limits)
	for i, part := range parts {
		committed, err := fb.ApplyPart(part)
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if want := i == len(parts)-1; committed != want {
			t.Fatalf("part %d committed=%v, want %v", i, committed, want)
		}
	}
}

// reference4x2 applies frames to a default-limits framebuffer to produce the
// byte-exact expected output, independent of the part budget under test.
func reference4x2(t *testing.T, frames ...protocol.Frame) Snapshot {
	t.Helper()
	fb := NewFramebuffer(protocol.DefaultLimits())
	if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if err := fb.Apply(frame); err != nil {
			t.Fatal(err)
		}
	}
	return fb.Snapshot()
}

func configuredFramebuffer(t *testing.T, limits protocol.Limits) *Framebuffer {
	t.Helper()
	fb := NewFramebuffer(limits)
	if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	return fb
}

func TestFramebufferMultiPartKeyframeCommitsOnce(t *testing.T) {
	limits := partLimits()
	fb := configuredFramebuffer(t, limits)
	frame := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	parts := mustSplit(t, frame, limits)
	if len(parts) != 4 {
		t.Fatalf("got %d parts, want 4", len(parts))
	}

	before := fb.Snapshot()
	for i, part := range parts[:len(parts)-1] {
		committed, err := fb.ApplyPart(part)
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if committed {
			t.Fatalf("part %d committed before the final part", i)
		}
		if got := fb.Snapshot(); got.FrameSequence != before.FrameSequence || !bytes.Equal(got.Pixels, before.Pixels) {
			t.Fatalf("part %d changed the visible framebuffer", i)
		}
	}

	committed, err := fb.ApplyPart(parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("final part did not commit")
	}
	want := reference4x2(t, frame)
	got := fb.Snapshot()
	if got.FrameSequence != want.FrameSequence || !bytes.Equal(got.Pixels, want.Pixels) {
		t.Fatalf("multi-part keyframe output differs from a single FRAME")
	}
}

func TestFramebufferMultiPartDeltaPreservesUntilCommit(t *testing.T) {
	limits := partLimits()
	fb := configuredFramebuffer(t, limits)
	base := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	applyParts(t, fb, base, limits)
	baseSnapshot := fb.Snapshot()

	delta := protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{
		rawRect(0, 0, 1, 1, 90),
		rawRect(1, 0, 1, 1, 91),
	}}
	parts := mustSplit(t, delta, limits)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}

	committed, err := fb.ApplyPart(parts[0])
	if err != nil || committed {
		t.Fatalf("first delta part: committed=%v err=%v", committed, err)
	}
	if got := fb.Snapshot(); got.FrameSequence != baseSnapshot.FrameSequence || !bytes.Equal(got.Pixels, baseSnapshot.Pixels) {
		t.Fatal("visible framebuffer changed before the delta committed")
	}

	committed, err = fb.ApplyPart(parts[1])
	if err != nil || !committed {
		t.Fatalf("final delta part: committed=%v err=%v", committed, err)
	}
	want := reference4x2(t, base, delta)
	got := fb.Snapshot()
	if got.FrameSequence != want.FrameSequence || !bytes.Equal(got.Pixels, want.Pixels) {
		t.Fatalf("multi-part delta output differs from a single FRAME")
	}
}

// TestFramebufferPartRejectsStaleKeyframeSequence proves the multi-part
// reassembler rejects a stale or duplicate keyframe at part 0: it starts no
// staging buffer, leaves the committed pixels and sequence untouched, does not
// let the rejected sequence's tail continue, and still accepts a valid higher
// keyframe (the host's periodic refresh).
func TestFramebufferPartRejectsStaleKeyframeSequence(t *testing.T) {
	limits := partLimits()
	fb := configuredFramebuffer(t, limits)
	base := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	applyParts(t, fb, base, limits)
	before := fb.Snapshot()

	stale := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: forcedRects4x2()}
	staleParts := mustSplit(t, stale, limits)
	if len(staleParts) < 2 {
		t.Fatalf("stale keyframe split into %d parts, want >= 2", len(staleParts))
	}
	if committed, err := fb.ApplyPart(staleParts[0]); err == nil || committed {
		t.Fatalf("stale keyframe part 0: committed=%v err=%v, want fatal rejection", committed, err)
	}
	if fb.staging != nil {
		t.Fatal("stale keyframe left an active staging buffer")
	}
	if got := fb.Snapshot(); got.FrameSequence != before.FrameSequence || !bytes.Equal(got.Pixels, before.Pixels) {
		t.Fatal("stale keyframe part 0 mutated the committed frame")
	}
	// No staging was begun, so the rejected sequence's tail cannot continue it.
	if _, err := fb.ApplyPart(staleParts[1]); err == nil {
		t.Fatal("continued a stale keyframe whose part 0 was rejected")
	}

	// A valid higher keyframe still reassembles and commits.
	next := protocol.Frame{Generation: 1, FrameSequence: 2, Keyframe: true, Rectangles: forcedRects4x2()}
	applyParts(t, fb, next, limits)
	want := reference4x2(t, base, next)
	if got := fb.Snapshot(); got.FrameSequence != want.FrameSequence || !bytes.Equal(got.Pixels, want.Pixels) {
		t.Fatal("valid higher keyframe did not commit")
	}
}

func TestFramebufferPartZeroBaseMismatchRequestsKeyframe(t *testing.T) {
	limits := partLimits()
	fb := configuredFramebuffer(t, limits)
	base := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	applyParts(t, fb, base, limits)
	before := fb.Snapshot()

	// A delta whose base names a sequence the client never committed.
	delta := protocol.Frame{Generation: 1, FrameSequence: 3, BaseFrameSequence: 2, Rectangles: []protocol.Rectangle{
		rawRect(0, 0, 1, 1, 90),
		rawRect(1, 0, 1, 1, 91),
	}}
	parts := mustSplit(t, delta, limits)
	committed, err := fb.ApplyPart(parts[0])
	if !errors.Is(err, ErrKeyframeRequired) || committed {
		t.Fatalf("got committed=%v err=%v, want ErrKeyframeRequired", committed, err)
	}
	if got := fb.Snapshot(); got.FrameSequence != before.FrameSequence || !bytes.Equal(got.Pixels, before.Pixels) {
		t.Fatal("recoverable base mismatch changed the visible framebuffer")
	}
	// The host is already mid-transmission: the trailing part belongs to the
	// rejected logical frame and is drained as its tail, not mistaken for a
	// fresh continuation. It is consumed without touching pixels.
	if committed, err := fb.ApplyPart(parts[1]); err != nil || committed {
		t.Fatalf("draining the rejected tail: committed=%v err=%v, want (false, nil)", committed, err)
	}
	if got := fb.Snapshot(); got.FrameSequence != before.FrameSequence || !bytes.Equal(got.Pixels, before.Pixels) {
		t.Fatal("draining the rejected tail changed the visible framebuffer")
	}
	// With the drain complete, the host's forced keyframe commits and presents
	// (returns committed=true) exactly once.
	forced := protocol.Frame{Generation: 1, FrameSequence: 2, Keyframe: true, Rectangles: keyframeRects4x2()}
	forcedParts := mustSplit(t, forced, limits)
	for i, part := range forcedParts {
		committed, err := fb.ApplyPart(part)
		if err != nil {
			t.Fatalf("forced keyframe part %d: %v", i, err)
		}
		if want := i == len(forcedParts)-1; committed != want {
			t.Fatalf("forced keyframe part %d committed=%v, want %v", i, committed, want)
		}
	}
	want := reference4x2(t, forced)
	if got := fb.Snapshot(); got.FrameSequence != want.FrameSequence || !bytes.Equal(got.Pixels, want.Pixels) {
		t.Fatal("forced keyframe after drain did not commit")
	}
}

func TestFramebufferRejectsMalformedParts(t *testing.T) {
	keyPart := func(index uint16) protocol.FramePart {
		return protocol.FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: index, PartCount: 2,
			Rectangles: []protocol.Rectangle{rawRect(2*uint32(index), 0, 2, 1, byte(index))}}
	}

	t.Run("part without part zero", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		if _, err := fb.ApplyPart(keyPart(1)); err == nil {
			t.Fatal("accepted a non-zero part before part 0")
		}
	})

	t.Run("duplicate part zero", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		if _, err := fb.ApplyPart(keyPart(0)); err != nil {
			t.Fatal(err)
		}
		if _, err := fb.ApplyPart(keyPart(0)); err == nil {
			t.Fatal("accepted a duplicate part 0")
		}
	})

	t.Run("out of order", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		if _, err := fb.ApplyPart(keyPart(0)); err != nil {
			t.Fatal(err)
		}
		outOfOrder := keyPart(1)
		outOfOrder.PartCount = 3
		if _, err := fb.ApplyPart(outOfOrder); err == nil {
			t.Fatal("accepted an out-of-order part")
		}
	})

	t.Run("inconsistent header", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		if _, err := fb.ApplyPart(keyPart(0)); err != nil {
			t.Fatal(err)
		}
		inconsistent := keyPart(1)
		inconsistent.FrameSequence = 2 // valid alone, but not this sequence
		if _, err := fb.ApplyPart(inconsistent); err == nil {
			t.Fatal("accepted a part with an inconsistent header")
		}
	})

	t.Run("stale generation", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		stale := keyPart(0)
		stale.Generation = 2
		if _, err := fb.ApplyPart(stale); err == nil {
			t.Fatal("accepted a part from another generation")
		}
	})

	t.Run("cross part overlap", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		if _, err := fb.ApplyPart(keyPart(0)); err != nil {
			t.Fatal(err)
		}
		overlap := protocol.FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 1, PartCount: 2,
			Rectangles: []protocol.Rectangle{rawRect(1, 0, 2, 1, 5)}}
		if _, err := fb.ApplyPart(overlap); err == nil {
			t.Fatal("accepted rectangles overlapping across parts")
		}
	})

	t.Run("incomplete keyframe coverage", func(t *testing.T) {
		fb := configuredFramebuffer(t, partLimits())
		// Two parts covering only the top row: coverage 4 of 8 pixels.
		part0 := protocol.FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2,
			Rectangles: []protocol.Rectangle{rawRect(0, 0, 2, 1, 1)}}
		part1 := protocol.FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 1, PartCount: 2,
			Rectangles: []protocol.Rectangle{rawRect(2, 0, 2, 1, 2)}}
		before := fb.Snapshot()
		if committed, err := fb.ApplyPart(part0); err != nil || committed {
			t.Fatalf("part 0: committed=%v err=%v", committed, err)
		}
		committed, err := fb.ApplyPart(part1)
		if err == nil || committed {
			t.Fatalf("incomplete keyframe: committed=%v err=%v", committed, err)
		}
		if got := fb.Snapshot(); got.FrameSequence != before.FrameSequence || !bytes.Equal(got.Pixels, before.Pixels) {
			t.Fatal("incomplete keyframe presented partial pixels")
		}
	})

	t.Run("cumulative rectangle overflow", func(t *testing.T) {
		limits := protocol.Limits{MaxPixelPayload: 61, MaxRectangles: 2}
		fb := configuredFramebuffer(t, limits)
		frame := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []protocol.Rectangle{
			rawRect(0, 0, 2, 1, 1), rawRect(2, 0, 2, 1, 2), rawRect(0, 1, 2, 1, 3),
		}}
		parts := mustSplit(t, frame, limits)
		if len(parts) != 3 {
			t.Fatalf("got %d parts, want 3", len(parts))
		}
		if _, err := fb.ApplyPart(parts[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := fb.ApplyPart(parts[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := fb.ApplyPart(parts[2]); err == nil {
			t.Fatal("accepted a cumulative rectangle count above the limit")
		}
	})
}

func TestFramebufferRejectsInterleavedFrame(t *testing.T) {
	limits := partLimits()
	fb := configuredFramebuffer(t, limits)
	parts := mustSplit(t, protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}, limits)
	if _, err := fb.ApplyPart(parts[0]); err != nil {
		t.Fatal(err)
	}
	interleaved := protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{rawRect(0, 0, 1, 1, 7)}}
	if err := fb.Apply(interleaved); err == nil {
		t.Fatal("accepted a FRAME interleaved with a part sequence")
	}
}

func TestFramebufferResetAndConfigureClearStaging(t *testing.T) {
	limits := partLimits()
	keyframe := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	parts := mustSplit(t, keyframe, limits)

	t.Run("reset", func(t *testing.T) {
		fb := configuredFramebuffer(t, limits)
		if _, err := fb.ApplyPart(parts[0]); err != nil {
			t.Fatal(err)
		}
		fb.Reset()
		if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			t.Fatal(err)
		}
		// A fresh sequence must start cleanly; leftover staging would reject
		// the new part 0 as a restart.
		applyParts(t, fb, keyframe, limits)
	})

	t.Run("configure", func(t *testing.T) {
		fb := configuredFramebuffer(t, limits)
		if _, err := fb.ApplyPart(parts[0]); err != nil {
			t.Fatal(err)
		}
		if err := fb.Configure(protocol.DisplayConfig{Generation: 2, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err == nil {
			t.Fatal("accepted a DISPLAY_CONFIG interleaved with a part sequence")
		}
		// Staging was dropped: the next part cannot continue the old frame.
		if _, err := fb.ApplyPart(parts[1]); err == nil {
			t.Fatal("continued a frame across a reconfigure")
		}
	})
}

// TestFramebufferV1SingleFramePathUnchanged confirms the ordinary FRAME path
// (what a v1 peer sends) still commits byte-for-byte and leaves no staging
// behind, so the v2 reassembler is additive.
func TestFramebufferV1SingleFramePathUnchanged(t *testing.T) {
	fb := NewFramebuffer(protocol.DefaultLimits())
	if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	keyframe := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	if err := fb.Apply(keyframe); err != nil {
		t.Fatal(err)
	}
	delta := protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{rawRect(0, 0, 2, 2, 77)}}
	if err := fb.Apply(delta); err != nil {
		t.Fatal(err)
	}
	want := reference4x2(t, keyframe, delta)
	if got := fb.Snapshot(); !bytes.Equal(got.Pixels, want.Pixels) || got.FrameSequence != want.FrameSequence {
		t.Fatal("ordinary FRAME path changed")
	}
	// No staging exists after ordinary frames: a part 1 has nothing to join.
	orphan := protocol.FramePart{Generation: 1, FrameSequence: 3, BaseFrameSequence: 2, PartIndex: 1, PartCount: 2,
		Rectangles: []protocol.Rectangle{rawRect(0, 0, 1, 1, 8)}}
	if _, err := fb.ApplyPart(orphan); err == nil {
		t.Fatal("ordinary FRAME left staging behind")
	}
}

// TestSessionPresentsOnceOnFinalPartWithPingInterleave drives the real client
// against a v2 host that interleaves a PING between the penultimate and final
// part. It proves PING/PONG does not drop staging, nothing is presented before
// the final part, and the committed frame is presented exactly once.
func TestSessionPresentsOnceOnFinalPartWithPingInterleave(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	serverConn := tls.Server(serverRaw, serverTLS)
	clientConn := tls.Client(clientRaw, clientTLS)
	var token [32]byte
	token[0] = 21
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	limits := partLimits()
	frame := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	parts := mustSplit(t, frame, limits)
	if len(parts) != 4 {
		t.Fatalf("got %d parts, want 4", len(parts))
	}

	pongSeen := make(chan struct{})
	releaseFinalPart := make(chan struct{})
	hostResult := make(chan error, 1)
	go func() {
		if err := transport.Handshake(ctx, serverConn, time.Second); err != nil {
			hostResult <- err
			return
		}
		peer := transport.NewPeer(serverConn, protocol.RoleHost, limits, time.Second)
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
		if _, ok := message.(protocol.ClientHello); !ok {
			hostResult <- errUnexpected("CLIENT_HELLO", message)
			return
		}
		peer.PinVersion(protocol.Version2)
		if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version2}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		for i := 0; i < len(parts)-1; i++ {
			if err := peer.Send(ctx, parts[i]); err != nil {
				hostResult <- err
				return
			}
		}
		if err := peer.Send(ctx, protocol.Ping{Nonce: 7}); err != nil {
			hostResult <- err
			return
		}
		message, err = peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		if pong, ok := message.(protocol.Pong); !ok || pong.Nonce != 7 {
			hostResult <- errUnexpected("PONG{7}", message)
			return
		}
		close(pongSeen)
		select {
		case <-releaseFinalPart:
		case <-ctx.Done():
			hostResult <- ctx.Err()
			return
		}
		if err := peer.Send(ctx, parts[len(parts)-1]); err != nil {
			hostResult <- err
			return
		}
		hostResult <- peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &recordingRenderer{presented: make(chan Snapshot, 2)}
	session, err := NewSession(Config{Token: token, TLSConfig: clientTLS, Limits: limits, IOTimeout: time.Second}, renderer, &recordingObserver{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.ServeConn(ctx, clientConn) }()

	select {
	case <-pongSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("host never received the PONG")
	}
	select {
	case s := <-renderer.presented:
		t.Fatalf("presented before the final part: %#v", s)
	default:
	}
	close(releaseFinalPart)

	want := reference4x2(t, frame)
	select {
	case s := <-renderer.presented:
		if s.Generation != 1 || s.Width != 4 || s.Height != 2 || !bytes.Equal(s.Pixels, want.Pixels) {
			t.Fatalf("presented snapshot mismatch: %#v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("final part did not present a snapshot")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-renderer.presented:
		t.Fatalf("presented more than once: %#v", s)
	default:
	}
}

// TestSessionRequestsKeyframeOnPartZeroBaseMismatch proves the recoverable
// path reaches the wire: after a committed keyframe, a delta part 0 whose base
// is unknown makes the client send KEYFRAME_REQUEST rather than dropping the
// session.
func TestSessionRequestsKeyframeOnPartZeroBaseMismatch(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	serverConn := tls.Server(serverRaw, serverTLS)
	clientConn := tls.Client(clientRaw, clientTLS)
	var token [32]byte
	token[0] = 22
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	limits := partLimits()
	keyframe := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	keyParts := mustSplit(t, keyframe, limits)
	delta := protocol.Frame{Generation: 1, FrameSequence: 5, BaseFrameSequence: 4, Rectangles: []protocol.Rectangle{
		rawRect(0, 0, 1, 1, 90), rawRect(1, 0, 1, 1, 91),
	}}
	deltaParts := mustSplit(t, delta, limits)

	requestSeen := make(chan uint64, 1)
	hostResult := make(chan error, 1)
	go func() {
		if err := transport.Handshake(ctx, serverConn, time.Second); err != nil {
			hostResult <- err
			return
		}
		peer := transport.NewPeer(serverConn, protocol.RoleHost, limits, time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			hostResult <- err
			return
		}
		peer.SetVersionWindow(protocol.Version1, protocol.Version2)
		if _, err := peer.Receive(ctx); err != nil {
			hostResult <- err
			return
		}
		peer.PinVersion(protocol.Version2)
		if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version2}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		for _, part := range keyParts {
			if err := peer.Send(ctx, part); err != nil {
				hostResult <- err
				return
			}
		}
		if err := peer.Send(ctx, deltaParts[0]); err != nil {
			hostResult <- err
			return
		}
		message, err := peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		request, ok := message.(protocol.KeyframeRequest)
		if !ok {
			hostResult <- errUnexpected("KEYFRAME_REQUEST", message)
			return
		}
		requestSeen <- request.Generation
		hostResult <- peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &recordingRenderer{presented: make(chan Snapshot, 1)}
	session, err := NewSession(Config{Token: token, TLSConfig: clientTLS, Limits: limits, IOTimeout: time.Second}, renderer, &recordingObserver{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.ServeConn(ctx, clientConn) }()

	select {
	case generation := <-requestSeen:
		if generation != 1 {
			t.Fatalf("KEYFRAME_REQUEST generation=%d, want 1", generation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client never requested a keyframe")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
}

// TestFramebufferDrainRejectsMismatchedTail proves the drain state is strict:
// once part 0 is rejected, a tail part with a different header, a wrong index,
// or a restart at part 0 is fatal rather than silently consumed, so a desynced
// stream cannot resume unnoticed.
func TestFramebufferDrainRejectsMismatchedTail(t *testing.T) {
	limits := partLimits()
	base := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	// base 4 against a committed sequence of 1 is unknown, so part 0 is
	// recoverably rejected and the following two parts must be drained (one
	// 1x1 rectangle per part at this budget).
	delta := protocol.Frame{Generation: 1, FrameSequence: 5, BaseFrameSequence: 4, Rectangles: []protocol.Rectangle{
		rawRect(0, 0, 1, 1, 90), rawRect(1, 0, 1, 1, 91), rawRect(2, 0, 1, 1, 92),
	}}
	deltaParts := mustSplit(t, delta, limits)
	if len(deltaParts) != 3 {
		t.Fatalf("delta split into %d parts, want 3", len(deltaParts))
	}

	rejectPartZero := func(t *testing.T) *Framebuffer {
		t.Helper()
		fb := configuredFramebuffer(t, limits)
		applyParts(t, fb, base, limits)
		if _, err := fb.ApplyPart(deltaParts[0]); !errors.Is(err, ErrKeyframeRequired) {
			t.Fatalf("part 0: got %v, want ErrKeyframeRequired", err)
		}
		return fb
	}

	t.Run("mismatched header", func(t *testing.T) {
		fb := rejectPartZero(t)
		bad := deltaParts[1]
		bad.FrameSequence = 6 // valid alone, but not this drained sequence
		if _, err := fb.ApplyPart(bad); err == nil {
			t.Fatal("accepted a tail part with a mismatched header during drain")
		}
	})

	t.Run("out of order", func(t *testing.T) {
		fb := rejectPartZero(t)
		if _, err := fb.ApplyPart(deltaParts[2]); err == nil {
			t.Fatal("accepted an out-of-order tail part during drain")
		}
	})

	t.Run("restart at part zero", func(t *testing.T) {
		fb := rejectPartZero(t)
		if _, err := fb.ApplyPart(deltaParts[0]); err == nil {
			t.Fatal("accepted a part 0 restart during drain")
		}
	})
}

// TestFramebufferResetAndConfigureClearDiscard proves a rejected sequence's
// drain state does not survive a Reset or a reconfigure: the next sequence
// starts fresh rather than being rejected as a restart during a stale drain.
func TestFramebufferResetAndConfigureClearDiscard(t *testing.T) {
	limits := partLimits()
	base := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	delta := protocol.Frame{Generation: 1, FrameSequence: 5, BaseFrameSequence: 4, Rectangles: []protocol.Rectangle{
		rawRect(0, 0, 1, 1, 90), rawRect(1, 0, 1, 1, 91), rawRect(2, 0, 1, 1, 92),
	}}
	deltaParts := mustSplit(t, delta, limits)

	t.Run("reset", func(t *testing.T) {
		fb := configuredFramebuffer(t, limits)
		if _, err := fb.ApplyPart(deltaParts[0]); !errors.Is(err, ErrKeyframeRequired) {
			t.Fatalf("part 0: got %v, want ErrKeyframeRequired", err)
		}
		fb.Reset()
		if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			t.Fatal(err)
		}
		// A fresh sequence must start cleanly; leftover discard would reject
		// its part 0 as a restart.
		applyParts(t, fb, base, limits)
	})

	t.Run("configure", func(t *testing.T) {
		fb := configuredFramebuffer(t, limits)
		if _, err := fb.ApplyPart(deltaParts[0]); !errors.Is(err, ErrKeyframeRequired) {
			t.Fatalf("part 0: got %v, want ErrKeyframeRequired", err)
		}
		if err := fb.Configure(protocol.DisplayConfig{Generation: 2, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err == nil {
			t.Fatal("accepted a DISPLAY_CONFIG interleaved with a drain")
		}
		// Discard was cleared alongside staging, so a new generation's part 0
		// begins a fresh sequence instead of being consumed as a stale tail.
		fresh := protocol.Frame{Generation: 2, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
		applyParts(t, fb, fresh, limits)
	})
}

// TestFramebufferApplyReusesSpareBuffer proves an ordinary FRAME reuses the
// spare buffer as its scratch next-frame buffer instead of allocating a fresh
// full-size copy per frame. After Configure the framebuffer owns exactly two
// backing arrays (pixels and spare); with reuse every successful Apply swaps
// between them, so the visible pixels never point at a third allocation. GC is
// disabled so a swept array cannot be recycled into a false pass.
func TestFramebufferApplyReusesSpareBuffer(t *testing.T) {
	previous := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previous)

	fb := configuredFramebuffer(t, protocol.DefaultLimits()) // 4x2
	base := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	if err := fb.Apply(base); err != nil {
		t.Fatal(err)
	}
	// The only two arrays Apply is allowed to keep using after Configure.
	allowed := map[*byte]bool{&fb.pixels[0]: true, &fb.spare[0]: true}

	for i := 0; i < 8; i++ {
		delta := protocol.Frame{Generation: 1, FrameSequence: uint64(2 + i), BaseFrameSequence: uint64(1 + i),
			Rectangles: []protocol.Rectangle{rawRect(0, 0, 1, 1, byte(i))}}
		if err := fb.Apply(delta); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if p := &fb.pixels[0]; !allowed[p] {
			t.Fatalf("frame %d allocated a new full-size pixel buffer", i)
		}
	}
}

// TestSessionDrainsRejectedDeltaTailWithoutReconnect drives the real client
// through Run (so any torn-down connection would reconnect) against a v2 host
// that, after the client rejects a delta's part 0, sends the remainder of that
// logical frame and interleaves a PING mid-drain. It proves the drain state
// survives PING/PONG, exactly one KEYFRAME_REQUEST is sent, the session never
// reconnects, and the host's forced keyframe commits and is presented once.
func TestSessionDrainsRejectedDeltaTailWithoutReconnect(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	var token [32]byte
	token[0] = 23
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	limits := partLimits()
	keyframe := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	keyParts := mustSplit(t, keyframe, limits)
	// Three 1x1 rectangles force a three-part delta (one rectangle per part at
	// this budget) so a PING can be interleaved while the drain is still active.
	delta := protocol.Frame{Generation: 1, FrameSequence: 5, BaseFrameSequence: 4, Rectangles: []protocol.Rectangle{
		rawRect(0, 0, 1, 1, 90), rawRect(1, 0, 1, 1, 91), rawRect(2, 0, 1, 1, 92),
	}}
	deltaParts := mustSplit(t, delta, limits)
	if len(deltaParts) != 3 {
		t.Fatalf("delta split into %d parts, want 3", len(deltaParts))
	}
	forced := protocol.Frame{Generation: 1, FrameSequence: 2, Keyframe: true, Rectangles: forcedRects4x2()}
	forcedParts := mustSplit(t, forced, limits)

	hostResult := make(chan error, 1)
	go func() {
		serverConn := tls.Server(serverRaw, serverTLS)
		if err := transport.Handshake(ctx, serverConn, time.Second); err != nil {
			hostResult <- err
			return
		}
		peer := transport.NewPeer(serverConn, protocol.RoleHost, limits, time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			hostResult <- err
			return
		}
		peer.SetVersionWindow(protocol.Version1, protocol.Version2)
		if _, err := peer.Receive(ctx); err != nil {
			hostResult <- err
			return
		}
		peer.PinVersion(protocol.Version2)
		if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version2}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		for _, part := range keyParts {
			if err := peer.Send(ctx, part); err != nil {
				hostResult <- err
				return
			}
		}
		// Rejecting part 0 makes the client request a keyframe.
		if err := peer.Send(ctx, deltaParts[0]); err != nil {
			hostResult <- err
			return
		}
		message, err := peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		if _, ok := message.(protocol.KeyframeRequest); !ok {
			hostResult <- errUnexpected("exactly one KEYFRAME_REQUEST", message)
			return
		}
		// Drain the rejected tail, interleaving a PING mid-sequence: reading
		// the PONG (rather than another KEYFRAME_REQUEST) proves the drain state
		// survived the control message and the tail parts did not each trigger a
		// request.
		if err := peer.Send(ctx, deltaParts[1]); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.Ping{Nonce: 7}); err != nil {
			hostResult <- err
			return
		}
		message, err = peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		if pong, ok := message.(protocol.Pong); !ok || pong.Nonce != 7 {
			hostResult <- errUnexpected("PONG{7}", message)
			return
		}
		if err := peer.Send(ctx, deltaParts[2]); err != nil {
			hostResult <- err
			return
		}
		for _, part := range forcedParts {
			if err := peer.Send(ctx, part); err != nil {
				hostResult <- err
				return
			}
		}
		hostResult <- peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &recordingRenderer{presented: make(chan Snapshot, 4)}
	states := &recordingObserver{}
	dialCount := 0
	session, err := NewSession(Config{
		Token: token, TLSConfig: clientTLS, Limits: limits, IOTimeout: time.Second,
		ReconnectDelay: time.Millisecond,
		Dial:           func(context.Context) (net.Conn, error) { dialCount++; return clientRaw, nil },
	}, renderer, states)
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- session.Run(ctx) }()

	baseline := <-renderer.presented
	if baseline.FrameSequence != 1 || baseline.Generation != 1 {
		t.Fatalf("baseline snapshot %#v", baseline)
	}
	want := reference4x2(t, forced)
	select {
	case s := <-renderer.presented:
		if s.FrameSequence != 2 || !bytes.Equal(s.Pixels, want.Pixels) {
			t.Fatalf("forced keyframe snapshot mismatch: %#v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forced keyframe was never presented")
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned %v; a rejected delta tail must not tear down the session", err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
	if dialCount != 1 {
		t.Fatalf("dialed %d times; a rejected delta tail caused a reconnect", dialCount)
	}
	if states.contains(StateReconnecting) || states.contains(StateError) {
		t.Fatalf("client entered a failure state: %v", states.states)
	}
	if !states.contains(StateConnected) || !states.contains(StateClosed) {
		t.Fatalf("states %v", states.states)
	}
	select {
	case s := <-renderer.presented:
		t.Fatalf("presented more than once for the forced keyframe: %#v", s)
	default:
	}
}
