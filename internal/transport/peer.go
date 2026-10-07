package transport

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"virtualdesktop/internal/protocol"
)

var (
	ErrAuthentication = errors.New("authentication failed")
	ErrTLSRequired    = errors.New("TLS connection is required")
	ErrClosed         = errors.New("transport closed")
)

// HeartbeatSlack is the grace window added to HeartbeatInterval +
// HeartbeatTimeout when deriving the steady-state (Active) read deadline.
// Establishment reads keep the peer's IOTimeout; once a session is Active the
// heartbeat watchdog owns dead-peer detection, and this slack keeps a healthy
// idle peer's read from expiring before the watchdog can act.
const HeartbeatSlack = 5 * time.Second

type Peer struct {
	conn              net.Conn
	rawConn           net.Conn
	role              protocol.Role
	encoder           *protocol.Encoder
	decoder           *protocol.Decoder
	timeout           time.Duration
	steadyReadTimeout time.Duration // guarded by stateMu; 0 disables the switch
	sendMu            sync.Mutex
	receiveMu         sync.Mutex
	stateMu           sync.Mutex
	state             protocol.State
	clientHelloSeen   bool
	serverHelloSeen   bool
	close             sync.Once
}

func NewPeer(conn net.Conn, role protocol.Role, limits protocol.Limits, timeout time.Duration) *Peer {
	return NewPeerConn(conn, conn, role, limits, timeout)
}

// NewPeerConn builds a Peer that reads and writes through conn and closes the
// underlying socket directly on Close. For a TLS connection the caller passes
// the *tls.Conn as conn and the wrapped net.Conn as rawConn; closing the raw
// socket directly avoids the graceful close_notify alert that tls.Conn.Close
// writes, which can block forever when the remote peer has stopped reading
// (for example on an unbuffered net.Pipe).
func NewPeerConn(conn, rawConn net.Conn, role protocol.Role, limits protocol.Limits, timeout time.Duration) *Peer {
	return &Peer{
		conn:    conn,
		rawConn: rawConn,
		role:    role,
		encoder: protocol.NewEncoder(conn, limits),
		decoder: protocol.NewDecoder(conn, limits),
		timeout: timeout,
		state:   protocol.StateAuthenticating,
	}
}

