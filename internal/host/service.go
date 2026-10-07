package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

var (
	ErrInvalidInput  = errors.New("invalid host input")
	ErrInputDisabled = fmt.Errorf("%w: input is disabled", ErrInvalidInput)
	// ErrHeartbeatTimeout is reported when the watchdog reaps a silent peer.
	ErrHeartbeatTimeout = errors.New("no response to heartbeat")
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
	// SetSteadyReadTimeout arms the heartbeat-derived read deadline that the
	// implementation applies once the session reaches Active (spec D7). It is
	// part of the contract so a future wrapper cannot silently regress F2 by
	// dropping the method; host.Service.Run keeps a runtime assertion as
	// defence in depth.
	SetSteadyReadTimeout(time.Duration)
}

type Config struct {
	DisplayID        uint32
	CaptureInterval  time.Duration
	KeyframeInterval time.Duration
	// MaxInputEventsPerSecond is the input rate limit. Exceeding it sheds
	// events (counted via Service.InputDropped, one log line per burst)
	// rather than terminating the session (F7/Q5).
	MaxInputEventsPerSecond int
	EnableInput             bool
	Damage                  damage.Config
	// HeartbeatInterval is the heartbeat cadence: an unsolicited PING probe
	// goes out to the client once half this interval has passed without any
	// message from it (spec D7: both sides probe at HeartbeatInterval/2 of
	// idle). HeartbeatTimeout is how long a fully silent peer survives before
	// the watchdog treats the session as gone. Both default to 30s when zero.
	// In the steady state the read deadline is interval + timeout +
	// transport.HeartbeatSlack, so the watchdog — never a short per-operation
	// I/O deadline — owns dead-peer detection.
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	// ProtocolVersion is the negotiated session protocol version. Zero (the
	// default) preserves v1 behavior: an oversized v1 capture is downscaled so
	// its single FRAME stays within the v1 raw-frame budget, and only ordinary
	// FRAME messages are emitted. Version2 keeps native resolution and splits
	// an oversized frame into ordered FRAME_PARTs.
	ProtocolVersion uint16
	// Limits bounds wire framing; zero selects protocol.DefaultLimits(). It is
	// the active limit set SplitFrame uses to size FRAME_PARTs on v2.
	Limits protocol.Limits
}

