package input

import (
	"testing"

	"virtualdesktop/internal/protocol"
)

func TestWheelClicksMapsDeltaToButtonSequence(t *testing.T) {
	// Vertical up (positive) is button 4, down (negative) is 5.
	if got, err := wheelClicks(0, 120); err != nil || len(got) != 1 || got[0] != 4 {
		t.Fatalf("vertical up: got %v err %v", got, err)
	}
	if got, err := wheelClicks(0, -120); err != nil || len(got) != 1 || got[0] != 5 {
		t.Fatalf("vertical down: got %v err %v", got, err)
	}
	// Horizontal right (positive) is button 7, left (negative) is 6.
	if got, err := wheelClicks(120, 0); err != nil || len(got) != 1 || got[0] != 7 {
		t.Fatalf("horizontal right: got %v err %v", got, err)
	}
	if got, err := wheelClicks(-120, 0); err != nil || len(got) != 1 || got[0] != 6 {
		t.Fatalf("horizontal left: got %v err %v", got, err)
	}
	// Multiple detents accumulate in order.
	if got, err := wheelClicks(0, 240); err != nil || len(got) != 2 || got[0] != 4 || got[1] != 4 {
		t.Fatalf("two up: got %v err %v", got, err)
	}
}

func TestWheelClicksRejectsNonMultipleOf120(t *testing.T) {
	if _, err := wheelClicks(10, 0); err == nil {
		t.Fatal("expected error for non-120 multiple")
	}
	if _, err := wheelClicks(0, 119); err == nil {
		t.Fatal("expected error for non-120 multiple")
	}
}

func TestWheelClicksRejectsExcessiveMagnitude(t *testing.T) {
	if _, err := wheelClicks(0, 120*21); err == nil {
		t.Fatal("expected error for excessive magnitude")
	}
}

func TestButtonNumberMapsProtocolButtons(t *testing.T) {
	cases := map[protocol.Button]byte{
		protocol.ButtonLeft:    1,
		protocol.ButtonMiddle:  2,
		protocol.ButtonRight:   3,
		protocol.ButtonBack:    8,
		protocol.ButtonForward: 9,
	}
	for in, want := range cases {
		if got := buttonNumber(in); got != want {
			t.Errorf("buttonNumber(%d) = %d, want %d", in, got, want)
		}
	}
}
