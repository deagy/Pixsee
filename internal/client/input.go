package client

import (
	"errors"
	"math"
	"sort"
	"sync"

	"virtualdesktop/internal/protocol"
)

type InputSender interface{ Send(protocol.Message) error }

type InputState struct {
	mu                 sync.Mutex
	sender             InputSender
	generation         uint64
	width, height      uint32
	sequence           uint64
	focused, connected bool
	keys               map[uint16]uint8
	buttons            map[protocol.Button]struct{}
}

func NewInputState(sender InputSender) *InputState {
	return &InputState{sender: sender, connected: true, keys: make(map[uint16]uint8), buttons: make(map[protocol.Button]struct{})}
}

// SetSender replaces the delivery target for captured input. The shared-input
// path uses it so the renderer's InputState reaches the session's sender.
func (s *InputState) SetSender(sender InputSender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
}

func (s *InputState) SetDisplay(generation uint64, width, height uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation, s.width, s.height = generation, width, height
}

func (s *InputState) SetFocused(focused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return nil
	}
	if !focused && s.focused {
		if err := s.releaseAllLocked(); err != nil {
			s.connected = false
			return err
		}
		if err := s.sendLocked(func(seq uint64) protocol.Message {
			return protocol.FocusLost{Generation: s.generation, InputSequence: seq}
		}); err != nil {
			s.connected = false
			return err
		}
	}
	s.focused = focused
	return nil
}

func (s *InputState) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return nil
	}
	err := s.releaseAllLocked()
	s.connected, s.focused = false, false
	return err
}

func (s *InputState) Connect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected, s.focused = true, false
	s.keys = make(map[uint16]uint8)
	s.buttons = make(map[protocol.Button]struct{})
}

func (s *InputState) Key(usage uint16, action protocol.Action, modifiers uint8) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active() {
		return nil
	}
	_, held := s.keys[usage]
	if (action == protocol.ActionDown && held) || (action == protocol.ActionUp && !held) {
		return nil
	}
	message := func(seq uint64) protocol.Message {
		return protocol.Key{Generation: s.generation, InputSequence: seq, Usage: usage, Action: action, Modifiers: modifiers}
	}
	if err := s.validateAndSendLocked(message); err != nil {
		return err
	}
	if action == protocol.ActionDown {
		s.keys[usage] = modifiers
	} else {
		delete(s.keys, usage)
	}
	return nil
}

func (s *InputState) Button(button protocol.Button, action protocol.Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active() {
		return nil
	}
	_, held := s.buttons[button]
	if (action == protocol.ActionDown && held) || (action == protocol.ActionUp && !held) {
		return nil
	}
	message := func(seq uint64) protocol.Message {
		return protocol.PointerButton{Generation: s.generation, InputSequence: seq, Button: button, Action: action}
	}
	if err := s.validateAndSendLocked(message); err != nil {
		return err
	}
	if action == protocol.ActionDown {
		s.buttons[button] = struct{}{}
	} else {
		delete(s.buttons, button)
	}
	return nil
}

func (s *InputState) Pointer(x, y, viewWidth, viewHeight float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active() {
		return nil
	}
	rx, ry, ok := MapCoordinates(x, y, viewWidth, viewHeight, s.width, s.height)
	if !ok {
		return nil
	}
	return s.validateAndSendLocked(func(seq uint64) protocol.Message {
		return protocol.PointerMove{Generation: s.generation, InputSequence: seq, X: rx, Y: ry}
	})
}

func (s *InputState) Wheel(horizontal, vertical int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active() {
		return nil
	}
	return s.validateAndSendLocked(func(seq uint64) protocol.Message {
		return protocol.PointerWheel{Generation: s.generation, InputSequence: seq, Horizontal: horizontal, Vertical: vertical}
	})
}

func (s *InputState) active() bool {
	return s.connected && s.focused && s.generation != 0 && s.width != 0 && s.height != 0 && s.sender != nil
}
func (s *InputState) validateAndSendLocked(build func(uint64) protocol.Message) error {
	m := build(s.sequence + 1)
	if err := protocol.ValidateMessage(m, protocol.DefaultLimits()); err != nil {
		return err
	}
	return s.sendLocked(build)
}
func (s *InputState) sendLocked(build func(uint64) protocol.Message) error {
	if s.sender == nil {
		return errors.New("client: input sender unavailable")
	}
	seq := s.sequence + 1
	if err := s.sender.Send(build(seq)); err != nil {
		return err
	}
	s.sequence = seq
	return nil
}
func (s *InputState) releaseAllLocked() error {
	keys := make([]int, 0, len(s.keys))
	for k := range s.keys {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, raw := range keys {
		k := uint16(raw)
		mod := s.keys[k]
		if err := s.validateAndSendLocked(func(seq uint64) protocol.Message {
			return protocol.Key{Generation: s.generation, InputSequence: seq, Usage: k, Action: protocol.ActionUp, Modifiers: mod}
		}); err != nil {
			return err
		}
		delete(s.keys, k)
	}
	buttons := make([]int, 0, len(s.buttons))
	for b := range s.buttons {
		buttons = append(buttons, int(b))
	}
	sort.Ints(buttons)
	for _, raw := range buttons {
		b := protocol.Button(raw)
		if err := s.validateAndSendLocked(func(seq uint64) protocol.Message {
			return protocol.PointerButton{Generation: s.generation, InputSequence: seq, Button: b, Action: protocol.ActionUp}
		}); err != nil {
			return err
		}
		delete(s.buttons, b)
	}
	return nil
}

func MapCoordinates(x, y, viewWidth, viewHeight float64, remoteWidth, remoteHeight uint32) (uint32, uint32, bool) {
	if viewWidth <= 0 || viewHeight <= 0 || remoteWidth == 0 || remoteHeight == 0 || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return 0, 0, false
	}
	scale := math.Min(viewWidth/float64(remoteWidth), viewHeight/float64(remoteHeight))
	renderedW, renderedH := float64(remoteWidth)*scale, float64(remoteHeight)*scale
	offsetX, offsetY := (viewWidth-renderedW)/2, (viewHeight-renderedH)/2
	if x < offsetX || y < offsetY || x >= offsetX+renderedW || y >= offsetY+renderedH {
		return 0, 0, false
	}
	rx := uint32((x - offsetX) / scale)
	ry := uint32((y - offsetY) / scale)
	if rx >= remoteWidth {
		rx = remoteWidth - 1
	}
	if ry >= remoteHeight {
		ry = remoteHeight - 1
	}
	return rx, ry, true
}