type Service struct {
	config  Config
	capture Capture
	input   Input

	mu            sync.RWMutex
	generation    uint64
	width, height uint32
	// nativeWidth/nativeHeight are the captured (pre-downscale) dimensions. On
	// a downscaled v1 session they are the target of the incoming pointer
	// remap; on v2 and non-downscaled v1 they equal width/height.
	nativeWidth, nativeHeight uint32
	lastInputSeq              uint64
	rateWindowStart           time.Time
	rateCount                 int
	// rateDrops counts input events shed by the rate limiter (Q5: overrun
	// degrades with a counted, logged signal instead of terminating the
	// session — finding F7). Guarded by mu like the rest of the rate state.
	rateDrops int64
	// rateBurstActive marks an ongoing drop burst so exactly one log line is
	// emitted per burst, never per event.
	rateBurstActive bool
	// keyframeRequested is set when the client sends a KEYFRAME_REQUEST (it
	// received a delta it could not apply) and cleared the next time the send
	// loop processes a capture, forcing that capture to be a full keyframe so
	// the client can resynchronize instead of ending the session.
	keyframeRequested bool
	// pingNonce is a monotonic heartbeat nonce so each PING is distinguishable.
	pingNonce uint64
	// lastActivity is the last time any message arrived from the client; the
	// heartbeat loop uses it to decide when to probe.
	lastActivity time.Time
	// pendingPing marks an unanswered probe: sentAt is when the last PING went
	// out and any message from the client clears it. The give-up branch fires
	// only past a probe (D7: "no PONG within HeartbeatTimeout after a PING"),
	// never on a session whose probes are being answered — the elapsed-since-
	// activity formulation put the deadline exactly on the tick phase and let
	// scheduling jitter kill healthy sessions.
	pendingPing     bool
	pendingPingSent time.Time
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
	// Default heartbeat cadence follows docs/architecture.md: a probe ping is
	// sent after an idle period and a silent peer is treated as gone once the
	// heartbeat timeout elapses.
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 30 * time.Second
	}
	if config.HeartbeatTimeout <= 0 {
		config.HeartbeatTimeout = 30 * time.Second
	}
	// Zero ProtocolVersion preserves v1 behavior for existing callers and
	// tests; only an explicit Version2 opts into native-resolution FRAME_PARTs.
	if config.ProtocolVersion != protocol.Version2 {
		config.ProtocolVersion = protocol.Version1
	}
	if config.Limits == (protocol.Limits{}) {
		config.Limits = protocol.DefaultLimits()
	}
	// Normalize once to the effective wire limits the codec, validator, and
	// SplitFrame all apply (mirroring protocol.Limits.bounded, which is
	// unexported): a zero or over-hard field falls back to the protocol hard
	// cap. Storing the enforced values lets a feasibility error name the cap
	// that is actually applied rather than a raw zero/oversized config field.
	config.Limits = effectiveLimits(config.Limits)
	// Clamp the detector's rectangle byte budget and rectangle count to the
	// effective wire limits before any capture, so a reduced MaxPixelPayload or
	// MaxRectangles cannot let the detector band a rectangle (or emit a
	// rectangle count) the codec or SplitFrame then rejects.
	config.Damage = damage.EffectiveConfig(config.Damage, config.Limits)
	return &Service{config: config, capture: capture, input: input}
}

// effectiveLimits returns limits with every zero or over-hard field replaced by
// the protocol hard cap, mirroring protocol.Limits.bounded exactly so the host
// applies — and can report — the same effective caps the codec, validator, and
// SplitFrame compute internally. This is the single place the host normalizes
// its wire limits; downstream code passes the result straight through, so the
// normalization is idempotent with protocol's own bounding.
func effectiveLimits(limits protocol.Limits) protocol.Limits {
	defaults := protocol.DefaultLimits()
	if limits.MaxControlPayload == 0 || limits.MaxControlPayload > protocol.MaxControlPayloadHard {
		limits.MaxControlPayload = defaults.MaxControlPayload
	}
	if limits.MaxPixelPayload == 0 || limits.MaxPixelPayload > protocol.MaxPixelPayloadHard {
		limits.MaxPixelPayload = defaults.MaxPixelPayload
	}
	if limits.MaxRectangles == 0 || limits.MaxRectangles > protocol.MaxRectanglesHard {
		limits.MaxRectangles = defaults.MaxRectangles
	}
	if limits.MaxFrameParts == 0 || limits.MaxFrameParts > protocol.MaxFramePartsHard {
		limits.MaxFrameParts = defaults.MaxFrameParts
	}
	if limits.MaxDimension == 0 || limits.MaxDimension > protocol.MaxDimensionHard {
		limits.MaxDimension = defaults.MaxDimension
	}
	return limits
}

// InputDropped reports how many input events the rate limiter has shed since
// the service started (Q5 telemetry for F7: overruns drop + count + one log
// line per burst; the session survives).
func (s *Service) InputDropped() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rateDrops
}

