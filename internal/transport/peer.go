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

type Peer struct {
	conn            net.Conn
	rawConn         net.Conn
	role            protocol.Role
	encoder         *protocol.Encoder
	decoder         *protocol.Decoder
	timeout         time.Duration
	sendMu          sync.Mutex
	receiveMu       sync.Mutex
	stateMu         sync.Mutex
	state           protocol.State
	clientHelloSeen bool
	serverHelloSeen bool
	close           sync.Once
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
	if !ok || auth.Version != protocol.Version1 || subtle.ConstantTimeCompare(auth.Token[:], expected[:]) != 1 {
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
	deadline, stop, err := operationDeadline(ctx, p.timeout, p.conn.SetReadDeadline)
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
