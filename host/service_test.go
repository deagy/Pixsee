package host

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/protocol"
)

type fakeCapture struct {
	mu     sync.Mutex
	frames []damage.Image
	err    error
	calls  int
}

func (f *fakeCapture) Capture(context.Context, uint32) (damage.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return damage.Image{}, f.err
	}
	if len(f.frames) == 0 {
		return damage.Image{}, io.EOF
	}
	i := f.calls
	if i >= len(f.frames) {
		i = len(f.frames) - 1
	}
	f.calls++
	im := f.frames[i]
	im.Pixels = append([]byte(nil), im.Pixels...)
	return im, nil
}

type fakePeer struct {
	mu       sync.Mutex
	sent     []protocol.Message
	incoming chan protocol.Message
	sendErr  error
}

func (p *fakePeer) Send(_ context.Context, m protocol.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sendErr != nil {
		return p.sendErr
	}
	p.sent = append(p.sent, m)
	return nil
}
func (p *fakePeer) Receive(ctx context.Context) (protocol.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m, ok := <-p.incoming:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	}
}
func (p *fakePeer) messages() []protocol.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.Message(nil), p.sent...)
}

type inputCall struct {
	kind string
	a, b int
}
type fakeInput struct {
	mu       sync.Mutex
	calls    []inputCall
	err      error
	releases int
}

func (i *fakeInput) Key(_ context.Context, u uint16, a protocol.Action, m uint8) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, inputCall{"key", int(u), int(a)})
	return i.err
}
func (i *fakeInput) Move(_ context.Context, x, y uint32) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, inputCall{"move", int(x), int(y)})
	return i.err
}
func (i *fakeInput) Button(_ context.Context, b protocol.Button, a protocol.Action) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, inputCall{"button", int(b), int(a)})
	return i.err
}
func (i *fakeInput) Wheel(_ context.Context, h, v int16) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, inputCall{"wheel", int(h), int(v)})
	return i.err
}
func (i *fakeInput) ReleaseAll(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.releases++
	return nil
}
func (i *fakeInput) snapshot() ([]inputCall, int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]inputCall(nil), i.calls...), i.releases
}

func testImage(b byte) damage.Image {
	return damage.Image{Width: 4, Height: 4, Pixels: solidPixels(4, 4, b)}
}
func solidPixels(w, h int, b byte) []byte {
	p := make([]byte, w*h*4)
	for n := 0; n < len(p); n += 4 {
		p[n], p[n+1], p[n+2], p[n+3] = b, b, b, 0xff
	}
	return p
}
func testConfig() Config {
	return Config{DisplayID: 0, CaptureInterval: time.Millisecond, KeyframeInterval: time.Hour, MaxInputEventsPerSecond: 100, EnableInput: true}
}

func TestServiceSendsDisplayKeyframeAndNoFramesForUnchangedCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	capture := &fakeCapture{frames: []damage.Image{testImage(1)}}
	peer := &fakePeer{incoming: make(chan protocol.Message)}
	s := NewService(testConfig(), capture, &fakeInput{})
	if err := s.Run(ctx, peer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error=%v", err)
	}
	msgs := peer.messages()
	var displays, frames int
	for _, m := range msgs {
		switch m.(type) {
		case protocol.DisplayConfig:
			displays++
		case protocol.Frame:
			frames++
		}
	}
	if displays != 1 || frames != 1 {
		t.Fatalf("display=%d frames=%d messages=%#v", displays, frames, msgs)
	}
}

