//go:build darwin && cgo

// Package input injects keyboard and pointer events on the host. This file is
// the darwin adapter: it sends keyboard and pointer events through CoreGraphics
// low-level event APIs (CGEventKeyboardKeyDown/KeyUp, CGEventCreateMouseEvent,
// and CGEventCreateScrollWheelEvent). It requires cgo. When cgo is disabled on
// darwin, input_stub_darwin.go provides a stub so the package still compiles.
// The package is platform neutral; input_linux.go and input_windows.go provide
// the other adapters behind the same method set.
package input

/*
#cgo CFLAGS: -framework CoreGraphics
#include <CoreGraphics/CoreGraphics.h>
#include <stdlib.h>

// CGEvent enums are not exposed as Go constants by cgo, so re-expose the few
// ones we use as integer macros that cgo can read.
#define VD_HID_SYSTEM_STATE 2
#define VD_TAP_LOCATION_SESSION 0
#define VD_MOUSE_LEFT_DOWN 1
#define VD_MOUSE_LEFT_UP 2
#define VD_MOUSE_RIGHT_DOWN 3
#define VD_MOUSE_RIGHT_UP 4
#define VD_MOUSE_MIDDLE_DOWN 5
#define VD_MOUSE_MIDDLE_UP 6
#define VD_MOUSE_OTHER_DOWN 7
#define VD_MOUSE_OTHER_UP 8
*/
import "C"

import (
	"context"
	"errors"
	"sync"

	"virtualdesktop/internal/protocol"
)

// keymap maps a USB HID keyboard-page usage ID to a macOS CGKeyCode for a US
// QWERTY layout. The CGKeyCode table is not contiguous with the HID table, so
// every key is listed explicitly.
var keymap = map[uint16]C.CGKeyCode{
	0x04: 0x00, // A
	0x05: 0x0B, // B
	0x06: 0x08, // C
	0x07: 0x05, // D
	0x08: 0x0E, // E
	0x09: 0x04, // F
	0x0a: 0x03, // G
	0x0b: 0x01, // H
	0x0c: 0x02, // I
	0x0d: 0x26, // J
	0x0e: 0x28, // K
	0x0f: 0x25, // L
	0x10: 0x2E, // M
	0x11: 0x2D, // N
	0x12: 0x1D, // O
	0x13: 0x1F, // P
	0x14: 0x0C, // Q
	0x15: 0x0D, // R
	0x16: 0x10, // S
	0x17: 0x11, // T
	0x18: 0x20, // U
	0x19: 0x09, // V
	0x1a: 0x0F, // W
	0x1b: 0x07, // X
	0x1c: 0x1A, // Y
	0x1d: 0x06, // Z
	0x1e: 0x12, // 1
	0x1f: 0x13, // 2
	0x20: 0x14, // 3
	0x21: 0x15, // 4
	0x22: 0x17, // 5
	0x23: 0x16, // 6
	0x24: 0x1A, // 7
	0x25: 0x1C, // 8
	0x26: 0x19, // 9
	0x27: 0x1D, // 0
	0x28: 0x24, // Return
	0x29: 0x35, // Escape
	0x2a: 0x33, // Backspace
	0x2b: 0x30, // Tab
	0x2c: 0x31, // Space
	0x2d: 0x1B, // Minus
	0x2e: 0x18, // Equal
	0x2f: 0x21, // LeftBracket
	0x30: 0x1E, // RightBracket
	0x31: 0x2A, // Backslash
	0x33: 0x29, // Semicolon
	0x34: 0x27, // Quote
	0x35: 0x32, // Grave
	0x36: 0x2B, // Comma
	0x37: 0x2F, // Period
	0x38: 0x2C, // Slash
	0x39: 0x39, // CapsLock
	0x3a: 0x7A, // F1
	0x3b: 0x78, // F2
	0x3c: 0x63, // F3
	0x3d: 0x76, // F4
	0x3e: 0x77, // F5
	0x3f: 0x61, // F6
	0x40: 0x71, // F7
	0x41: 0x74, // F8
	0x42: 0x64, // F9
	0x43: 0x61, // F10
	0x44: 0x67, // F11
	0x45: 0x6F, // F12
	0x49: 0x75, // Insert (maps to ForwardDelete on ANSI)
	0x4a: 0x73, // Home
	0x4b: 0x74, // PageUp
	0x4c: 0x72, // Delete
	0x4d: 0x77, // End
	0x4e: 0x79, // PageDown
	0x4f: 0x7C, // Right
	0x50: 0x7B, // Left
	0x51: 0x7D, // Down
	0x52: 0x7E, // Up
	0xe0: 0x3B, // LeftControl
	0xe1: 0x38, // LeftShift
	0xe2: 0x3A, // LeftAlt
	0xe3: 0x37, // LeftGUI
	0xe4: 0x3E, // RightControl
	0xe5: 0x3C, // RightShift
	0xe6: 0x3D, // RightAlt
	0xe7: 0x36, // RightGUI
}

