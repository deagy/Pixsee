package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

var ErrDisconnected = errors.New("client: disconnected")

type ConnectionState uint8

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateAuthenticating
	StateNegotiating
	StateConnected
	StateReconnecting
	StateClosed
	StateError
)

type Renderer interface{ Present(Snapshot) }
type StateObserver interface{ ConnectionState(ConnectionState, error) }

type Config struct {
	Token          [32]byte
	TLSConfig      *tls.Config
	Limits         protocol.Limits
	IOTimeout      time.Duration
	ReconnectDelay time.Duration
	// HeartbeatInterval and HeartbeatTimeout mirror the host's
	// -heartbeat-interval/-heartbeat-timeout flags (both default to 30s when
	// unset, the same as the host's). In the steady state the read deadline is
	// derived from them — interval + timeout + transport.HeartbeatSlack — the
	// client probes an idle peer with an unsolicited PING after
	// HeartbeatInterval/2 of receiving nothing, and a peer still silent
	// HeartbeatTimeout after that probe closes the connection so the Run loop
	// reconnects. Keep host and client symmetric; see docs §5.
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	// ConnectTimeout bounds the initial connect sequence (Dial + TLS
	// handshake + AUTH + CLIENT_HELLO + SERVER_HELLO). On exhaustion Run
	// returns an error instead of reconnecting forever against a black-hole
	// IP or a firewall-dropped port. Zero disables the limit, preserving the
	// historical behavior of retrying until the context is cancelled.
	ConnectTimeout time.Duration
	Dial           func(context.Context) (net.Conn, error)
	// Input, when non-nil, is shared with the renderer instead of one being
	// created here. The caller must supply it to both the renderer and the
	// session so captured input reaches this session.
	Input *InputState
}

type Session struct {
	config      Config
	framebuffer *Framebuffer
	renderer    Renderer
	observer    StateObserver
	sender      *peerSender
	input       *InputState
}

func NewSession(config Config, renderer Renderer, observer StateObserver) (*Session, error) {
	if config.TLSConfig == nil {
		return nil, errors.New("client: TLS configuration is required")
	}
	if config.TLSConfig.MinVersion != tls.VersionTLS13 {
		return nil, errors.New("client: TLS 1.3 is required")
	}
	if config.Token == ([32]byte{}) {
		return nil, errors.New("client: authentication token is required")
	}
	if config.IOTimeout <= 0 {
		config.IOTimeout = 10 * time.Second
	}
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = time.Second
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 30 * time.Second
	}
	if config.HeartbeatTimeout <= 0 {
		config.HeartbeatTimeout = 30 * time.Second
	}
	if config.Limits == (protocol.Limits{}) {
		config.Limits = protocol.DefaultLimits()
	}
	sender := &peerSender{}
	s := &Session{config: config, framebuffer: NewFramebuffer(config.Limits), renderer: renderer, observer: observer, sender: sender}
	if config.Input != nil {
		config.Input.SetSender(sender)
		s.input = config.Input
	} else {
		s.input = NewInputState(sender)
	}
	return s, nil
}

func (s *Session) Input() *InputState { return s.input }
func (s *Session) Snapshot() Snapshot { return s.framebuffer.Snapshot() }

