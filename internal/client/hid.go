package client

import (
	"strconv"
	"strings"
)

// HIDUsage maps toolkit key names to USB HID keyboard-page usages. Unknown
// names are ignored; platform-native keycodes and text are never forwarded.
func HIDUsage(name string) (uint16, bool) {
	if len(name) == 1 {
		c := name[0]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c >= 'A' && c <= 'Z' {
			return uint16(0x04 + c - 'A'), true
		}
		if c >= '1' && c <= '9' {
			return uint16(0x1e + c - '1'), true
		}
		if c == '0' {
			return 0x27, true
		}
	}
	if strings.HasPrefix(name, "F") {
		n, err := strconv.Atoi(strings.TrimPrefix(name, "F"))
		if err == nil && n >= 1 && n <= 12 {
			return uint16(0x3a + n - 1), true
		}
	}
	usage, ok := namedHIDUsages[name]
	return usage, ok
}

// Kept as a variable to make key mapping independent from native toolkit APIs.
var namedHIDUsages = map[string]uint16{
	"Enter": 0x28, "Return": 0x28, "Escape": 0x29, "Backspace": 0x2a,
	"Tab": 0x2b, "Space": 0x2c, "Minus": 0x2d, "Equal": 0x2e,
	"LeftBracket": 0x2f, "RightBracket": 0x30, "Backslash": 0x31,
	"Semicolon": 0x33, "Apostrophe": 0x34, "Grave": 0x35,
	"Comma": 0x36, "Period": 0x37, "Slash": 0x38, "CapsLock": 0x39,
	"PrintScreen": 0x46, "ScrollLock": 0x47, "Pause": 0x48, "Insert": 0x49,
	"Home": 0x4a, "PageUp": 0x4b, "Delete": 0x4c, "End": 0x4d,
	"PageDown": 0x4e, "Right": 0x4f, "Left": 0x50, "Down": 0x51, "Up": 0x52,
	"NumLock": 0x53, "LeftControl": 0xe0, "LeftShift": 0xe1, "LeftAlt": 0xe2,
	"LeftSuper": 0xe3, "RightControl": 0xe4, "RightShift": 0xe5,
	"RightAlt": 0xe6, "RightSuper": 0xe7,
}
