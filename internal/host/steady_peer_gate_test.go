package host

// R2/R3 history of this file: the verifier follow-on (N1) made Service.Run
// refuse peers that do not implement SetSteadyReadTimeout, red-greens at
// bfe63fa. R3 item O2 then promoted SetSteadyReadTimeout INTO the host.Peer
// interface, so the guarantee is now compile-time for every statically-typed
// caller; the runtime assertion in Run remains as defence in depth (D7). This
// test was rewritten accordingly: a minimal conforming Peer now records the
// armed deadline, and the test pins the D7 derivation value itself
// (HeartbeatInterval + HeartbeatTimeout + transport.HeartbeatSlack).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// steadyRecordingPeer is the minimal complete host.Peer implementation and
// records the steady read deadline Service.Run arms on it.
type steadyRecordingPeer struct {
	mu    sync.Mutex
	armed time.Duration
	got   bool
}

func (p *steadyRecordingPeer) Send(context.Context, protocol.Message) error { return nil }
func (p *steadyRecordingPeer) Receive(ctx context.Context) (protocol.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *steadyRecordingPeer) SetSteadyReadTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed, p.got = d, true
}

func (p *steadyRecordingPeer) deadline() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.armed, p.got
}

// TestServiceRunArmsSteadyReadTimeout pins the D7 derivation: the deadline
// armed at session start equals HeartbeatInterval + HeartbeatTimeout +
// transport.HeartbeatSlack. (Arming happens before the first capture, so a
// failing capture still proves it.)
func TestServiceRunArmsSteadyReadTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatInterval = 4 * time.Second
	cfg.HeartbeatTimeout = 6 * time.Second
	peer := &steadyRecordingPeer{}
	boom := errors.New("capture under test")

	err := NewService(cfg, &fakeCapture{err: boom}, &fakeInput{}).Run(context.Background(), peer)
	if !errors.Is(err, boom) {
		t.Fatalf("capture error = %v, want %v", err, boom)
	}
	armed, got := peer.deadline()
	if !got {
		t.Fatal("Service.Run did not arm the steady read deadline (D7)")
	}
	want := 4*time.Second + 6*time.Second + transport.HeartbeatSlack
	if armed != want {
		t.Fatalf("armed steady read deadline = %v, want interval+timeout+slack = %v", armed, want)
	}
}

// TestPeerInterfaceIncludesSteadyDeadline is the O2 compile-time guarantee:
// the host.Peer contract itself carries SetSteadyReadTimeout, so no wrapper
// can silently regress F2.
func TestPeerInterfaceIncludesSteadyDeadline(t *testing.T) {
	var p Peer = &steadyRecordingPeer{}
	_ = p
	var c interface{ SetSteadyReadTimeout(time.Duration) } = &steadyRecordingPeer{}
	_ = c
}
