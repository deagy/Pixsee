//go:build linux

// Package input injects keyboard and pointer events on the host. This file is
// the linux adapter: it sends keyboard and pointer events through X11's XTEST
// extension. The package is platform neutral; the other files in this package
// provide the windows and darwin adapters behind the same method set.
package input

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"virtualdesktop/internal/protocol"
)

// Injector sends the restricted keyboard and pointer event set through XTEST.
type Injector struct {
	conn           *xgb.Conn
	root           xproto.Window
	mu             sync.Mutex
	pressedKeys    map[byte]struct{}
	pressedButtons map[byte]struct{}
}

// New opens a connection to the X server and returns an injector ready to send
// events. It fails if the X server cannot be reached or has no root screen.
func New() (*Injector, error) {
	if runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("X11 input injection is unsupported on linux/%s; MVP requires linux/amd64", runtime.GOARCH)
	}
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("connect to X11 for input: %w", err)
	}
	if err := xtest.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("initialize XTEST: %w", err)
	}
	setup := xproto.Setup(conn)
	if setup == nil || len(setup.Roots) == 0 {
		conn.Close()
		return nil, errors.New("X11 server has no root screen")
	}
	return &Injector{
		conn:           conn,
		root:           setup.DefaultScreen(conn).Root,
		pressedKeys:    make(map[byte]struct{}),
		pressedButtons: make(map[byte]struct{}),
	}, nil
}

// Close releases the X11 connection.
func (i *Injector) Close() {
	if i != nil && i.conn != nil {
		i.conn.Close()
	}
}

