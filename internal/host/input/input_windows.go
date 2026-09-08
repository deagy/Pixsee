//go:build windows

// Package input injects keyboard and pointer events on the host. This file is
// the windows adapter: it sends keyboard and pointer events through SendInput,
// which injects into the active input stream without requiring a foreground
// window. The package is platform neutral; input_linux.go and input_darwin.go
// provide the other adapters behind the same method set.
package input

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"virtualdesktop/internal/protocol"
)

var (
	user32        = syscall.NewLazyDLL("user32.dll")
	sendInputProc = user32.NewProc("SendInput")
)

// SendInput event types.
const (
	INPUT_MOUSE    = 0
	INPUT_KEYBOARD = 1
)

// Keyboard event flags.
const (
	KEYEVENTF_KEYUP    = 0x0002
	KEYEVENTF_EXTENDED = 0x0001
)

// Mouse event flags.
const (
	MOUSEEVENTF_MOVE       = 0x0001
	MOUSEEVENTF_LEFTDOWN   = 0x0002
	MOUSEEVENTF_LEFTUP     = 0x0004
	MOUSEEVENTF_RIGHTDOWN  = 0x0008
	MOUSEEVENTF_RIGHTUP    = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP   = 0x0040
	MOUSEEVENTF_WHEEL      = 0x0800
	MOUSEEVENTF_XDOWN      = 0x0080
	MOUSEEVENTF_XUP        = 0x0100
)

// input mirrors the C INPUT structure. The anonymous union is sized for
// MOUSEINPUT (the largest member, 28 bytes), so the total is 32 bytes. The
// KEYBDINPUT fields overlap the low bytes of the union; KEYBDINPUT.dwFlags sits
// at union offset 4 (the Dy field) and KEYBDINPUT.time at offset 8 (the
// MouseData field). KEYBDINPUT.dwExtraInfo overlaps union offset 12 (DwFlags +
// Time); it is normally zero, so the two low fields stay available for the
// keyboard flags and time.
type input struct {
	Type        uint32
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// keyboard builds a KEYBDINPUT event. vk and scan fill the low 32 bits of the
// union (Dx); flags fill KEYBDINPUT.dwFlags (Dy); time fills KEYBDINPUT.time
// (MouseData). dwExtraInfo is left zero.
func keyboard(vk, scan, flags, time uint16) input {
	var in input
	in.Type = INPUT_KEYBOARD
	in.Dx = int32(uint32(scan)<<16 | uint32(vk))
	in.Dy = int32(flags)
	in.MouseData = uint32(time)
	in.DwFlags = 0
	in.Time = 0
	in.DwExtraInfo = 0
	return in
}

func mouse(dx, dy, mouseData, flags uint32) input {
	var in input
	in.Type = INPUT_MOUSE
	in.Dx = int32(dx)
	in.Dy = int32(dy)
	in.MouseData = mouseData
	in.DwFlags = flags
	in.Time = 0
	in.DwExtraInfo = 0
	return in
}

func sendInput(inputs []input) error {
	if len(inputs) == 0 {
		return nil
	}
	r, _, _ := sendInputProc.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		uintptr(unsafe.Sizeof(input{})),
	)
	if r == 0 {
		return errors.New("SendInput failed")
	}
	return nil
}

// Injector sends keyboard and pointer events through SendInput.
type Injector struct {
	mu             sync.Mutex
	pressedKeys    map[uint16]struct{}
	pressedButtons map[byte]struct{}
}

// New returns an injector ready to send events. Windows needs no connection, so
// this always succeeds.
func New() (*Injector, error) {
	if runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("Windows input injection is unsupported on windows/%s; MVP requires windows/amd64", runtime.GOARCH)
	}
	return &Injector{
		pressedKeys:    make(map[uint16]struct{}),
		pressedButtons: make(map[byte]struct{}),
	}, nil
}

// Close releases any held input.
func (i *Injector) Close() {
	// SendInput is stateless; nothing to close.
}