func (s *Service) Run(ctx context.Context, peer Peer) error {
	if ctx == nil || peer == nil || s.capture == nil || s.input == nil {
		return errors.New("host service requires context, peer, capture, and input adapters")
	}
	// D7: in the steady state (Active) the read deadline derives from the
	// heartbeat config so the heartbeat watchdog — not a short per-operation
	// I/O deadline — owns dead-peer detection. Establishment reads keep the
	// peer's IOTimeout until the first DISPLAY_CONFIG flips the session to
	// Active. SetSteadyReadTimeout is part of the Peer contract (O2), so the
	// type assertion below is a runtime belt for dynamically-wrapped peers;
	// it refuses the session rather than silently skipping the arming (N1).
	setter, ok := peer.(interface{ SetSteadyReadTimeout(time.Duration) })
	if !ok {
		return fmt.Errorf("host service: peer %T does not implement SetSteadyReadTimeout; steady-state read deadlines must derive from the heartbeat config (F2/D7)", peer)
	}
	setter.SetSteadyReadTimeout(s.config.HeartbeatInterval + s.config.HeartbeatTimeout + transport.HeartbeatSlack)
	// Build the detector against the active wire limits first, so a pixel-payload
	// budget too small for even a one-pixel frame is reported clearly before any
	// capture is attempted rather than as a frame the codec rejects later.
	detector, err := damage.NewDetectorWithLimits(s.config.Damage, s.config.Limits)
	if err != nil {
		return fmt.Errorf("host service: %w", err)
	}
	image, err := s.capture.Capture(ctx, s.config.DisplayID)
	if err != nil {
		return fmt.Errorf("capture initial display: %w", err)
	}
	lastKeyframe := time.Time{}
	if lastKeyframe, err = s.sendCapture(ctx, peer, detector, image, true, lastKeyframe); err != nil {
		return err
	}
	// Seed the heartbeat's activity clock so the first probe fires only after
	// half a HeartbeatInterval of idle, not immediately on the zero time.
	s.recordActivity()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = s.input.ReleaseAll(context.WithoutCancel(ctx)) }()

	captures := make(chan damage.Image, 1)
	errs := make(chan error, 4)
	go s.captureLoop(runCtx, captures, errs)
	go s.sendLoop(runCtx, peer, detector, captures, lastKeyframe, errs)
	go s.inputLoop(runCtx, peer, errs)
	go s.heartbeatLoop(runCtx, peer, errs)

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
	// Honor a pending KEYFRAME_REQUEST: the client asked to resynchronize (it
	// could not apply a delta), so force the next visual update to a full
	// keyframe so it can catch up instead of ending the session. This must be
	// folded into force before the single detector.Compare call below, which
	// advances the detector's generation/sequence and has side effects.
	s.mu.Lock()
	force = force || s.keyframeRequested
	s.keyframeRequested = false
	s.mu.Unlock()

	// Apply the v1 compatibility transform before damage detection and
	// DISPLAY_CONFIG, so the detector, the advertised dimensions, and every
	// encoded rectangle all agree on the downscaled geometry. The native
	// dimensions are retained for pointer remapping.
	native := image
	image = s.prepareCapture(image)
	s.mu.Lock()
	s.nativeWidth, s.nativeHeight = native.Width, native.Height
	s.mu.Unlock()

	frame, changed, err := detector.Compare(image, force)
	if err != nil {
		return lastKeyframe, fmt.Errorf("process capture within MaxRectangles=%d, MaxFrameParts=%d, MaxPixelPayload=%d: %w",
			s.config.Limits.MaxRectangles, s.config.Limits.MaxFrameParts, s.config.Limits.MaxPixelPayload, err)
	}
	if !changed {
		return lastKeyframe, nil
	}

	// Plan the wire packing BEFORE announcing any DISPLAY_CONFIG (finding M2):
	// a frame the configured MaxFrameParts/MaxPixelPayload cannot carry must be
	// refused here, so the peer is never told a session exists and then left
	// without the frame that completes it. The plan is what gets sent below, so
	// the packing (SplitFrame) runs exactly once, never duplicated.
	plan, err := s.planFrame(frame)
	if err != nil {
		return lastKeyframe, fmt.Errorf("host service: generation %d frame cannot be sent within MaxFrameParts=%d, MaxRectangles=%d, MaxPixelPayload=%d (rectangles=%d): %w",
			frame.Generation, s.config.Limits.MaxFrameParts, s.config.Limits.MaxRectangles, s.config.Limits.MaxPixelPayload, len(frame.Rectangles), err)
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
	if err := s.sendPlan(ctx, peer, plan); err != nil {
		return lastKeyframe, err
	}
	if frame.Keyframe {
		lastKeyframe = time.Now()
	}
	return lastKeyframe, nil
}