// Injector sends keyboard and pointer events through CoreGraphics.
type Injector struct {
	mu             sync.Mutex
	pressedKeys    map[uint16]struct{}
	pressedButtons map[byte]struct{}
}

// New returns an injector ready to send events.
func New() (*Injector, error) {
	return &Injector{
		pressedKeys:    make(map[uint16]struct{}),
		pressedButtons: make(map[byte]struct{}),
	}, nil
}

// Close releases any held input.
func (i *Injector) Close() {}

// Key sends a keyboard event, keeping modifier keys synced with the supplied
// modifier bitmap (HID order: Shift=bit0, Ctrl=bit1, Alt=bit2, GUI=bit3).
func (i *Injector) Key(ctx context.Context, usage uint16, action protocol.Action, modifiers uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	code, ok := keymap[usage]
	if !ok {
		return errors.New("unsupported HID keyboard usage")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.syncModifiers(modifiers); err != nil {
		return err
	}
	return i.emitKey(usage, code, action)
}

// Move sends an absolute pointer move in host framebuffer pixels.
func (i *Injector) Move(ctx context.Context, x, y uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.emitMouse(C.VD_MOUSE_LEFT_UP, x, y, 0)
}

// Button sends a pointer button press or release.
func (i *Injector) Button(ctx context.Context, button protocol.Button, action protocol.Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	flag, ok := buttonFlag(button)
	if !ok {
		return errors.New("unsupported pointer button")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if action == protocol.ActionDown {
		i.pressedButtons[buttonNumber(button)] = struct{}{}
	} else {
		delete(i.pressedButtons, buttonNumber(button))
	}
	return i.emitMouse(flag, 0, 0, 0)
}

// Wheel sends a vertical or horizontal wheel delta.
func (i *Injector) Wheel(ctx context.Context, horizontal, vertical int16) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if vertical != 0 {
		return i.sendWheel(C.int16_t(vertical))
	}
	if horizontal != 0 {
		return i.sendWheel(C.int16_t(horizontal))
	}
	return nil
}

// ReleaseAll sends release events for every held key and button.
func (i *Injector) ReleaseAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	var first error
	for usage := range i.pressedKeys {
		code, ok := keymap[usage]
		if !ok {
			continue
		}
		if err := i.emitKey(code, protocol.ActionUp); err != nil && first == nil {
			first = err
		}
		delete(i.pressedKeys, usage)
	}
	for n := range i.pressedButtons {
		flag, ok := buttonFlag(protocol.Button(n))
		if !ok {
			continue
		}
		if err := i.emitMouse(flag, 0, 0, 0); err != nil && first == nil {
			first = err
		}
		delete(i.pressedButtons, n)
	}
	return first
}

