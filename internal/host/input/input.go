// Package input injects the restricted keyboard and pointer event set from an
// authenticated client session onto the host. The package is platform neutral:
// every OS-specific adapter implements the same method set and is selected by
// build tags, so the session layer never imports a concrete platform.
//
// This file holds helpers shared by every adapter. The platform files declare
// New() and the Input implementation; they all share buttonNumber, wheelClicks,
// and abs below so the semantics stay identical across linux, windows, and
// darwin.
package input

import (
	"errors"

	"virtualdesktop/internal/protocol"
)

// maxWheelClicks bounds the number of wheel detents a single event may request.
const maxWheelClicks = 20

// buttonNumber maps a protocol button to the platform-native button number.
// The mapping is identical on every platform, so it lives here.
func buttonNumber(button protocol.Button) byte {
	switch button {
	case protocol.ButtonLeft:
		return 1
	case protocol.ButtonMiddle:
		return 2
	case protocol.ButtonRight:
		return 3
	case protocol.ButtonBack:
		return 8
	case protocol.ButtonForward:
		return 9
	}
	return 0
}

// wheelClicks converts a signed wheel delta (in multiples of 120 units per
// detent) into the ordered list of native button numbers to replay. A positive
// vertical delta scrolls up (button 4), a negative delta scrolls down (button
// 5); horizontal follows the same sign convention with buttons 6/7.
func wheelClicks(horizontal, vertical int16) ([]byte, error) {
	if horizontal%120 != 0 || vertical%120 != 0 {
		return nil, errors.New("wheel delta must be a multiple of 120")
	}
	h, v := int(horizontal/120), int(vertical/120)
	if abs(h)+abs(v) > maxWheelClicks {
		return nil, errors.New("wheel event exceeds click limit")
	}
	out := make([]byte, 0, abs(h)+abs(v))
	for ; v > 0; v-- {
		out = append(out, 4)
	}
	for ; v < 0; v++ {
		out = append(out, 5)
	}
	for ; h > 0; h-- {
		out = append(out, 7)
	}
	for ; h < 0; h++ {
		out = append(out, 6)
	}
	return out, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