func (p *Peer) State() protocol.State {
	if p == nil {
		return protocol.StateClosed
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.state
}

// SetSteadyReadTimeout arms the heartbeat-derived read deadline that Receive
// switches to once the session reaches the Active state. Reads before Active
// (TLS handshake, AUTH, HELLO, the first DISPLAY_CONFIG) keep the peer's
// establishment timeout. A non-positive d disables the switch.
func (p *Peer) SetSteadyReadTimeout(d time.Duration) {
	if p == nil {
		return
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.steadyReadTimeout = d
}

// readTimeout picks the regime for the next read: the steady-state deadline
// once the session is Active, otherwise the establishment IOTimeout.
func (p *Peer) readTimeout() time.Duration {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.steadyReadTimeout > 0 && p.state == protocol.StateActive {
		return p.steadyReadTimeout
	}
	return p.timeout
}

// SetVersionWindow widens the decoder's accepted record-envelope versions to
// the inclusive range [min,max] for the HELLO exchange. A v2-capable peer
// calls SetVersionWindow(protocol.Version1, protocol.Version2) before the
// exchange; the default is v1-only, preserving v1 byte behavior. The encoder
// is unaffected until PinVersion.
func (p *Peer) SetVersionWindow(min, max uint16) {
	if p == nil {
		return
	}
	p.receiveMu.Lock()
	defer p.receiveMu.Unlock()
	p.decoder.SetVersionWindow(min, max)
}

// PinVersion pins both the encoder and the decoder to v once SERVER_HELLO
// fixes the negotiated version. It never resets the decoder's strict
// record-sequence counter, so continuity is enforced across the transition.
func (p *Peer) PinVersion(v uint16) {
	if p == nil {
		return
	}
	p.sendMu.Lock()
	p.encoder.PinVersion(v)
	p.sendMu.Unlock()
	p.receiveMu.Lock()
	p.decoder.PinVersion(v)
	p.receiveMu.Unlock()
}

func (p *Peer) Send(ctx context.Context, message protocol.Message) error {
	if p == nil || p.conn == nil {
		return ErrClosed
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if err := protocol.ValidateDirection(p.role, protocol.Outgoing, message); err != nil {
		return err
	}
	if err := p.validateState(message, protocol.Outgoing); err != nil {
		return err
	}
	if err := p.withWriteDeadline(ctx, func() error { return p.encoder.Encode(message) }); err != nil {
		p.Close()
		return fmt.Errorf("send %s: %w", message.Type(), err)
	}
	p.advance(message, protocol.Outgoing)
	return nil
}

func (p *Peer) Receive(ctx context.Context) (protocol.Message, error) {
	if p == nil || p.conn == nil {
		return nil, ErrClosed
	}
	p.receiveMu.Lock()
	defer p.receiveMu.Unlock()
	var message protocol.Message
	err := p.withReadDeadline(ctx, func() error {
		var err error
		message, err = p.decoder.Decode()
		return err
	})
	if err == nil {
		err = protocol.ValidateDirection(p.role, protocol.Incoming, message)
	}
	if err == nil {
		err = p.validateState(message, protocol.Incoming)
	}
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("receive: %w", err)
	}
	p.advance(message, protocol.Incoming)
	return message, nil
}

func (p *Peer) AuthenticateClient(ctx context.Context, token [32]byte) error {
	if p == nil || p.role != protocol.RoleClient {
		return ErrAuthentication
	}
	if !p.usesTLS() {
		p.Close()
		return ErrTLSRequired
	}
	return p.Send(ctx, protocol.Auth{Version: protocol.Version1, Token: token})
}

func (p *Peer) AuthenticateHost(ctx context.Context, expected [32]byte) error {
	if p == nil || p.role != protocol.RoleHost {
		return ErrAuthentication
	}
	if !p.usesTLS() {
		p.Close()
		return ErrTLSRequired
	}
	message, err := p.Receive(ctx)
	if err != nil {
		return ErrAuthentication
	}
	auth, ok := message.(protocol.Auth)
	if !ok || auth.Version != protocol.Version1 {
		p.Close()
		return ErrAuthentication
	}
	var zero [32]byte
	if auth.Token == zero {
		// The wire invariant forbids a zero token, but reject it explicitly.
		p.Close()
		return ErrAuthentication
	}
	if subtle.ConstantTimeCompare(expected[:], zero[:]) == 1 {
		// No token configured on the host: accept any non-zero client token.
		// This is no-authentication mode, not weak authentication.
		return nil
	}
	if subtle.ConstantTimeCompare(auth.Token[:], expected[:]) != 1 {
		p.Close()
		return ErrAuthentication
	}
	return nil
}

func (p *Peer) validateState(message protocol.Message, _ protocol.Flow) error {
	if message != nil && message.Type() == protocol.TypeAuth && !p.usesTLS() {
		return ErrTLSRequired
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if err := protocol.ValidateState(p.state, message); err != nil {
		return err
	}
	if p.state != protocol.StateNegotiating {
		return nil
	}

	switch message.Type() {
	case protocol.TypeClientHello:
		if p.clientHelloSeen {
			return protocol.ErrState
		}
	case protocol.TypeServerHello:
		if !p.clientHelloSeen || p.serverHelloSeen {
			return protocol.ErrState
		}
	case protocol.TypeDisplayConfig:
		if !p.clientHelloSeen || !p.serverHelloSeen {
			return protocol.ErrState
		}
	case protocol.TypeError, protocol.TypeClose:
	default:
		return protocol.ErrState
	}
	return nil
}

func (p *Peer) advance(message protocol.Message, flow protocol.Flow) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	switch message.Type() {
	case protocol.TypeAuth:
		if (p.role == protocol.RoleClient && flow == protocol.Outgoing) || (p.role == protocol.RoleHost && flow == protocol.Incoming) {
			p.state = protocol.StateNegotiating
		}
	case protocol.TypeClientHello:
		p.clientHelloSeen = true
	case protocol.TypeServerHello:
		p.serverHelloSeen = true
	case protocol.TypeDisplayConfig:
		if (p.role == protocol.RoleHost && flow == protocol.Outgoing) || (p.role == protocol.RoleClient && flow == protocol.Incoming) {
			p.state = protocol.StateActive
		}
	case protocol.TypeClose:
		p.state = protocol.StateClosing
	}
}

func (p *Peer) usesTLS() bool {
	_, ok := p.conn.(*tls.Conn)
	return ok
}

func (p *Peer) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	var err error
	p.close.Do(func() {
		p.stateMu.Lock()
		p.state = protocol.StateClosed
		p.stateMu.Unlock()
		// Close the underlying socket directly rather than via
		// tls.Conn.Close. The graceful close_notify alert that
		// tls.Conn.Close writes can block forever when the remote peer
		// has stopped reading, which would hang this cleanup path.
		err = p.rawConn.Close()
	})
	return err
}

func (p *Peer) withReadDeadline(ctx context.Context, operation func() error) error {
	deadline, stop, err := operationDeadline(ctx, p.readTimeout(), p.conn.SetReadDeadline)
	if err != nil {
		return err
	}
	defer stop()
	defer p.conn.SetReadDeadline(time.Time{})
	err = operation()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() && time.Now().After(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

func (p *Peer) withWriteDeadline(ctx context.Context, operation func() error) error {
	deadline, stop, err := operationDeadline(ctx, p.timeout, p.conn.SetWriteDeadline)
	if err != nil {
		return err
	}
	defer stop()
	defer p.conn.SetWriteDeadline(time.Time{})
	err = operation()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() && time.Now().After(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

func operationDeadline(ctx context.Context, timeout time.Duration, setDeadline func(time.Time) error) (time.Time, func(), error) {
	if ctx == nil {
		return time.Time{}, nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, nil, err
	}
	if timeout <= 0 {
		return time.Time{}, nil, errors.New("timeout must be positive")
	}
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := setDeadline(deadline); err != nil {
		return time.Time{}, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = setDeadline(time.Now()) })
	return deadline, func() { stop() }, nil
}