// emitKey posts a keyboard key down/up event, tracking the usage so ReleaseAll
// can release the correct key when the session ends.
func (i *Injector) emitKey(usage uint16, code C.CGKeyCode, action protocol.Action) error {
	source := C.CGEventSourceCreate(C.CGEventSourceStateID(C.VD_HID_SYSTEM_STATE))
	if source == nil {
		return errors.New("failed to create CGEventSource")
	}
	defer C.CGEventRelease(source)
	if action == protocol.ActionDown {
		i.pressedKeys[usage] = struct{}{}
	} else {
		delete(i.pressedKeys, usage)
	}
	if action == protocol.ActionDown {
		C.CGEventKeyboardKeyDown(source, code)
	} else {
		C.CGEventKeyboardKeyUp(source, code)
	}
	return nil
}

// emitMouse posts a mouse event at position (x, y) with the given button state.
func (i *Injector) emitMouse(flag C.CGEventType, x, y, button uint32) error {
	source := C.CGEventSourceCreate(C.CGEventSourceStateID(C.VD_HID_SYSTEM_STATE))
	if source == nil {
		return errors.New("failed to create CGEventSource")
	}
	defer C.CGEventRelease(source)
	pos := C.CGPoint{x: C.CGFloat(x), y: C.CGFloat(y)}
	event := C.CGEventCreateMouseEvent(source, flag, pos, C.CGMouseButton(button))
	if event == nil {
		return errors.New("failed to create mouse event")
	}
	C.CGEventPost(C.CGEventPort(C.VD_TAP_LOCATION_SESSION), event)
	C.CGEventRelease(event)
	return nil
}

func (i *Injector) sendWheel(delta C.int16_t) error {
	source := C.CGEventSourceCreate(C.CGEventSourceStateID(C.VD_HID_SYSTEM_STATE))
	if source == nil {
		return errors.New("failed to create CGEventSource")
	}
	defer C.CGEventRelease(source)
	event := C.CGEventCreateScrollWheelEvent(source, C.CGScrollWheelEventUnitLine, 1, delta)
	if event == nil {
		return errors.New("failed to create scroll wheel event")
	}
	C.CGEventPost(C.CGEventPort(C.VD_TAP_LOCATION_SESSION), event)
	C.CGEventRelease(event)
	return nil
}

// syncModifiers adjusts held modifier keys to match the protocol bitmap.
func (i *Injector) syncModifiers(bits uint8) error {
	for bit := uint8(0); bit < 4; bit++ {
		want := bits&(1<<bit) != 0
		var usage uint16
		switch bit {
		case 0:
			usage = 0xe1 // LeftShift
		case 1:
			usage = 0xe0 // LeftControl
		case 2:
			usage = 0xe2 // LeftAlt
		case 3:
			usage = 0xe3 // LeftGUI
		}
		_, down := i.pressedKeys[usage]
		switch {
		case want && !down:
			if code, ok := keymap[usage]; ok {
				i.emitKey(code, protocol.ActionDown)
			}
		case !want && down:
			if code, ok := keymap[usage]; ok {
				i.emitKey(code, protocol.ActionUp)
			}
		}
	}
	return nil
}

// buttonFlag maps a protocol button to a CoreGraphics mouse event type.
func buttonFlag(button protocol.Button) (C.CGEventType, bool) {
	switch button {
	case protocol.ButtonLeft:
		return C.CGEventType(C.VD_MOUSE_LEFT_DOWN), true
	case protocol.ButtonRight:
		return C.CGEventType(C.VD_MOUSE_RIGHT_DOWN), true
	case protocol.ButtonMiddle:
		return C.CGEventType(C.VD_MOUSE_MIDDLE_DOWN), true
	case protocol.ButtonBack:
		return C.CGEventType(C.VD_MOUSE_OTHER_DOWN), true
	case protocol.ButtonForward:
		return C.CGEventType(C.VD_MOUSE_OTHER_DOWN), true
	}
	return 0, false
}
