package client

import (
	"errors"
	"reflect"
	"testing"

	"virtualdesktop/internal/protocol"
)

func TestMapCoordinatesPreservesAspectRatioAndRejectsLetterbox(t *testing.T) {
	tests := []struct {
		name         string
		x, y, vw, vh float64
		rw, rh       uint32
		wantX, wantY uint32
		ok           bool
	}{
		{"center", 500, 500, 1000, 1000, 1920, 1080, 960, 540, true},
		{"top letterbox", 500, 100, 1000, 1000, 1920, 1080, 0, 0, false},
		{"first pixel", 0, 218.75, 1000, 1000, 1920, 1080, 0, 0, true},
		{"last pixel clamped", 999.999, 781.249, 1000, 1000, 1920, 1080, 1919, 1079, true},
		{"nan", 0.0 / zero(), 0, 100, 100, 10, 10, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, y, ok := MapCoordinates(tc.x, tc.y, tc.vw, tc.vh, tc.rw, tc.rh)
			if x != tc.wantX || y != tc.wantY || ok != tc.ok {
				t.Fatalf("got (%d,%d,%v)", x, y, ok)
			}
		})
	}
}

func zero() float64 { return 0 }

func TestInputStateTransitionsAndFocusLoss(t *testing.T) {
	sender := &recordingSender{}
	state := NewInputState(sender)
	state.SetDisplay(7, 100, 50)
	state.SetFocused(true)
	if err := state.Key(0x04, protocol.ActionDown, 2); err != nil {
		t.Fatal(err)
	}
	if err := state.Key(0x04, protocol.ActionDown, 2); err != nil {
		t.Fatal(err)
	}
	if err := state.Button(protocol.ButtonLeft, protocol.ActionDown); err != nil {
		t.Fatal(err)
	}
	if err := state.Button(protocol.ButtonLeft, protocol.ActionDown); err != nil {
		t.Fatal(err)
	}
	if err := state.Pointer(50, 25, 100, 50); err != nil {
		t.Fatal(err)
	}
	if err := state.Wheel(0, 120); err != nil {
		t.Fatal(err)
	}
	if err := state.SetFocused(false); err != nil {
		t.Fatal(err)
	}

	wantTypes := []protocol.Type{protocol.TypeKey, protocol.TypePointerButton, protocol.TypePointerMove, protocol.TypePointerWheel, protocol.TypeKey, protocol.TypePointerButton, protocol.TypeFocusLost}
	var gotTypes []protocol.Type
	for _, m := range sender.messages {
		gotTypes = append(gotTypes, m.Type())
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("types got %v want %v", gotTypes, wantTypes)
	}
	for i, m := range sender.messages {
		seq := inputSequence(m)
		if seq != uint64(i+1) {
			t.Fatalf("message %d sequence=%d", i, seq)
		}
	}
}

func TestInputStateOnlyMutatesHeldStateAfterSuccessfulSend(t *testing.T) {
	sender := &recordingSender{fail: errors.New("full")}
	state := NewInputState(sender)
	state.SetDisplay(1, 10, 10)
	state.SetFocused(true)
	if err := state.Key(4, protocol.ActionDown, 0); err == nil {
		t.Fatal("expected error")
	}
	sender.fail = nil
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("got %d sent messages", len(sender.messages))
	}
}

func TestInputStateReconnectStartsWithNoInheritedHeldState(t *testing.T) {
	sender := &recordingSender{}
	state := NewInputState(sender)
	state.SetDisplay(1, 10, 10)
	state.SetFocused(true)
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if err := state.Disconnect(); err != nil {
		t.Fatal(err)
	}
	state.Connect()
	state.SetDisplay(2, 10, 10)
	if err := state.SetFocused(true); err != nil {
		t.Fatal(err)
	}
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if got := sender.messages[len(sender.messages)-1].(protocol.Key); got.Generation != 2 || got.Action != protocol.ActionDown {
		t.Fatalf("reconnected key = %#v", got)
	}
}

func TestInputStateConnectClearsInheritedHeldState(t *testing.T) {
	sender := &recordingSender{}
	state := NewInputState(sender)
	state.SetDisplay(1, 10, 10)
	state.SetFocused(true)
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if err := state.Button(protocol.ButtonLeft, protocol.ActionDown); err != nil {
		t.Fatal(err)
	}
	// A manual Connect (as a reconnect would trigger) must drop held state so
	// no stale key/button leaks across the boundary.
	state.Connect()
	state.SetDisplay(2, 10, 10)
	state.SetFocused(true)
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	// The first message after Connect must be the fresh DOWN, not a stray UP.
	last := sender.messages[len(sender.messages)-1].(protocol.Key)
	if last.Generation != 2 || last.Action != protocol.ActionDown || last.Usage != 4 {
		t.Fatalf("unexpected post-connect key = %#v", last)
	}
	for _, m := range sender.messages {
		if k, ok := m.(protocol.Key); ok && k.Action == protocol.ActionUp {
			t.Fatalf("Connect leaked a stray key up: %#v", k)
		}
		if b, ok := m.(protocol.PointerButton); ok && b.Action == protocol.ActionUp {
			t.Fatalf("Connect leaked a stray button up: %#v", b)
		}
	}
}

func TestInputStateSetSenderDeliversThroughNewTarget(t *testing.T) {
	first := &recordingSender{}
	state := NewInputState(first)
	state.SetDisplay(1, 10, 10)
	state.SetFocused(true)
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if len(first.messages) != 1 {
		t.Fatalf("expected delivery through initial sender, got %d", len(first.messages))
	}
	second := &recordingSender{}
	state.SetSender(second)
	if err := state.Key(5, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if len(second.messages) != 1 || len(first.messages) != 1 {
		t.Fatalf("sender swap did not redirect delivery: first=%d second=%d", len(first.messages), len(second.messages))
	}
}

func TestInputStateRejectsInvalidEventsAndDisconnectsCleanly(t *testing.T) {
	sender := &recordingSender{}
	state := NewInputState(sender)
	state.SetDisplay(1, 10, 10)
	state.SetFocused(true)
	if err := state.Key(1, protocol.ActionDown, 0); err == nil {
		t.Fatal("accepted reserved HID usage")
	}
	if err := state.Button(protocol.Button(99), protocol.ActionDown); err == nil {
		t.Fatal("accepted unknown button")
	}
	if err := state.Wheel(0, 1); err == nil {
		t.Fatal("accepted fractional wheel wire value")
	}
	if err := state.Disconnect(); err != nil {
		t.Fatal(err)
	}
	before := len(sender.messages)
	if err := state.Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != before {
		t.Fatal("sent input after disconnect")
	}
}

type recordingSender struct {
	messages []protocol.Message
	fail     error
}

func (r *recordingSender) Send(m protocol.Message) error {
	if r.fail != nil {
		return r.fail
	}
	r.messages = append(r.messages, m)
	return nil
}

func inputSequence(m protocol.Message) uint64 {
	switch v := m.(type) {
	case protocol.Key:
		return v.InputSequence
	case protocol.PointerMove:
		return v.InputSequence
	case protocol.PointerButton:
		return v.InputSequence
	case protocol.PointerWheel:
		return v.InputSequence
	case protocol.FocusLost:
		return v.InputSequence
	default:
		return 0
	}
}
