//go:build linux

package input

import "testing"

func TestLinuxKeycodeForUsage(t *testing.T) {
	cases := map[uint16]byte{
		0x04: 38,  // A
		0x1d: 52,  // Z
		0x1e: 10,  // 1
		0x27: 19,  // 0
		0x28: 36,  // Enter
		0x29: 9,   // Escape
		0x2c: 65,  // Space
		0x4f: 114, // Right
		0x50: 113, // Left
		0xe0: 37,  // LeftControl
		0xe7: 134, // LeftGUI
	}
	for usage, want := range cases {
		if got, ok := keycodeForUsage(usage); !ok || got != want {
			t.Errorf("usage %#x: got (%d,%v), want (%d,true)", usage, got, ok, want)
		}
	}
}

func TestLinuxKeycodeRejectsReserved(t *testing.T) {
	if _, ok := keycodeForUsage(0x48); ok {
		t.Fatal("expected unmapped usage 0x48 to be rejected")
	}
}
