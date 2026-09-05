package integration

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"virtualdesktop/internal/client"
)

// snapshotRenderer records every snapshot the client presents.
type snapshotRenderer struct {
	mu        sync.Mutex
	snapshots []client.Snapshot
}

func (r *snapshotRenderer) Present(s client.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots = append(r.snapshots, s)
}
func (r *snapshotRenderer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.snapshots)
}
func (r *snapshotRenderer) last() client.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshots[len(r.snapshots)-1]
}

// stateRecorder records connection-state transitions.
type stateRecorder struct {
	mu     sync.Mutex
	states []client.ConnectionState
}

func (s *stateRecorder) ConnectionState(state client.ConnectionState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, state)
}
func (s *stateRecorder) contains(state client.ConnectionState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.states {
		if st == state {
			return true
		}
	}
	return false
}

// startHost starts a real host service on a fresh listener and returns the
// listener address, the client TLS config, and a stop function.
func startHost(t *testing.T, ctx context.Context, serverTLS *tls.Config, token [32]byte, cap *fakeCapture, input *recordingInput, log *messageLog) (string, func()) {
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
		hostService(ctx, conn, serverTLS, token, cap, input, log)
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// newClient builds a real client session pointing at addr.
func newClient(t *testing.T, ctx context.Context, clientTLS *tls.Config, token [32]byte, addr string, renderer *snapshotRenderer, observer *stateRecorder) (*client.Session, <-chan error) {
	t.Helper()
	sess, err := client.NewSession(client.Config{
		Token:          token,
		TLSConfig:      clientTLS,
		IOTimeout:      5 * time.Second,
		ReconnectDelay: 50 * time.Millisecond,
		Dial:           func(context.Context) (net.Conn, error) { return net.Dial("tcp", addr) },
		Input:          client.NewInputState(nil),
	}, renderer, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()
	return sess, done
}

// TestEndToEndConnectAuthInitialFrame verifies a full connect -> auth ->
// initial keyframe delivery path with the real client and real host service.
func TestEndToEndConnectAuthInitialFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverTLS, clientTLS, _ := testTLS(t)
	var token [32]byte
	token[0] = 9

	cap := &fakeCapture{}
	cap.setImage(4, 4, 0x80)
	input := &recordingInput{}
	log := &messageLog{}
	addr, stop := startHost(t, ctx, serverTLS, token, cap, input, log)
	defer stop()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, addr, renderer, observer)
	defer sess.Input().Disconnect()

	// Wait for the initial frame to be presented.
	deadline := time.After(5 * time.Second)
	for renderer.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial frame")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if snap := renderer.last(); snap.Width != 4 || snap.Height != 4 {
		t.Fatalf("initial snapshot size = %dx%d", snap.Width, snap.Height)
	}

	// The client should report Connected.
	if !observer.contains(client.StateConnected) {
		t.Fatalf("client never reported Connected; states=%v", observer.states)
	}

	// The host must have sent exactly one DisplayConfig, one keyframe, and no
	// input messages.
	kinds := log.hostKinds()
	if kinds["DISPLAY_CONFIG"] != 1 {
		t.Fatalf("host DISPLAY_CONFIG count = %d", kinds["DISPLAY_CONFIG"])
	}
	if kinds["FRAME"] != 1 {
		t.Fatalf("host FRAME count = %d", kinds["FRAME"])
	}
	if kinds["KEY"] != 0 || kinds["POINTER_MOVE"] != 0 || kinds["POINTER_BUTTON"] != 0 || kinds["POINTER_WHEEL"] != 0 {
		t.Fatalf("host sent input messages: %v", kinds)
	}

	cancel()
	<-done
}
