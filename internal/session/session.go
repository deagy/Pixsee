// Package session owns establishing a host-side client session: the TLS
// accept, AUTH, the single-active-session (busy) admission, and the HELLO
// version negotiation. It is the ONE establishment implementation: the shipped
// host (cmd/vdhost) and the integration harness both drive it, so tests
// validate the session path that actually ships (finding F10 / spec D6 /
// AC-9).
//
// Accept deliberately stops at the SERVER_HELLO: running the display service
// (host.Service) and owning platform adapters stays with the caller, so this
// package depends only on transport/protocol. The startup-time authentication
// GATE (fail-closed token requirements, AC-6) is validated separately at
// config load in cmd/vdhost and is not this package's concern; Accept
// enforces whatever token the caller configured.
package session

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

// ErrBusy is returned by Accept when another client already holds the host's
// single active session slot. The rejected peer receives
// protocol.ErrorMessage{Code: protocol.ErrorBusy} on the wire before the
// connection is closed.
var ErrBusy = errors.New("session: another client already holds the session")

// Admissions enforces the host's single-active-session policy (docs §3, §5):
// exactly one established session at a time; a second concurrent connection
// is rejected as busy. The zero value is ready to use and safe for concurrent
// Accept calls.
type Admissions struct {
	mu   sync.Mutex
	open bool
}

// acquire claims the slot. A nil *Admissions imposes no policy and always
// admits, for the rare caller that wants no busy check.
func (a *Admissions) acquire() (release func(), ok bool) {
	if a == nil {
		return func() {}, true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.open {
		return nil, false
	}
	a.open = true
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.open = false
		})
	}, true
}

// Config carries the establishment inputs the shipped host wires from flags.
type Config struct {
	// Token is the 32-byte authentication material transport compares the
	// client's AUTH against in constant time. A zero token is the wire's
	// no-authentication mode; whether that mode may run at all is the
	// startup gate's decision (AC-6), not this package's.
	Token [32]byte
	// TLSConfig is the server TLS configuration (cloned per Accept).
	TLSConfig *tls.Config
	// Limits bounds the wire framing; zero selects protocol.DefaultLimits().
	Limits protocol.Limits
	// IOTimeout is the per-operation deadline for every establishment
	// operation (TLS handshake, AUTH, HELLO). Non-positive selects 10s,
	// matching the host's -timeout default. Steady-state deadlines are armed
	// later by host.Service.Run from the heartbeat config (D7).
	IOTimeout time.Duration
	// Admissions carries the single-active-session policy; nil admits
	// unconditionally.
	Admissions *Admissions
}

// Established is a successfully accepted session: the peer is authenticated,
// admitted, and version-negotiated, and the client has received its
// SERVER_HELLO. The caller runs the display service on Peer and MUST call
// Release when the session ends; the caller owns closing the connection.
type Established struct {
	Peer    *transport.Peer
	Version uint16
	// Release returns the admission slot; idempotent.
	Release func()
}

// Accept establishes one inbound session over conn. On any failure the
// connection is closed before returning, so callers treat errors as terminal
// for conn. The sequence is lifted verbatim from cmd/vdhost's former inline
// handleConnection (F10): TLS handshake → NewPeerConn → AuthenticateHost →
// busy admission → CLIENT_HELLO → NegotiateVersion → SERVER_HELLO.
func Accept(ctx context.Context, conn net.Conn, cfg Config) (*Established, error) {
	if conn == nil {
		return nil, errors.New("session: connection is required")
	}
	if cfg.TLSConfig == nil {
		conn.Close()
		return nil, errors.New("session: TLS configuration is required")
	}
	limits := cfg.Limits
	if limits == (protocol.Limits{}) {
		limits = protocol.DefaultLimits()
	}
	ioTimeout := cfg.IOTimeout
	if ioTimeout <= 0 {
		ioTimeout = 10 * time.Second
	}

	var err error
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	tlsConn := tls.Server(conn, cfg.TLSConfig.Clone())
	if err = transport.Handshake(ctx, tlsConn, ioTimeout); err != nil {
		return nil, fmt.Errorf("session: handshake: %w", err)
	}
	// Pass the raw socket so Peer.Close bypasses tls.Conn.Close's close_notify,
	// which would hang when the remote peer has stopped reading.
	peer := transport.NewPeerConn(tlsConn, conn, protocol.RoleHost, limits, ioTimeout)
	if err = peer.AuthenticateHost(ctx, cfg.Token); err != nil {
		return nil, fmt.Errorf("session: authentication failed: %w", err)
	}
	release, admitted := cfg.Admissions.acquire()
	if !admitted {
		// Reject with ERROR_BUSY and close before anything can enter the
		// service loop and leak a goroutine on the second connection.
		if sendErr := peer.Send(ctx, protocol.ErrorMessage{
			Code:       protocol.ErrorBusy,
			Diagnostic: "another client already holds the session",
		}); sendErr != nil {
			err = fmt.Errorf("session: busy: send error: %w", sendErr)
			return nil, err
		}
		err = ErrBusy
		return nil, err
	}
	established, helloErr := acceptHello(ctx, peer)
	if helloErr != nil {
		release()
		err = helloErr
		return nil, err
	}
	established.Release = release
	return established, nil
}

// acceptHello completes the version negotiation: CLIENT_HELLO in, SERVER_HELLO
// out. Without this exchange the peer's state machine stays in Negotiating
// and the service's first DISPLAY_CONFIG send is rejected as invalid-for-
// state, closing the connection; the client then times out waiting for
// SERVER_HELLO and reconnects forever with no display ever appearing. See
// docs/architecture.md §5 and §8.
func acceptHello(ctx context.Context, peer *transport.Peer) (*Established, error) {
	message, err := peer.Receive(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: hello: %w", err)
	}
	hello, ok := message.(protocol.ClientHello)
	if !ok {
		return nil, fmt.Errorf("session: hello: expected CLIENT_HELLO, got %T", message)
	}
	version, err := protocol.NegotiateVersion(hello.MinVersion, hello.MaxVersion, protocol.Version1, protocol.Version1)
	if err != nil {
		return nil, fmt.Errorf("session: version negotiation: %w", err)
	}
	if err := peer.Send(ctx, protocol.ServerHello{Version: version}); err != nil {
		return nil, fmt.Errorf("session: hello: %w", err)
	}
	return &Established{Peer: peer, Version: version}, nil
}
