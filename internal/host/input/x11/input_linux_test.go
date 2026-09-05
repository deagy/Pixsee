//go:build linux

package x11

import (
	"testing"
	"virtualdesktop/internal/protocol"
)

func TestHIDUsageToX11Keycode(t *testing.T) {
	cases := map[uint16]byte{
		0x04: 38,  // A
		0x1d: 52,  // Z
		0x1e: 10,  // 1
		0x27: 19,  // 0
		0x28: 36,  // Enter
		0x29: 9,   // Escape
		0x2c: 65,  // Space
		0x4f: 114, // Right arrow
		0x50: 113, // Left arrow
		0xe0: 37,  // Left control
		0xe7: 134, // Right GUI
	}
	for usage, want := range cases {
		got, ok := keycodeForUsage(usage)
		if !ok || got != want {
			t.Errorf("usage %#x => (%d,%v), want (%d,true)", usage, got, ok, want)
		}
	}
	if _, ok := keycodeForUsage(0xa4); ok {
		t.Fatal("unmapped usage appeared supported")
	}
}

func TestPointerButtonMapping(t *testing.T) {
	cases := map[protocol.Button]byte{
		protocol.ButtonLeft:    1,
		protocol.ButtonMiddle:  2,
		protocol.ButtonRight:   3,
		protocol.ButtonBack:    8,
		protocol.ButtonForward: 9,
	}
	for button, want := range cases {
		if got := buttonNumber(button); got != want {
			t.Errorf("button %d => %d, want %d", button, got, want)
		}
	}
}

func TestWheelClicksRejectsExcessiveMagnitude(t *testing.T) {
	if _, err := wheelClicks(0, 120); err != nil {
		t.Fatal(err)
	}
	if clicks, _ := wheelClicks(-240, 120); len(clicks) != 3 {
		t.Fatalf("clicks=%v", clicks)
	}
	if _, err := wheelClicks(0, 120*21); err == nil {
		t.Fatal("accepted excessive wheel event")
	}
}