// prepareCapture applies the v1 compatibility transform: when a v1 session's
// capture exceeds the v1 raw-frame budget it is box-downscaled before damage
// detection and DISPLAY_CONFIG, so the client receives a smaller advertised
// display it can actually render instead of a frame the wire rejects. The
// budget is the detector's effective per-rectangle budget, already clamped to
// the session's pixel-payload limit, so a reduced limit downscales further
// instead of overflowing the wire. A v2 session keeps native resolution and
// relies on FRAME_PART splitting instead.
func (s *Service) prepareCapture(image damage.Image) damage.Image {
	if s.config.ProtocolVersion == protocol.Version2 {
		return image
	}
	k := downscaleFactor(image.Width, image.Height, s.config.Damage.MaxRectangleBytes)
	if k == 0 {
		return image
	}
	return downscaleImage(image, k)
}

// framePlan describes how one logical frame goes on the wire: whole is true
// when it is sent as a single FRAME, otherwise parts holds its ordered
// FRAME_PART sequence. Planning is separated from sending so the packer runs
// before any DISPLAY_CONFIG announces the session (finding M2) and so the pack
// computed once is the one sent, never re-packed.
type framePlan struct {
	whole bool
	frame protocol.Frame
	parts []protocol.FramePart
}

// planFrame computes how frame will be sent under the active limits without
// touching the peer. v1 always yields a whole FRAME because its wire has no
// part type. On v2 it runs protocol.SplitFrame: a frame that fits stays whole
// (ErrFrameFits), an oversized one becomes an ordered part list, and a frame
// the limits cannot carry (ErrFrameTooLarge, ErrPartTooLarge) is returned so
// the caller can refuse before sending DISPLAY_CONFIG.
func (s *Service) planFrame(frame protocol.Frame) (framePlan, error) {
	if s.config.ProtocolVersion != protocol.Version2 {
		return framePlan{whole: true, frame: frame}, nil
	}
	parts, err := protocol.SplitFrame(frame, s.config.Limits)
	switch {
	case errors.Is(err, protocol.ErrFrameFits):
		return framePlan{whole: true, frame: frame}, nil
	case err != nil:
		return framePlan{}, err
	default:
		return framePlan{parts: parts}, nil
	}
}

// sendPlan transmits a plan computed by planFrame. A part send failure names
// the part so a partially delivered logical frame is diagnosable.
func (s *Service) sendPlan(ctx context.Context, peer Peer, plan framePlan) error {
	if plan.whole {
		if err := peer.Send(ctx, plan.frame); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}
		return nil
	}
	for _, part := range plan.parts {
		if err := peer.Send(ctx, part); err != nil {
			return fmt.Errorf("send frame part %d/%d: %w", part.PartIndex, part.PartCount, err)
		}
	}
	return nil
}

func (s *Service) inputLoop(ctx context.Context, peer Peer, errs chan<- error) {
	for {
		message, err := peer.Receive(ctx)
		if err != nil {
			report(errs, err)
			return
		}
		s.recordActivity()
		if err := s.handleInput(ctx, peer, message); err != nil {
			report(errs, err)
			return
		}
	}
}

// recordActivity stamps the last time any message arrived so the heartbeat
// loop can tell a responsive peer from a silent one. Any message also answers
// a pending PING probe (the watchdog only counts silence past a probe).
func (s *Service) recordActivity() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.pendingPing = false
	s.mu.Unlock()
}

