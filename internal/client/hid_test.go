package client

import "testing"

func TestHIDUsageMapsToolkitNamesWithoutPlatformKeycodes(t *testing.T) {
	tests := map[string]uint16{
		"A": 0x04, "z": 0x1d, "1": 0x1e, "0": 0x27,
		"Enter": 0x28, "Escape": 0x29, "Space": 0x2c,
		"F12": 0x45, "Right": 0x4f, "LeftShift": 0xe1,
	}
	for name, want := range tests {
		got, ok := HIDUsage(name)
		if !ok || got != want {
			t.Fatalf("%q: got %#x, %v; want %#x", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "é", "XF86AudioPlay", "NativeKeyCode42"} {
		if usage, ok := HIDUsage(name); ok {
			t.Fatalf("accepted %q as %#x", name, usage)
		}
	}
}
