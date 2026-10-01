package host

// R2 red-baseline test for the verifier follow-on (N1): the steady-state read
// deadline arming in Service.Run must fail LOUDLY when the peer does not
// implement SetSteadyReadTimeout. A peer that silently skips the arming
// regresses F2 (idle sessions die at the short per-operation I/O deadline),
// and a future wrapper around transport.Peer that drops the method would do
// exactly that without a compile error — so the assertion must be mandatory.
// At 3983c4b the check is optional (`if setter, ok := ...; ok`), and Run
// proceeds happily with a non-conforming peer: red.

import (
	"context"
	"strings"
	"testing"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/protocol"
)

// plainPeer satisfies the documented host.Peer contract (Send/Receive) and
// nothing else.
type plainPeer struct{}

func (plainPeer) Send(context.Context, protocol.Message) error { return nil }
func (plainPeer) Receive(ctx context.Context) (protocol.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestServiceRunFailsLoudWithoutSteadyReadTimeout(t *testing.T) {
	svc := NewService(testConfig(), &fakeCapture{frames: []damage.Image{testImage(1)}}, &fakeInput{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := svc.Run(ctx, plainPeer{})
	if err == nil {
		t.Fatal("Run accepted a peer without SetSteadyReadTimeout; F2 steady-deadline arming would be silently skipped (N1)")
	}
	if !strings.Contains(err.Error(), "SetSteadyReadTimeout") {
		t.Fatalf("Run must fail naming the missing SetSteadyReadTimeout contract, got: %v", err)
	}
}
