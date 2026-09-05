package client

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sync"

	"virtualdesktop/internal/protocol"
)

var (
	ErrDisplayNotConfigured = errors.New("client: display not configured")
	ErrKeyframeRequired     = errors.New("client: keyframe required")
)

type Snapshot struct {
	Generation, FrameSequence uint64
	Width, Height             uint32
	Pixels                    []byte
}

type Framebuffer struct {
	mu                        sync.RWMutex
	limits                    protocol.Limits
	generation, frameSequence uint64
	width, height             uint32
	pixels                    []byte
	keyframeRequired          bool
}

func NewFramebuffer(limits protocol.Limits) *Framebuffer { return &Framebuffer{limits: limits} }

func (f *Framebuffer) Configure(config protocol.DisplayConfig) error {
	if err := protocol.ValidateMessage(config, f.limits); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if config.Generation <= f.generation {
		return fmt.Errorf("client: stale display generation")
	}
	size := uint64(config.Width) * uint64(config.Height) * 4
	if size > uint64(protocol.MaxPixelPayloadHard)*16 {
		return protocol.ErrMessageTooLarge
	}
	f.generation, f.frameSequence = config.Generation, 0
	f.width, f.height = config.Width, config.Height
	f.pixels = make([]byte, int(size))
	f.keyframeRequired = true
	return nil
}

func (f *Framebuffer) Snapshot() Snapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Snapshot{f.generation, f.frameSequence, f.width, f.height, append([]byte(nil), f.pixels...)}
}

func (f *Framebuffer) Apply(frame protocol.Frame) error {
	if err := protocol.ValidateMessage(frame, f.limits); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generation == 0 {
		return ErrDisplayNotConfigured
	}
	if frame.Generation != f.generation {
		return fmt.Errorf("client: stale display generation")
	}
	if f.keyframeRequired && !frame.Keyframe {
		return ErrKeyframeRequired
	}
	if !frame.Keyframe && frame.BaseFrameSequence != f.frameSequence {
		return ErrKeyframeRequired
	}

	next := make([]byte, len(f.pixels))
	if !frame.Keyframe {
		copy(next, f.pixels)
	}
	covered := uint64(0)
	for _, rect := range frame.Rectangles {
		if rect.X >= f.width || rect.Y >= f.height || rect.Width > f.width-rect.X || rect.Height > f.height-rect.Y {
			return fmt.Errorf("client: rectangle outside display")
		}
		decoded, err := decodeRectangle(rect, f.limits)
		if err != nil {
			return err
		}
		rowBytes := int(rect.Width) * 4
		for row := uint32(0); row < rect.Height; row++ {
			dst := (int(rect.Y+row)*int(f.width) + int(rect.X)) * 4
			src := int(row) * rowBytes
			copy(next[dst:dst+rowBytes], decoded[src:src+rowBytes])
		}
		covered += uint64(rect.Width) * uint64(rect.Height)
	}
	if frame.Keyframe && covered != uint64(f.width)*uint64(f.height) {
		return fmt.Errorf("client: incomplete keyframe")
	}
	f.pixels, f.frameSequence, f.keyframeRequired = next, frame.FrameSequence, false
	return nil
}

func decodeRectangle(rect protocol.Rectangle, limits protocol.Limits) ([]byte, error) {
	expected := uint64(rect.Width) * uint64(rect.Height) * 4
	if expected > uint64(protocol.MaxPixelPayloadHard) {
		return nil, protocol.ErrMessageTooLarge
	}
	if rect.Encoding == protocol.EncodingRawBGRA {
		return append([]byte(nil), rect.Pixels...), nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(rect.Pixels))
	if err != nil {
		return nil, fmt.Errorf("client: zlib: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, int64(expected)+1))
	if err != nil {
		return nil, fmt.Errorf("client: zlib: %w", err)
	}
	if uint64(len(out)) != expected {
		return nil, fmt.Errorf("client: decoded rectangle length")
	}
	return out, nil
}