func TestServiceTranslatesInputAndCleansUpOnDisconnect(t *testing.T) {
	in := make(chan protocol.Message, 5)
	in <- protocol.Key{Generation: 1, InputSequence: 1, Usage: 0x04, Action: protocol.ActionDown}
	in <- protocol.PointerMove{Generation: 1, InputSequence: 2, X: 3, Y: 2}
	in <- protocol.PointerButton{Generation: 1, InputSequence: 3, Button: protocol.ButtonLeft, Action: protocol.ActionDown}
	in <- protocol.PointerWheel{Generation: 1, InputSequence: 4, Vertical: 120}
	close(in)
	input := &fakeInput{}
	err := NewService(testConfig(), &fakeCapture{frames: []damage.Image{testImage(1)}}, input).Run(context.Background(), &fakePeer{incoming: in})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run error=%v", err)
	}
	calls, releases := input.snapshot()
	if len(calls) != 4 || calls[0].kind != "key" || calls[1] != (inputCall{"move", 3, 2}) || calls[2].kind != "button" || calls[3] != (inputCall{"wheel", 0, 120}) {
		t.Fatalf("calls=%#v", calls)
	}
	if releases != 1 {
		t.Fatalf("ReleaseAll calls=%d", releases)
	}
}

func TestServiceFocusLostAndShutdownReleaseInput(t *testing.T) {
	in := make(chan protocol.Message, 1)
	in <- protocol.FocusLost{Generation: 1, InputSequence: 1}
	close(in)
	input := &fakeInput{}
	_ = NewService(testConfig(), &fakeCapture{frames: []damage.Image{testImage(1)}}, input).Run(context.Background(), &fakePeer{incoming: in})
	_, releases := input.snapshot()
	if releases != 2 {
		t.Fatalf("focus loss plus shutdown releases=%d", releases)
	}
}

func TestServiceRejectsStaleGenerationOutOfBoundsAndDisabledInput(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		message protocol.Message
	}{
		{"stale", testConfig(), protocol.PointerMove{Generation: 2, InputSequence: 1, X: 0, Y: 0}},
		{"bounds", testConfig(), protocol.PointerMove{Generation: 1, InputSequence: 1, X: 4, Y: 0}},
		{"disabled", func() Config { c := testConfig(); c.EnableInput = false; return c }(), protocol.Key{Generation: 1, InputSequence: 1, Usage: 4, Action: protocol.ActionDown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := make(chan protocol.Message, 1)
			in <- tc.message
			input := &fakeInput{}
			err := NewService(tc.cfg, &fakeCapture{frames: []damage.Image{testImage(1)}}, input).Run(context.Background(), &fakePeer{incoming: in})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error=%v", err)
			}
			calls, _ := input.snapshot()
			if len(calls) != 0 {
				t.Fatalf("injected %#v", calls)
			}
		})
	}
}

func TestServiceRateLimitsInput(t *testing.T) {
	in := make(chan protocol.Message, 2)
	in <- protocol.Key{Generation: 1, InputSequence: 1, Usage: 4, Action: protocol.ActionDown}
	in <- protocol.Key{Generation: 1, InputSequence: 2, Usage: 4, Action: protocol.ActionUp}
	cfg := testConfig()
	cfg.MaxInputEventsPerSecond = 1
	err := NewService(cfg, &fakeCapture{frames: []damage.Image{testImage(1)}}, &fakeInput{}).Run(context.Background(), &fakePeer{incoming: in})
	if !errors.Is(err, ErrInputRate) {
		t.Fatalf("error=%v", err)
	}
}

func TestServicePropagatesCaptureSendAndInjectionErrors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("capture", func(t *testing.T) {
		err := NewService(testConfig(), &fakeCapture{err: boom}, &fakeInput{}).Run(context.Background(), &fakePeer{incoming: make(chan protocol.Message)})
		if !errors.Is(err, boom) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("send", func(t *testing.T) {
		err := NewService(testConfig(), &fakeCapture{frames: []damage.Image{testImage(1)}}, &fakeInput{}).Run(context.Background(), &fakePeer{incoming: make(chan protocol.Message), sendErr: boom})
		if !errors.Is(err, boom) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("inject", func(t *testing.T) {
		ch := make(chan protocol.Message, 1)
		ch <- protocol.Key{Generation: 1, InputSequence: 1, Usage: 4, Action: protocol.ActionDown}
		err := NewService(testConfig(), &fakeCapture{frames: []damage.Image{testImage(1)}}, &fakeInput{err: boom}).Run(context.Background(), &fakePeer{incoming: ch})
		if !errors.Is(err, boom) {
			t.Fatalf("error=%v", err)
		}
	})
}