// heartbeatLoop implements the idle-session watchdog described in
// docs/architecture.md per remediation spec D7: once the peer has sent nothing
// for half the heartbeat interval the host probes it with an unsolicited PING
// (both sides probe at HeartbeatInterval/2 of idle), and a peer that stays
// silent for a full HeartbeatTimeout after an unanswered probe is treated as
// gone. Give-up is measured past the probe — not past the last activity on a
// phase-aligned tick — so a peer whose PONGs keep arriving can never be
// reaped by scheduling jitter, and the heartbeat watchdog (not the read
// deadline) owns dead-peer detection.
func (s *Service) heartbeatLoop(ctx context.Context, peer Peer, errs chan<- error) {
	half := s.config.HeartbeatInterval / 2
	if half <= 0 {
		half = s.config.HeartbeatInterval
	}
	ticker := time.NewTicker(half)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.pendingPing && time.Since(s.pendingPingSent) >= s.config.HeartbeatTimeout {
				s.mu.Unlock()
				report(errs, ErrHeartbeatTimeout)
				return
			}
			probe := time.Since(s.lastActivity) >= half
			var nonce uint64
			if probe {
				s.pingNonce++
				nonce = s.pingNonce
				if !s.pendingPing {
					s.pendingPing = true
					s.pendingPingSent = time.Now()
				}
			}
			s.mu.Unlock()
			if !probe {
				continue
			}
			if err := peer.Send(ctx, protocol.Ping{Nonce: nonce}); err != nil {
				report(errs, err)
				return
			}
		}
	}
}

func (s *Service) handleInput(ctx context.Context, peer Peer, message protocol.Message) error {
	// Control messages the client may send. They are not input events: they
	// carry no input header and must not be rate limited or fall through to
	// the input handlers. An unhandled control message is a protocol error;
	// anything else in this list is handled below so a stray control message
	// does not silently end the session.
	switch v := message.(type) {
	case protocol.KeyframeRequest:
		// The client received a delta it could not apply and asked to
		// resynchronize. Mark that the next visual update must be a full
		// keyframe so it can catch up; the send loop clears the flag.
		s.mu.Lock()
		s.keyframeRequested = true
		s.mu.Unlock()
		return nil
	case protocol.Ping:
		// Respond to a client heartbeat probe; the client uses this to keep
		// the session alive and detect a dead host.
		if err := peer.Send(ctx, protocol.Pong{Nonce: v.Nonce}); err != nil {
			return err
		}
		return nil
	case protocol.Pong:
		// A client answered one of our probes; treat it as activity.
		s.recordActivity()
		return nil
	}

	if !s.config.EnableInput {
		return ErrInputDisabled
	}
	generation, sequence, ok := inputHeader(message)
	if !ok {
		return fmt.Errorf("%w: message type %T", ErrInvalidInput, message)
	}
	if err := protocol.ValidateMessage(message, s.config.Limits); err != nil {
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
		// Q5 (F7): a rate overrun is a DEGRADE, not a disconnect — a fast
		// polling mouse legitimately exceeds the limit. Drop the event,
		// count it, and log exactly one line at the start of each burst.
		// Session termination stays reserved for protocol violations, which
		// the checks above still enforce.
		s.rateDrops++
		firstOfBurst := !s.rateBurstActive
		s.rateBurstActive = true
		total := s.rateDrops
		s.mu.Unlock()
		if firstOfBurst {
			slog.Info("host: input rate limit exceeded, dropping events until the next rate window",
				"limit_per_second", s.config.MaxInputEventsPerSecond, "total_dropped", total)
		}
		return nil
	}
	s.rateBurstActive = false // an accepted event ends any drop burst
	width, height := s.width, s.height
	nativeWidth, nativeHeight := s.nativeWidth, s.nativeHeight
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
		// The client reports coordinates in the advertised (possibly
		// downscaled) display space; map them back to native capture
		// coordinates so a v1 client's pointer still lands where it aimed.
		return s.input.Move(ctx, remapCoordinate(v.X, width, nativeWidth), remapCoordinate(v.Y, height, nativeHeight))
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