func (s *Session) Run(ctx context.Context) error {
	if s.config.Dial == nil {
		return errors.New("client: dial function is required")
	}
	// ConnectTimeout bounds only the initial connect sequence (Dial + TLS
	// handshake + AUTH + CLIENT_HELLO + SERVER_HELLO). Once the session is
	// up, reconnection is governed by ctx as before.
	runCtx := ctx
	if s.config.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.config.ConnectTimeout)
		defer cancel()
	}
	attempt := 0
	for {
		if err := runCtx.Err(); err != nil {
			// A deadline reached during the initial connect is surfaced as a
			// distinct error so callers can tell "never connected" apart from
			// a context cancelled after connecting.
			if attempt == 0 && runCtx != ctx {
				s.notify(StateClosed, nil)
				return fmt.Errorf("client: connect timeout: %w", err)
			}
			s.notify(StateClosed, nil)
			return err
		}
		if attempt == 0 {
			s.notify(StateConnecting, nil)
		} else {
			s.notify(StateReconnecting, nil)
		}
		// Dial against runCtx so ConnectTimeout bounds the initial
		// connect. The parent ctx still governs reconnection.
		conn, err := s.config.Dial(runCtx)
		if err == nil {
			err = s.ServeConn(ctx, conn)
		}
		if ctx.Err() != nil {
			s.notify(StateClosed, nil)
			return ctx.Err()
		}
		if err == nil {
			// ServeConn returned cleanly (server sent CLOSE). Stop the loop.
			s.notify(StateClosed, nil)
			return nil
		}
		// If the initial dial was aborted by the connect timeout, surface
		// that error now instead of treating it as a transient failure and
		// waiting a reconnect delay against an already-expired deadline.
		if attempt == 0 && runCtx != ctx && runCtx.Err() != nil {
			s.notify(StateClosed, nil)
			return fmt.Errorf("client: connect timeout: %w", runCtx.Err())
		}
		s.notify(StateError, err)
		attempt++
		timer := time.NewTimer(s.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			s.notify(StateClosed, nil)
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Session) ServeConn(ctx context.Context, conn net.Conn) (result error) {
	if conn == nil {
		return errors.New("client: connection is required")
	}
	var tlsConn *tls.Conn
	if existing, ok := conn.(*tls.Conn); ok {
		tlsConn = existing
	} else {
		tlsConn = tls.Client(conn, s.config.TLSConfig.Clone())
	}
	defer conn.Close()
	s.notify(StateConnecting, nil)
	if err := transport.Handshake(ctx, tlsConn, s.config.IOTimeout); err != nil {
		s.notify(StateError, err)
		return err
	}
	// Pass the raw socket so Close can bypass tls.Conn.Close's close_notify,
	// which would hang when the remote peer has stopped reading.
	peer := transport.NewPeerConn(tlsConn, conn, protocol.RoleClient, s.config.Limits, s.config.IOTimeout)
	// D7: once the session is Active the read deadline derives from the
	// heartbeat config, so the keepalive watchdog — not a per-operation I/O
	// deadline — owns dead-peer detection. Establishment reads keep IOTimeout
	// until the peer's state machine reaches Active.
	peer.SetSteadyReadTimeout(s.config.HeartbeatInterval + s.config.HeartbeatTimeout + transport.HeartbeatSlack)
	s.sender.set(peer)
	// F1: a reconnect lands on a fresh host service whose generation restarts
	// at 1. Drop the previous session's framebuffer state before this
	// connection's first DISPLAY_CONFIG arrives, or it would be rejected as a
	// stale display generation forever.
	s.framebuffer.Reset()
	defer func() {
		s.sender.set(nil)
		_ = s.input.Disconnect()
		_ = peer.Close()
	}()
	s.notify(StateAuthenticating, nil)
	if err := peer.AuthenticateClient(ctx, s.config.Token); err != nil {
		s.notify(StateError, err)
		return err
	}
	s.notify(StateNegotiating, nil)
	// Advertise v2 while staying backward compatible: AUTH and CLIENT_HELLO
	// ride a v1 record envelope, and a deployed v1 host clamps the offer down
	// to v1 during the first handshake (no retry).
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version2}); err != nil {
		return err
	}
	// Accept either record-envelope version until SERVER_HELLO fixes the
	// negotiation, then pin both directions to the negotiated version.
	peer.SetVersionWindow(protocol.Version1, protocol.Version2)
	message, err := peer.Receive(ctx)
	if err != nil {
		return err
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok {
		return fmt.Errorf("client: expected SERVER_HELLO, got %T", message)
	}
	negotiated, err := protocol.NegotiateVersion(protocol.Version1, protocol.Version2, hello.Version, hello.Version)
	if err != nil {
		return err
	}
	peer.PinVersion(negotiated)
	message, err = peer.Receive(ctx)
	if err != nil {
		return err
	}
	config, ok := message.(protocol.DisplayConfig)
	if !ok {
		return fmt.Errorf("client: expected DISPLAY_CONFIG, got %T", message)
	}
	if err := s.applyDisplay(config); err != nil {
		return err
	}
	// F3: teardown disconnected input for the previous connection. Re-arm the
	// input state per connection — before this point the generation is set
	// and the sender is live, and input is connected=false until Connect, so
	// no keystroke can reach the wire during establishment. Connect clears
	// any inherited key/button bookkeeping (docs §5: no inherited input
	// state on a new connection).
	s.input.Connect()
	s.notify(StateConnected, nil)

	// Client-side keepalive (D7): probe an idle peer with PING and let the
	// watchdog close a peer that stays silent past HeartbeatTimeout.
	live := &keepalive{lastRx: time.Now()}
	keepaliveCtx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go s.keepaliveLoop(keepaliveCtx, peer, live)

	for {
		message, err = peer.Receive(ctx)
		if err != nil {
			return err
		}
		live.touch()
		switch value := message.(type) {
		case protocol.DisplayConfig:
			if err := s.applyDisplay(value); err != nil {
				return err
			}
		case protocol.Frame:
			if err := s.framebuffer.Apply(value); err != nil {
				if errors.Is(err, ErrKeyframeRequired) {
					if sendErr := peer.Send(ctx, protocol.KeyframeRequest{Generation: s.framebuffer.Generation()}); sendErr != nil {
						return sendErr
					}
					continue
				}
				return err
			}
			if s.renderer != nil {
				s.renderer.Present(s.framebuffer.Snapshot())
			}
		case protocol.FramePart:
			// v2 only: the decoder's negotiated-version gate rejects a
			// FRAME_PART on a v1 envelope before it reaches here. Present
			// exactly once, only when the logical frame commits; intermediate
			// parts leave the committed pixels and sequence untouched. A
			// recoverable part-0 rejection sends one KEYFRAME_REQUEST and the
			// framebuffer drains the rest of that logical frame, so the
			// host's forced keyframe is accepted without a reconnect.
			committed, err := s.framebuffer.ApplyPart(value)
			if err != nil {
				if errors.Is(err, ErrKeyframeRequired) {
					if sendErr := peer.Send(ctx, protocol.KeyframeRequest{Generation: s.framebuffer.Generation()}); sendErr != nil {
						return sendErr
					}
					continue
				}
				return err
			}
			if committed && s.renderer != nil {
				s.renderer.Present(s.framebuffer.Snapshot())
			}
		case protocol.Ping:
			if err := peer.Send(ctx, protocol.Pong{Nonce: value.Nonce}); err != nil {
				return err
			}
		case protocol.Pong:
		case protocol.ErrorMessage:
			return fmt.Errorf("client: server error %d: %s", value.Code, value.Diagnostic)
		case protocol.Close:
			// CLOSE is a terminal signal: the peer stops new messages,
			// closes TLS/TCP, and the session ends cleanly.
			s.notify(StateClosed, nil)
			return nil
		default:
			return fmt.Errorf("client: unexpected server message %T", message)
		}
	}
}

