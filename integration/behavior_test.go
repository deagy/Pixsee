package integration

import (
	"context"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/protocol"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestEndToEndIncrementalUpdate verifies that a change to the captured image
// produces an incremental (delta) frame that the client applies and presents.
func TestEndToEndIncrementalUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })

	// Change the captured image color; the host should emit an incremental frame.
	cap.setImage(8, 8, 0x20)
	waitFor(t, 5*time.Second, "incremental frame", func() bool { return renderer.count() >= 2 })

	// The host must have sent exactly one keyframe and at least one incremental
	// frame, with no second DISPLAY_CONFIG (geometry unchanged).
	kinds := log.hostKinds()
	if kinds["DISPLAY_CONFIG"] != 1 {
		t.Fatalf("expected 1 DISPLAY_CONFIG, got %d", kinds["DISPLAY_CONFIG"])
	}
	if kinds["FRAME"] < 2 {
		t.Fatalf("expected >=2 FRAMEs (keyframe+incremental), got %d", kinds["FRAME"])
	}
}

// TestEndToEndResizeGeometry verifies that a change to the capture dimensions
// produces a new DISPLAY_CONFIG (new generation) followed by a keyframe, and
// the client presents the new geometry.
func TestEndToEndResizeGeometry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if snap := renderer.last(); snap.Width != 8 || snap.Height != 8 {
		t.Fatalf("initial geometry = %dx%d", snap.Width, snap.Height)
	}

	// Change dimensions; the host must emit a new DISPLAY_CONFIG then a keyframe.
	cap.setImage(16, 16, 0x30)
	waitFor(t, 5*time.Second, "resized keyframe", func() bool {
		snap := renderer.last()
		return snap.Width == 16 && snap.Height == 16
	})

	kinds := log.hostKinds()
	if kinds["DISPLAY_CONFIG"] != 2 {
		t.Fatalf("expected 2 DISPLAY_CONFIG (resize), got %d", kinds["DISPLAY_CONFIG"])
	}
	if kinds["FRAME"] < 2 {
		t.Fatalf("expected >=2 FRAMEs after resize, got %d", kinds["FRAME"])
	}
}

// TestEndToEndKeyboardInput verifies that a key pressed on the client is
// injected on the host through the Input adapter.
func TestEndToEndKeyboardInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })

	// Focus the input and press the A key (HID usage 0x04), then release it.
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatal(err)
	}
	if err := sess.Input().Key(0x04, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "key injected", func() bool { n, _ := input.snapshot(); return n >= 1 })
	if err := sess.Input().Key(0x04, protocol.ActionUp, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "key release injected", func() bool { n, _ := input.snapshot(); return n >= 2 })

	// The host must NOT have sent any input messages to the client.
	if kinds := log.hostKinds(); kinds["KEY"] != 0 {
		t.Fatalf("host must not send KEY messages, got %d", kinds["KEY"])
	}
	cancel()
}

// TestEndToEndPointerInput verifies pointer move, button, and wheel events
// captured on the client are injected on the host.
func TestEndToEndPointerInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatal(err)
	}

	// Pointer move to remote (2, 3).
	if err := sess.Input().Pointer(2, 3, 8, 8); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "pointer move injected", func() bool { n, _ := input.snapshot(); return n >= 1 })

	// Left button down/up.
	if err := sess.Input().Button(protocol.ButtonLeft, protocol.ActionDown); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "button injected", func() bool { n, _ := input.snapshot(); return n >= 2 })
	if err := sess.Input().Button(protocol.ButtonLeft, protocol.ActionUp); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "button release injected", func() bool { n, _ := input.snapshot(); return n >= 3 })

	// Wheel scroll.
	if err := sess.Input().Wheel(0, 120); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "wheel injected", func() bool { n, _ := input.snapshot(); return n >= 4 })

	if kinds := log.hostKinds(); kinds["POINTER_MOVE"] != 0 || kinds["POINTER_BUTTON"] != 0 || kinds["POINTER_WHEEL"] != 0 {
		t.Fatalf("host must not echo input messages back to client: %v", kinds)
	}
	cancel()
}

// TestDisconnectReleasesStickyInput verifies that when the client disconnects,
// the host releases any held keys and buttons (via ReleaseAll).
func TestDisconnectReleasesStickyInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatal(err)
	}
	if err := sess.Input().Key(0x04, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "key injected", func() bool { n, _ := input.snapshot(); return n >= 1 })

	// Drop the client connection abruptly; the host must release the sticky key.
	// The client's Disconnect() releases held keys and the host must flush the
	// release on shutdown.
	sess.Input().Disconnect()
	cancel()

	// The host must release sticky input on disconnect/shutdown.
	waitFor(t, 5*time.Second, "release on disconnect", func() bool { _, r := input.snapshot(); return r >= 1 })
}

// TestUnauthorizedClientRejected verifies that a client presenting the wrong
// token is rejected at authentication and never reaches the Connected state.
func TestUnauthorizedClientRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	// Wrong token: [1] instead of [9].
	wrong := [32]byte{}
	wrong[0] = 1
	sess, done := newClient(t, ctx, clientTLS, wrong, addr, renderer, observer)
	defer sess.Input().Disconnect()

	// The client must never reach Connected. The host closes the connection on
	// a bad token, so the client reports an error and then retries forever on a
	// live context; we verify it never reaches Connected, then cancel.
	waitFor(t, 5*time.Second, "error state", func() bool {
		return observer.contains(client.StateError)
	})
	if observer.contains(client.StateConnected) {
		t.Fatal("client reached Connected with an unauthorized token")
	}
	cancel()
	<-done
}

// TestTrafficDirectionIsolation verifies that the host sends only session/
// control metadata and visual updates (no input), and the client sends only
// session/control metadata and input events (no visual data).
func TestTrafficDirectionIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(8, 8, 0x10)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 5*time.Second, "initial keyframe", func() bool { return renderer.count() >= 1 })
	if err := sess.Input().SetFocused(true); err != nil {
		t.Fatal(err)
	}
	if err := sess.Input().Key(0x04, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "key injected", func() bool { n, _ := input.snapshot(); return n >= 1 })

	// Host -> client: must be only SERVER_HELLO, DISPLAY_CONFIG, FRAME (+ close).
	hostBad := map[string]bool{
		"AUTH": true, "CLIENT_HELLO": true, "KEYFRAME_REQUEST": true,
		"KEY": true, "POINTER_MOVE": true, "POINTER_BUTTON": true,
		"POINTER_WHEEL": true, "FOCUS_LOST": true,
	}
	for _, m := range log.hostOut {
		name := m.Type().String()
		if hostBad[name] {
			t.Fatalf("host sent illegal message %s to client", name)
		}
	}

	// Client -> host: must be only AUTH, CLIENT_HELLO, KEYFRAME_REQUEST (+ input).
	clientBad := map[string]bool{
		"SERVER_HELLO": true, "DISPLAY_CONFIG": true, "FRAME": true,
	}
	for _, m := range log.hostIn {
		name := m.Type().String()
		if clientBad[name] {
			t.Fatalf("client sent illegal message %s to host", name)
		}
	}

	// Sanity: the host sent a keyframe and the client sent a key.
	if log.hostKinds()["FRAME"] == 0 {
		t.Fatal("host never sent a frame")
	}
	if log.clientKinds()["KEY"] == 0 {
		t.Fatal("client never sent a key")
	}

	cancel()
}
