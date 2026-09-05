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
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			s.notify(StateClosed, nil)
			return err
		}
		if attempt == 0 {
			s.notify(StateConnecting, nil)
		} else {
			s.notify(StateReconnecting, nil)
		}
		conn, err := s.config.Dial(ctx)
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
	s.sender.set(peer)
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
	if err := peer.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version1}); err != nil {
		return err
	}
	message, err := peer.Receive(ctx)
	if err != nil {
		return err
	}
	hello, ok := message.(protocol.ServerHello)
	if !ok {
		return fmt.Errorf("client: expected SERVER_HELLO, got %T", message)
	}
	if _, err := protocol.NegotiateVersion(protocol.Version1, protocol.Version1, hello.Version, hello.Version); err != nil {
		return err
	}
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
	s.notify(StateConnected, nil)

	for {
		message, err = peer.Receive(ctx)
		if err != nil {
			return err
		}
		switch value := message.(type) {
		case protocol.DisplayConfig:
			if err := s.applyDisplay(value); err != nil {
				return err
			}
		case protocol.Frame:
			if err := s.framebuffer.Apply(value); err != nil {
				if errors.Is(err, ErrKeyframeRequired) {
					if sendErr := peer.Send(ctx, protocol.KeyframeRequest{Generation: s.framebuffer.Snapshot().Generation}); sendErr != nil {
						return sendErr
					}
					continue
				}
				return err
			}
			if s.renderer != nil {
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