// Key sends a keyboard event, keeping modifier keys synced with the supplied
// modifier bitmap (HID order: Shift=bit0, Ctrl=bit1, Alt=bit2, GUI=bit3).
func (i *Injector) Key(ctx context.Context, usage uint16, action protocol.Action, modifiers uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	vk, ok := hidToVK(usage)
	if !ok {
		return fmt.Errorf("unsupported HID keyboard usage %#x", usage)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.syncModifiers(modifiers); err != nil {
		return err
	}
	return i.key(vk, action)
}

// Move sends an absolute pointer move in host framebuffer pixels.
func (i *Injector) Move(ctx context.Context, x, y uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return sendInput([]input{mouse(x, y, 0, MOUSEEVENTF_MOVE)})
}

// Button sends a pointer button press or release.
func (i *Injector) Button(ctx context.Context, button protocol.Button, action protocol.Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	flag := buttonAction(button, action)
	if flag == 0 {
		return errors.New("unsupported pointer button")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if action == protocol.ActionDown {
		i.pressedButtons[buttonNumber(button)] = struct{}{}
	} else {
		delete(i.pressedButtons, buttonNumber(button))
	}
	return sendInput([]input{mouse(0, 0, 0, flag)})
}

// Wheel replays a wheel delta as MOUSEEVENTF_WHEEL and XBUTTON events.
func (i *Injector) Wheel(ctx context.Context, horizontal, vertical int16) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clicks, err := wheelClicks(horizontal, vertical)
	if err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, n := range clicks {
		var delta int16
		switch n {
		case 4:
			delta = 120
		case 5:
			delta = -120
		case 6:
			delta = 120
		case 7:
			delta = -120
		}
		if n == 6 || n == 7 {
			if err := sendInput([]input{mouse(0, 0, uint32(int32(delta)), MOUSEEVENTF_XDOWN|MOUSEEVENTF_XUP)}); err != nil {
				return err
			}
			continue
		}
		if err := sendInput([]input{mouse(0, 0, uint32(int32(delta)), MOUSEEVENTF_WHEEL)}); err != nil {
			return err
		}
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
	for vk := range i.pressedKeys {
		if err := i.key(vk, protocol.ActionUp); err != nil && first == nil {
			first = err
		}
		delete(i.pressedKeys, vk)
	}
	order := make([]byte, 0, len(i.pressedButtons))
	for n := range i.pressedButtons {
		order = append(order, n)
	}
	// Deterministic order keeps event logs stable across runs.
	for a := 0; a < len(order); a++ {
		for b := a + 1; b < len(order); b++ {
			if order[b] < order[a] {
				order[a], order[b] = order[b], order[a]
			}
		}
	}
	for _, n := range order {
		flag := uint32(0)
		switch n {
		case 1:
			flag = MOUSEEVENTF_LEFTUP
		case 2:
			flag = MOUSEEVENTF_MIDDLEUP
		case 3:
			flag = MOUSEEVENTF_RIGHTUP
		case 8:
			flag = MOUSEEVENTF_XUP
		case 9:
			flag = MOUSEEVENTF_XUP
		}
		if err := sendInput([]input{mouse(0, 0, 0, flag)}); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (i *Injector) key(vk uint16, action protocol.Action) error {
	if action == protocol.ActionUp {
		if _, ok := i.pressedKeys[vk]; !ok {
			return nil
		}
	}
	var in input
	in = keyboard(vk, 0, 0, 0)
	if action == protocol.ActionUp {
		in.Dy |= int32(KEYEVENTF_KEYUP)
	}
	if action == protocol.ActionDown {
		i.pressedKeys[vk] = struct{}{}
	} else {
		delete(i.pressedKeys, vk)
	}
	return sendInput([]input{in})
}

// modifierVK maps a protocol modifier bit to its left virtual key.
func modifierVK(bit uint8) uint16 {
	switch bit {
	case 0:
		return 0xA0 // VK_LSHIFT
	case 1:
		return 0xA2 // VK_LCONTROL
	case 2:
		return 0xA4 // VK_LMENU
	case 3:
		return 0x5B // VK_LWIN
	}
	return 0
}

// syncModifiers adjusts held modifier keys to match the protocol bitmap.
func (i *Injector) syncModifiers(bits uint8) error {
	for bit := uint8(0); bit < 4; bit++ {
		want := bits&(1<<bit) != 0
		vk := modifierVK(bit)
		_, down := i.pressedKeys[vk]
		switch {
		case want && !down:
			if err := i.key(vk, protocol.ActionDown); err != nil {
				return err
			}
		case !want && down:
			if err := i.key(vk, protocol.ActionUp); err != nil {
				return err
			}
		}
	}
	return nil
}

// buttonAction maps a protocol button and action to a mouse event flag.
func buttonAction(button protocol.Button, action protocol.Action) uint32 {
	if action == protocol.ActionDown {
		switch button {
		case protocol.ButtonLeft:
			return MOUSEEVENTF_LEFTDOWN
		case protocol.ButtonRight:
			return MOUSEEVENTF_RIGHTDOWN
		case protocol.ButtonMiddle:
			return MOUSEEVENTF_MIDDLEDOWN
		case protocol.ButtonBack:
			return MOUSEEVENTF_XDOWN
		case protocol.ButtonForward:
			return MOUSEEVENTF_XDOWN
		}
	} else {
		switch button {
		case protocol.ButtonLeft:
			return MOUSEEVENTF_LEFTUP
		case protocol.ButtonRight:
			return MOUSEEVENTF_RIGHTUP
		case protocol.ButtonMiddle:
			return MOUSEEVENTF_MIDDLEUP
		case protocol.ButtonBack:
			return MOUSEEVENTF_XUP
		case protocol.ButtonForward:
			return MOUSEEVENTF_XUP
		}
	}
	return 0
}

// hidToVK maps a USB HID keyboard-page usage ID to a Windows virtual key code,
// using the standard US layout. Letters and digits are linear offsets from
// their VK equivalents; the remaining keys are a lookup table derived from the
// Windows scan-code table.
func hidToVK(usage uint16) (uint16, bool) {
	switch {
	case usage >= 0x04 && usage <= 0x1d:
		return uint16(0x41) + usage - 0x04, true // A-Z
	case usage >= 0x1e && usage <= 0x26:
		return uint16(0x31) + usage - 0x1e, true // 1-9
	case usage == 0x27:
		return 0x30, true // 0
	case usage == 0x28:
		return 0x0D, true // Enter
	case usage == 0x29:
		return 0x1B, true // Escape
	case usage == 0x2a:
		return 0x08, true // Backspace
	case usage == 0x2b:
		return 0x09, true // Tab
	case usage == 0x2c:
		return 0x20, true // Space
	case usage == 0x2d:
		return 0xBD, true // Minus / _
	case usage == 0x2e:
		return 0xBB, true // Equal / +
	case usage == 0x2f:
		return 0xDB, true // [ / {
	case usage == 0x30:
		return 0xDD, true // ] / }
	case usage == 0x31:
		return 0xDC, true // \ |
	case usage == 0x33:
		return 0xBA, true // ; :
	case usage == 0x34:
		return 0xDE, true // ' "
	case usage == 0x35:
		return 0xC0, true // ` ~
	case usage == 0x36:
		return 0xBC, true // , <
	case usage == 0x37:
		return 0xBE, true // . >
	case usage == 0x38:
		return 0xBF, true // / ?
	case usage == 0x39:
		return 0x14, true // CapsLock
	case usage >= 0x3a && usage <= 0x45:
		return uint16(0x70) + usage - 0x3a, true // F1-F12
	case usage == 0x49:
		return 0x2D, true // Insert
	case usage == 0x4a:
		return 0x24, true // Home
	case usage == 0x4b:
		return 0x21, true // PageUp
	case usage == 0x4c:
		return 0x2E, true // Delete
	case usage == 0x4d:
		return 0x23, true // End
	case usage == 0x4e:
		return 0x22, true // PageDown
	case usage == 0x4f:
		return 0x27, true // Right
	case usage == 0x50:
		return 0x25, true // Left
	case usage == 0x51:
		return 0x28, true // Down
	case usage == 0x52:
		return 0x26, true // Up
	case usage == 0xe0:
		return 0xA2, true // LeftControl
	case usage == 0xe1:
		return 0xA0, true // LeftShift
	case usage == 0xe2:
		return 0xA4, true // LeftAlt
	case usage == 0xe3:
		return 0x5B, true // LeftGUI
	case usage == 0xe4:
		return 0xA3, true // RightControl
	case usage == 0xe5:
		return 0xA1, true // RightShift
	case usage == 0xe6:
		return 0xA6, true // RightAlt
	case usage == 0xe7:
		return 0x5C, true // RightGUI
	}
	return 0, false
}