// Key maps a USB HID keyboard usage to an X11 keycode and injects it, keeping
// modifier keys in sync with the supplied modifier bitmap.
func (i *Injector) Key(ctx context.Context, usage uint16, action protocol.Action, modifiers uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	code, ok := keycodeForUsage(usage)
	if !ok {
		return fmt.Errorf("unsupported HID keyboard usage %#x", usage)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.syncModifiers(modifiers, code); err != nil {
		return err
	}
	return i.key(code, action)
}

// Move sends an absolute pointer motion event in host framebuffer pixels.
func (i *Injector) Move(ctx context.Context, x, y uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x > 32767 || y > 32767 {
		return errors.New("X11 pointer coordinate exceeds int16")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return xtest.FakeInputChecked(i.conn, xproto.MotionNotify, 0, 0, i.root, int16(x), int16(y), 0).Check()
}

// Button injects a pointer button press or release.
func (i *Injector) Button(ctx context.Context, button protocol.Button, action protocol.Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n := buttonNumber(button)
	if n == 0 {
		return errors.New("unsupported pointer button")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.button(n, action)
}

// Wheel replays the wheel delta as a series of button press/release pairs.
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
		if err := i.fake(xproto.ButtonPress, n); err != nil {
			return err
		}
		if err := i.fake(xproto.ButtonRelease, n); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseAll sends release events for every key and button still held, so a
// stuck key or button cannot persist after focus loss or disconnect.
func (i *Injector) ReleaseAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i == nil || i.conn == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	var first error
	for code := range i.pressedKeys {
		if err := i.fake(xproto.KeyRelease, code); err != nil && first == nil {
			first = err
		}
		delete(i.pressedKeys, code)
	}
	for n := range i.pressedButtons {
		if err := i.fake(xproto.ButtonRelease, n); err != nil && first == nil {
			first = err
		}
		delete(i.pressedButtons, n)
	}
	return first
}

// key injects a single key event, tracking held keys to suppress duplicate
// releases.
func (i *Injector) key(code byte, action protocol.Action) error {
	if action == protocol.ActionUp {
		if _, ok := i.pressedKeys[code]; !ok {
			return nil
		}
	}
	t := byte(xproto.KeyPress)
	if action == protocol.ActionUp {
		t = xproto.KeyRelease
	}
	if err := i.fake(t, code); err != nil {
		return err
	}
	if action == protocol.ActionDown {
		i.pressedKeys[code] = struct{}{}
	} else {
		delete(i.pressedKeys, code)
	}
	return nil
}

// button injects a single pointer button event, tracking held buttons.
func (i *Injector) button(n byte, action protocol.Action) error {
	if action == protocol.ActionUp {
		if _, ok := i.pressedButtons[n]; !ok {
			return nil
		}
	}
	t := byte(xproto.ButtonPress)
	if action == protocol.ActionUp {
		t = xproto.ButtonRelease
	}
	if err := i.fake(t, n); err != nil {
		return err
	}
	if action == protocol.ActionDown {
		i.pressedButtons[n] = struct{}{}
	} else {
		delete(i.pressedButtons, n)
	}
	return nil
}

func (i *Injector) fake(t, detail byte) error {
	return xtest.FakeInputChecked(i.conn, t, detail, 0, i.root, 0, 0, 0).Check()
}

// modifierKeycodes lists the X11 keycodes for the four left-side modifiers,
// ordered by the protocol modifier bitmap: bit0 = Shift, bit1 = Ctrl,
// bit2 = Alt, bit3 = GUI. This matches the HID/Windows convention the client
// emits, so a single press of, say, Ctrl+Shift drives the correct pair of
// keycodes.
var modifierKeycodes = [...]byte{50, 37, 64, 133}

// syncModifiers adjusts the held modifier keycodes so they match the protocol
// modifier bitmap, releasing ones that are no longer set and pressing ones that
// are newly set.
func (i *Injector) syncModifiers(bits uint8, eventCode byte) error {
	for bit, code := range modifierKeycodes {
		if code == eventCode {
			continue
		}
		_, down := i.pressedKeys[code]
		want := bits&(1<<bit) != 0
		if want && !down {
			if err := i.key(code, protocol.ActionDown); err != nil {
				return err
			}
		}
		if !want && down {
			if err := i.key(code, protocol.ActionUp); err != nil {
				return err
			}
		}
	}
	return nil
}

// keycodeForUsage maps a USB HID keyboard-page usage ID to the X11 keycode for a
// standard US QWERTY keyboard. HID usage IDs for the letters are alphabetical
// (A=0x04..Z=0x1d), but the X11 keycodes for those letters are not: the base
// layout scatters them across the three letter rows (AC01..AC09, AD01..AD10,
// AB01..AB07). The digit row uses a linear formula; everything else is a table.
func keycodeForUsage(usage uint16) (byte, bool) {
	if usage >= 0x04 && usage <= 0x1d {
		switch usage {
		case 0x04:
			return 38, true // A  AC01
		case 0x05:
			return 56, true // B  AB05
		case 0x06:
			return 54, true // C  AB03
		case 0x07:
			return 40, true // D  AC04
		case 0x08:
			return 26, true // E  AD03
		case 0x09:
			return 41, true // F  AC06
		case 0x0a:
			return 42, true // G  AC05
		case 0x0b:
			return 43, true // H  AC07
		case 0x0c:
			return 31, true // I  AD08
		case 0x0d:
			return 44, true // J  AC07
		case 0x0e:
			return 45, true // K  AC08
		case 0x0f:
			return 46, true // L  AC09
		case 0x10:
			return 58, true // M  AB07
		case 0x11:
			return 57, true // N  AB06
		case 0x12:
			return 32, true // O  AD09
		case 0x13:
			return 33, true // P  AD10
		case 0x14:
			return 24, true // Q  AD01
		case 0x15:
			return 27, true // R  AD04
		case 0x16:
			return 39, true // S  AC02
		case 0x17:
			return 28, true // T  AD05
		case 0x18:
			return 30, true // U  AD07
		case 0x19:
			return 55, true // V  AB04
		case 0x1a:
			return 25, true // W  AD02
		case 0x1b:
			return 53, true // X  AB02
		case 0x1c:
			return 29, true // Y  AD06
		case 0x1d:
			return 52, true // Z  AB01
		default:
			return 0, false
		}
	}
	if usage >= 0x1e && usage <= 0x26 {
		// Top digit row: 1-9 map to X11 keycodes 10-18.
		return byte(10 + usage - 0x1e), true
	}
	m := map[uint16]byte{
		0x27: 19, 0x28: 36, 0x29: 9, 0x2a: 22, 0x2b: 23, 0x2c: 65, 0x2d: 20, 0x2e: 21, 0x2f: 34, 0x30: 35, 0x31: 51, 0x32: 94, 0x33: 47, 0x34: 48, 0x35: 49, 0x36: 59, 0x37: 60, 0x38: 61, 0x39: 66,
		0x3a: 67, 0x3b: 68, 0x3c: 69, 0x3d: 70, 0x3e: 71, 0x3f: 72, 0x40: 73, 0x41: 74, 0x42: 75, 0x43: 76, 0x44: 95, 0x45: 96,
		0x49: 118, 0x4a: 110, 0x4b: 112, 0x4c: 119, 0x4d: 115, 0x4e: 117, 0x4f: 114, 0x50: 113, 0x51: 116, 0x52: 111,
		0x53: 77, 0x54: 106, 0x55: 63, 0x56: 82, 0x57: 86, 0x58: 104, 0x59: 87, 0x5a: 88, 0x5b: 89, 0x5c: 83, 0x5d: 84, 0x5e: 85, 0x5f: 79, 0x60: 80, 0x61: 81, 0x62: 90, 0x63: 91, 0x64: 51,
		0xe0: 37, 0xe1: 50, 0xe2: 64, 0xe3: 133, 0xe4: 105, 0xe5: 62, 0xe6: 108, 0xe7: 134,
	}
	v, ok := m[usage]
	return v, ok
}
