package main

// R0 red-baseline test for the input adapter lifetime (finding F5, AC-5).
//
// At f7bb427 the Linux X11 injector is closed the instant it is created: the
// `defer inj.Close()` sits inside the sync.Once body in run(), so it runs when
// the body returns, and every later injection — and ReleaseAll — executes
// against a dead socket. The wiring was extracted verbatim (bug included) into
// inputOwnership so these tests drive the real production code path.

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"

	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
)

// lifecycleProbeInput is a host.Input that records its own closure and fails
// injection afterwards, mirroring the X11 injector's behavior once its socket
// is closed. (Windows/darwin Close are no-ops, which is exactly why the bug
// stayed invisible there; the lifetime question is platform-neutral.)
type lifecycleProbeInput struct {
	mu       sync.Mutex
	closed   bool
	closeOps int
	uses     int
}

func (p *lifecycleProbeInput) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.closeOps++
}

func (p *lifecycleProbeInput) injection() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("lifecycleProbeInput: injection against closed adapter")
	}
	p.uses++
	return nil
}

func (p *lifecycleProbeInput) Key(context.Context, uint16, protocol.Action, uint8) error {
	return p.injection()
}
func (p *lifecycleProbeInput) Move(context.Context, uint32, uint32) error { return p.injection() }
func (p *lifecycleProbeInput) Button(context.Context, protocol.Button, protocol.Action) error {
	return p.injection()
}
func (p *lifecycleProbeInput) Wheel(context.Context, int16, int16) error { return p.injection() }
func (p *lifecycleProbeInput) ReleaseAll(context.Context) error          { return p.injection() }

func (p *lifecycleProbeInput) stats() (closed bool, closeOps int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed, p.closeOps
}

// TestInputAdapterStaysLiveUntilShutdown asserts AC-5 on the platform-neutral
// wiring: creating the adapter (which happens during session establishment)
// must leave it live, injection must succeed against it, and it must be
// closed exactly once, at shutdown, by the owner — never at creation.
func TestInputAdapterStaysLiveUntilShutdown(t *testing.T) {
	probe := &lifecycleProbeInput{}
	ownership := &inputOwnership{create: func() (host.Input, error) { return probe, nil }}
	cfg := &appConfig{input: ownership.get}

	// Establishment path: the first session asks for the adapter.
	adapter, err := cfg.input()
	if err != nil {
		t.Fatalf("adapter creation: %v", err)
	}
	if adapter == nil {
		t.Fatal("adapter creation returned nil without an error")
	}

	// F5 red signal: the adapter must still be live for injection after
	// creation. At the baseline the Once body closed it already.
	if closed, ops := probe.stats(); closed || ops != 0 {
		t.Fatalf("adapter closed %d time(s) during creation; it must stay live until shutdown (F5)", ops)
	}
	if err := adapter.Key(context.Background(), 0x04, protocol.ActionDown, 0); err != nil {
		t.Fatalf("key injection against freshly created adapter: %v", err)
	}
	if err := adapter.ReleaseAll(context.Background()); err != nil {
		t.Fatalf("ReleaseAll against freshly created adapter: %v", err)
	}

	// Shutdown owns the close, exactly once.
	ownership.close()
	if closed, ops := probe.stats(); !closed || ops != 1 {
		t.Fatalf("after shutdown: closed=%v closeCount=%d, want closed exactly once", closed, ops)
	}
	if err := adapter.ReleaseAll(context.Background()); err == nil {
		t.Fatal("injection must fail against the adapter after shutdown close")
	}

	// close is idempotent: calling it again must not double-close.
	ownership.close()
	if _, ops := probe.stats(); ops != 1 {
		t.Fatalf("adapter close count = %d after repeated shutdown, want 1", ops)
	}
}

// TestLinuxInputAdapterLiveAfterCreation runs the same lifetime assertion
// against the real X11 injector on Linux: a no-op injection round-trip (a left
// modifier press+release, which types nothing) must succeed against the
// adapter as soon as creation returns. Against a closed X11 connection XTEST
// fails immediately, so this is red exactly while the adapter dies at creation.
//
// Skipped when there is no X11 display (DISPLAY unset) or not on linux.
func TestLinuxInputAdapterLiveAfterCreation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("X11 injector is the linux adapter; skipping on " + runtime.GOOS)
	}
	if display, ok := os.LookupEnv("DISPLAY"); !ok || display == "" {
		t.Skip("no X11 display (DISPLAY unset); skipping F5 live-adapter check")
	}
	ownership := newInputOwnership()
	adapter, err := ownership.get()
	if err != nil {
		t.Fatalf("X11 injector creation: %v", err)
	}
	ctx := context.Background()
	// HID usage 0xE1 = left shift: press+release is a real XTEST round-trip
	// with no visible effect on the desktop.
	if err := adapter.Key(ctx, 0xE1, protocol.ActionDown, 0); err != nil {
		t.Fatalf("left-shift press against the freshly created X11 adapter (F5: closed at creation?): %v", err)
	}
	if err := adapter.Key(ctx, 0xE1, protocol.ActionUp, 0); err != nil {
		t.Fatalf("left-shift release against the X11 adapter: %v", err)
	}
	if err := adapter.ReleaseAll(ctx); err != nil {
		t.Fatalf("ReleaseAll against the live X11 adapter: %v", err)
	}
	ownership.close()
}
