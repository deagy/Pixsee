package client

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

func TestSessionAuthenticatesRendersResizeAndForwardsInput(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	serverConn := tls.Server(serverRaw, serverTLS)
	clientConn := tls.Client(clientRaw, clientTLS)
	var token [32]byte
	token[0] = 9
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hostResult := make(chan error, 1)
	inputResult := make(chan protocol.Message, 1)
	go func() {
		if err := transport.Handshake(ctx, serverConn, time.Second); err != nil {
			hostResult <- err
			return
		}
		peer := transport.NewPeer(serverConn, protocol.RoleHost, protocol.DefaultLimits(), time.Second)
		if err := peer.AuthenticateHost(ctx, token); err != nil {
			hostResult <- err
			return
		}
		if _, err := peer.Receive(ctx); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.ServerHello{Version: 1}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 2, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []protocol.Rectangle{{Width: 2, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(2, 1)}}}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.DisplayConfig{Generation: 2, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			hostResult <- err
			return
		}
		if err := peer.Send(ctx, protocol.Frame{Generation: 2, FrameSequence: 2, Keyframe: true, Rectangles: []protocol.Rectangle{{Width: 1, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(1, 8)}}}); err != nil {
			hostResult <- err
			return
		}
		message, err := peer.Receive(ctx)
		if err != nil {
			hostResult <- err
			return
		}
		inputResult <- message
		hostResult <- peer.Send(ctx, protocol.Close{Code: protocol.CloseNormal})
	}()

	renderer := &recordingRenderer{presented: make(chan Snapshot, 2)}
	states := &recordingObserver{}
	session, err := NewSession(Config{Token: token, TLSConfig: clientTLS, IOTimeout: time.Second}, renderer, states)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.ServeConn(ctx, clientConn) }()
	first := <-renderer.presented
	second := <-renderer.presented
	if first.Width != 2 || second.Width != 1 || second.Generation != 2 {
		t.Fatalf("snapshots: %#v %#v", first, second)
	}
	session.Input().SetFocused(true)
	if err := session.Input().Key(4, protocol.ActionDown, 0); err != nil {
		t.Fatal(err)
	}
	if got := (<-inputResult).(protocol.Key); got.Generation != 2 || got.Usage != 4 {
		t.Fatalf("input %#v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
	if !states.contains(StateConnected) || !states.contains(StateClosed) {
		t.Fatalf("states %v", states.states)
	}
}

func TestRunReturnsOnCleanServerClose(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	go func() {
		serverConn := tls.Server(serverRaw, serverTLS)
		if err := transport.Handshake(context.Background(), serverConn, 2*time.Second); err != nil {
			t.Logf("server handshake: %v", err)
			return
		}
		t.Logf("server handshake ok")
		peer := transport.NewPeer(serverConn, protocol.RoleHost, protocol.DefaultLimits(), 2*time.Second)
		var token [32]byte
		token[0] = 1
		if err := peer.AuthenticateHost(context.Background(), token); err != nil {
			t.Logf("server auth: %v", err)
			return
		}
		t.Logf("server auth ok")
		if _, err := peer.Receive(context.Background()); err != nil {
			t.Logf("server receive: %v", err)
			return
		}
		t.Logf("server receive ok")
		if err := peer.Send(context.Background(), protocol.ServerHello{Version: 1}); err != nil {
			t.Logf("server send hello: %v", err)
			return
		}
		if err := peer.Send(context.Background(), protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			t.Logf("server send displayconfig: %v", err)
			return
		}
		t.Logf("server sent hello+displayconfig")
		if err := peer.Send(context.Background(), protocol.Close{Code: protocol.CloseNormal}); err != nil {
			t.Logf("server send close: %v", err)
			return
		}
		t.Logf("server sent close")
	}()

	var token [32]byte
	token[0] = 1
	session, err := NewSession(Config{Token: token, TLSConfig: clientTLS, IOTimeout: 2 * time.Second}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		t.Logf("client starting ServeConn")
		err := session.ServeConn(context.Background(), clientRaw)
		t.Logf("ServeConn returned: %v", err)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on clean close, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeConn did not return after clean server CLOSE")
	}
}

func TestRunReconnectsAfterTransientFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	want := errors.New("dial failed")
	session, err := NewSession(Config{Token: [32]byte{1}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}, ReconnectDelay: time.Millisecond, Dial: func(context.Context) (net.Conn, error) {
		attempts++
		if attempts == 2 {
			cancel()
		}
		return nil, want
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d", attempts)
	}
}

func TestSessionRejectsUnexpectedServerMessage(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	serverConn, clientConn := tls.Server(serverRaw, serverTLS), tls.Client(clientRaw, clientTLS)
	var token [32]byte
	token[0] = 1
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		_ = transport.Handshake(ctx, serverConn, time.Second)
		peer := transport.NewPeer(serverConn, protocol.RoleHost, protocol.DefaultLimits(), time.Second)
		_ = peer.AuthenticateHost(ctx, token)
		_, _ = peer.Receive(ctx)
		_ = peer.Send(ctx, protocol.ServerHello{Version: 1})
		_ = peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888})
		_ = peer.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888})
	}()
	session, _ := NewSession(Config{Token: token, TLSConfig: clientTLS, IOTimeout: time.Second}, nil, nil)
	if err := session.ServeConn(ctx, clientConn); err == nil {
		t.Fatal("accepted stale display config")
	}
}

type recordingRenderer struct{ presented chan Snapshot }

func (r *recordingRenderer) Present(s Snapshot) { r.presented <- s }

type recordingObserver struct {
	mu     sync.Mutex
	states []ConnectionState
}

func (o *recordingObserver) ConnectionState(s ConnectionState, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, s)
}
func (o *recordingObserver) contains(s ConnectionState) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, v := range o.states {
		if v == s {
			return true
		}
	}
	return false
}

func sessionTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	server, _ := transport.ServerTLSConfig(cert)
	client, _ := transport.ClientTLSConfigForCertificate("localhost", leaf)
	return server, client
}

var _ = reflect.DeepEqual
