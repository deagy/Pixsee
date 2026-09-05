package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/protocol"
)

var (
	ErrInvalidInput  = errors.New("invalid host input")
	ErrInputDisabled = fmt.Errorf("%w: input is disabled", ErrInvalidInput)
	ErrInputRate     = fmt.Errorf("%w: rate limit exceeded", ErrInvalidInput)
)

type Capture interface {
	Capture(context.Context, uint32) (damage.Image, error)
}

type Input interface {
	Key(context.Context, uint16, protocol.Action, uint8) error
	Move(context.Context, uint32, uint32) error
	Button(context.Context, protocol.Button, protocol.Action) error
	Wheel(context.Context, int16, int16) error
	ReleaseAll(context.Context) error
}

type Peer interface {
	Send(context.Context, protocol.Message) error
	Receive(context.Context) (protocol.Message, error)
}

type Config struct {
	DisplayID               uint32
	CaptureInterval         time.Duration
	KeyframeInterval        time.Duration
	MaxInputEventsPerSecond int
	EnableInput             bool
	Damage                  damage.Config
}

type Service struct {
	config  Config
	capture Capture
	input   Input

	mu              sync.RWMutex
	generation      uint64
	width, height   uint32
	lastInputSeq    uint64
	rateWindowStart time.Time
	rateCount       int
}

func NewService(config Config, capture Capture, input Input) *Service {
	if config.CaptureInterval <= 0 {
		config.CaptureInterval = time.Second / 30
	}
	if config.CaptureInterval < time.Second/30 {
		config.CaptureInterval = time.Second / 30
	}
	if config.KeyframeInterval <= 0 {
		config.KeyframeInterval = 10 * time.Second
	}
	if config.MaxInputEventsPerSecond <= 0 {
		config.MaxInputEventsPerSecond = 500
	}
	return &Service{config: config, capture: capture, input: input}
}

func (s *Service) Run(ctx context.Context, peer Peer) error {
	if ctx == nil || peer == nil || s.capture == nil || s.input == nil {
		return errors.New("host service requires context, peer, capture, and input adapters")
	}
	image, err := s.capture.Capture(ctx, s.config.DisplayID)
	if err != nil {
		return fmt.Errorf("capture initial display: %w", err)
	}
	detector := damage.NewDetector(s.config.Damage)
	lastKeyframe := time.Time{}
	if lastKeyframe, err = s.sendCapture(ctx, peer, detector, image, true, lastKeyframe); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = s.input.ReleaseAll(context.WithoutCancel(ctx)) }()

	captures := make(chan damage.Image, 1)
	errs := make(chan error, 3)
	go s.captureLoop(runCtx, captures, errs)
	go s.sendLoop(runCtx, peer, detector, captures, lastKeyframe, errs)
	go s.inputLoop(runCtx, peer, errs)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errs:
		cancel()
		return err
	}
}