// keepalive tracks peer liveness for an established session (D7). The
// receive loop touches it on every message; the keepaliveLoop probes a peer
// that has sent nothing for HeartbeatInterval/2 with an unsolicited PING and
// closes the connection once that peer stays silent for HeartbeatTimeout past
// the probe, letting the Run loop reconnect. It mirrors the host's heartbeat
// watchdog, which treats any received message as proof of life.
type keepalive struct {
	mu          sync.Mutex
	lastRx      time.Time
	outstanding bool
}

func (k *keepalive) touch() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lastRx = time.Now()
	k.outstanding = false
}

func (s *Session) keepaliveLoop(ctx context.Context, peer *transport.Peer, live *keepalive) {
	half := s.config.HeartbeatInterval / 2
	if half <= 0 {
		half = s.config.HeartbeatInterval
	}
	ticker := time.NewTicker(half)
	defer ticker.Stop()
	var nonce uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			live.mu.Lock()
			idle := time.Since(live.lastRx)
			if idle >= s.config.HeartbeatTimeout && live.outstanding {
				live.mu.Unlock()
				// Watchdog: a probe went out and no PONG (or any other
				// message) came back within HeartbeatTimeout. Close so the
				// session reconnects instead of drifting on a dead peer.
				_ = peer.Close()
				return
			}
			if idle >= half {
				nonce++
				live.outstanding = true
				live.mu.Unlock()
				if err := peer.Send(ctx, protocol.Ping{Nonce: nonce}); err != nil {
					_ = peer.Close()
					return
				}
				continue
			}
			live.mu.Unlock()
		}
	}
}

func (s *Session) applyDisplay(config protocol.DisplayConfig) error {
	if err := s.framebuffer.Configure(config); err != nil {
		return err
	}
	s.input.SetDisplay(config.Generation, config.Width, config.Height)
	return nil
}
func (s *Session) notify(state ConnectionState, err error) {
	if s.observer != nil {
		s.observer.ConnectionState(state, err)
	}
}

type peerSender struct {
	mu   sync.RWMutex
	peer *transport.Peer
}

func (s *peerSender) set(peer *transport.Peer) { s.mu.Lock(); s.peer = peer; s.mu.Unlock() }
func (s *peerSender) Send(message protocol.Message) error {
	s.mu.RLock()
	peer := s.peer
	s.mu.RUnlock()
	if peer == nil {
		return ErrDisconnected
	}
	return peer.Send(context.Background(), message)
}