func (s *Service) captureLoop(ctx context.Context, captures chan damage.Image, errs chan<- error) {
	ticker := time.NewTicker(s.config.CaptureInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			image, err := s.capture.Capture(ctx, s.config.DisplayID)
			if err != nil {
				report(errs, fmt.Errorf("capture display: %w", err))
				return
			}
			select {
			case captures <- image:
			default:
				// Keep one latest complete capture; never accumulate stale desktop data.
				select {
				case <-captures:
				default:
				}
				select {
				case captures <- image:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (s *Service) sendLoop(ctx context.Context, peer Peer, detector *damage.Detector, captures <-chan damage.Image, lastKeyframe time.Time, errs chan<- error) {
	for {
		select {
		case <-ctx.Done():
			return
		case image := <-captures:
			var err error
			lastKeyframe, err = s.sendCapture(ctx, peer, detector, image, time.Since(lastKeyframe) >= s.config.KeyframeInterval, lastKeyframe)
			if err != nil {
				report(errs, err)
				return
			}
		}
	}
}

func (s *Service) sendCapture(ctx context.Context, peer Peer, detector *damage.Detector, image damage.Image, force bool, lastKeyframe time.Time) (time.Time, error) {
	frame, changed, err := detector.Compare(image, force)
	if err != nil {
		return lastKeyframe, fmt.Errorf("process capture: %w", err)
	}
	if !changed {
		return lastKeyframe, nil
	}

	s.mu.RLock()
	oldGeneration := s.generation
	s.mu.RUnlock()
	if frame.Generation != oldGeneration {
		config := protocol.DisplayConfig{Generation: frame.Generation, Width: image.Width, Height: image.Height, PixelFormat: protocol.PixelBGRA8888}
		if err := peer.Send(ctx, config); err != nil {
			return lastKeyframe, fmt.Errorf("send display config: %w", err)
		}
		s.mu.Lock()
		s.generation, s.width, s.height = frame.Generation, image.Width, image.Height
		s.lastInputSeq = 0
		s.mu.Unlock()
	}
	if err := peer.Send(ctx, frame); err != nil {
		return lastKeyframe, fmt.Errorf("send frame: %w", err)
	}
	if frame.Keyframe {
		lastKeyframe = time.Now()
	}
	return lastKeyframe, nil
}

func (s *Service) inputLoop(ctx context.Context, peer Peer, errs chan<- error) {
	for {
		message, err := peer.Receive(ctx)
		if err != nil {
			report(errs, err)
			return
		}
		if err := s.handleInput(ctx, message); err != nil {
			report(errs, err)
			return
		}
	}
}

func (s *Service) handleInput(ctx context.Context, message protocol.Message) error {
	if !s.config.EnableInput {
		return ErrInputDisabled
	}
	generation, sequence, ok := inputHeader(message)
	if !ok {
		return fmt.Errorf("%w: message type %T", ErrInvalidInput, message)
	}
	if err := protocol.ValidateMessage(message, protocol.DefaultLimits()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	s.mu.Lock()
	if generation != s.generation || sequence <= s.lastInputSeq {
		s.mu.Unlock()
		return fmt.Errorf("%w: generation or sequence", ErrInvalidInput)
	}
	now := time.Now()
	if s.rateWindowStart.IsZero() || now.Sub(s.rateWindowStart) >= time.Second {
		s.rateWindowStart, s.rateCount = now, 0
	}
	if s.rateCount >= s.config.MaxInputEventsPerSecond {
		s.mu.Unlock()
		return ErrInputRate
	}
	width, height := s.width, s.height
	s.rateCount++
	s.lastInputSeq = sequence
	s.mu.Unlock()

	switch v := message.(type) {
	case protocol.Key:
		return s.input.Key(ctx, v.Usage, v.Action, v.Modifiers)
	case protocol.PointerMove:
		if v.X >= width || v.Y >= height {
			return fmt.Errorf("%w: pointer outside display", ErrInvalidInput)
		}
		return s.input.Move(ctx, v.X, v.Y)
	case protocol.PointerButton:
		return s.input.Button(ctx, v.Button, v.Action)
	case protocol.PointerWheel:
		return s.input.Wheel(ctx, v.Horizontal, v.Vertical)
	case protocol.FocusLost:
		return s.input.ReleaseAll(ctx)
	default:
		return fmt.Errorf("%w: message type %T", ErrInvalidInput, message)
	}
}

func inputHeader(message protocol.Message) (uint64, uint64, bool) {
	switch v := message.(type) {
	case protocol.Key:
		return v.Generation, v.InputSequence, true
	case protocol.PointerMove:
		return v.Generation, v.InputSequence, true
	case protocol.PointerButton:
		return v.Generation, v.InputSequence, true
	case protocol.PointerWheel:
		return v.Generation, v.InputSequence, true
	case protocol.FocusLost:
		return v.Generation, v.InputSequence, true
	default:
		return 0, 0, false
	}
}

func report(ch chan<- error, err error) {
	select {
	case ch <- err:
	default:
	}
}
