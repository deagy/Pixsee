package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host"
	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

// --- test infrastructure -------------------------------------------------------

// testTLS generates a self-signed TLS 1.3 certificate for "localhost" and
// returns server and client configs that trust it.
func testTLS(t *testing.T) (serverTLS, clientTLS *tls.Config, caCert *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{caCert.Raw}, PrivateKey: key}},
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	clientTLS, err = transport.ClientTLSConfig("localhost", roots)
	if err != nil {
		t.Fatal(err)
	}
	return serverTLS, clientTLS, caCert
}

// fakeCapture serves a solid-color image whose color/size the test can change
// to trigger incremental frames. Capture copies the current image.
type fakeCapture struct {
	mu    sync.Mutex
	image damage.Image
	calls int
}

func (c *fakeCapture) setImage(w, h uint32, color byte) {
	p := solidPixels(w, h, color)
	c.mu.Lock()
	c.image = damage.Image{Width: w, Height: h, Pixels: p}
	c.mu.Unlock()
}

func (c *fakeCapture) Capture(context.Context, uint32) (damage.Image, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	im := c.image
	im.Pixels = append([]byte(nil), im.Pixels...)
	return im, nil
}

func (c *fakeCapture) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// recordingInput captures the events the host injects into the platform.
type recordingInput struct {
	mu       sync.Mutex
	calls    []string
	releases int
}

func (i *recordingInput) Key(context.Context, uint16, protocol.Action, uint8) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, "key")
	return nil
}
func (i *recordingInput) Move(context.Context, uint32, uint32) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, "move")
	return nil
}
func (i *recordingInput) Button(context.Context, protocol.Button, protocol.Action) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, "button")
	return nil
}
func (i *recordingInput) Wheel(context.Context, int16, int16) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, "wheel")
	return nil
}
func (i *recordingInput) ReleaseAll(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.releases++
	return nil
}
func (i *recordingInput) snapshot() (int, int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.calls), i.releases
}

// messageLog records every message that crosses the host's transport.Peer, in
// both directions, so tests can assert the host only sends session/control
// metadata and visual updates while the client only sends session/control
// metadata and input events.
type messageLog struct {
	mu     sync.Mutex
	hostOut []protocol.Message // host -> client (peer.Send)
	// hostIn records what the host received, i.e. what the client sent.
	hostIn []protocol.Message
}

func (m *messageLog) addOut(mes protocol.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostOut = append(m.hostOut, mes)
}
func (m *messageLog) addIn(mes protocol.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostIn = append(m.hostIn, mes)
}
func (m *messageLog) kinds(out []protocol.Message) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out2 := append([]protocol.Message(nil), out...)
	out = out2
	counts := map[string]int{}
	for _, msg := range out {
		counts[msg.Type().String()]++
	}
	return counts
}
func (m *messageLog) hostKinds() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return countKinds(m.hostOut)
}
func (m *messageLog) clientKinds() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return countKinds(m.hostIn)
}

func countKinds(msgs []protocol.Message) map[string]int {
	counts := map[string]int{}
	for _, msg := range msgs {
		counts[msg.Type().String()]++
	}
	return counts
}

// recordingPeer wraps a *transport.Peer to log every message that crosses it,
// in both directions. Because the host's peer is the single point through which
// both the host's and the client's traffic flow, instrumenting it captures the
// complete wire picture for both sides.
type recordingPeer struct {
	*transport.Peer
	log *messageLog
}

func (p recordingPeer) Send(ctx context.Context, m protocol.Message) error {
	if err := p.Peer.Send(ctx, m); err != nil {
		return err
	}
	p.log.addOut(m)
	return nil
}
func (p recordingPeer) Receive(ctx context.Context) (protocol.Message, error) {
	m, err := p.Peer.Receive(ctx)
	if err != nil {
		return m, err
	}
	p.log.addIn(m)
	return m, nil
}

func solidPixels(w, h uint32, color byte) []byte {
	p := make([]byte, w*h*4)
	for i := 0; i < len(p); i += 4 {
		p[i], p[i+1], p[i+2], p[i+3] = color, color, color, 0xff
	}
	return p
}

// hostService runs a real host.Service against a single client connection. It
// performs the handshake, authentication, and hello exchange that the service
// itself does not, then runs the service. It returns the service, the
// recording peer, and a channel that receives the service's exit error.
func hostService(ctx context.Context, conn net.Conn, serverTLS *tls.Config, token [32]byte, cap *fakeCapture, input *recordingInput, log *messageLog) (*host.Service, *recordingPeer, <-chan error) {
	svc := host.NewService(host.Config{
		CaptureInterval:         time.Millisecond,
		KeyframeInterval:        time.Hour,
		MaxInputEventsPerSecond: 1000,
		EnableInput:             true,
	}, cap, input)
	serverConn := tls.Server(conn, serverTLS.Clone())
	if err := transport.Handshake(ctx, serverConn, 5*time.Second); err != nil {
		errc := make(chan error, 1)
		errc <- err
		return svc, nil, errc
	}
	peer := recordingPeer{Peer: transport.NewPeerConn(serverConn, conn, protocol.RoleHost, protocol.DefaultLimits(), 5*time.Second), log: log}
	if err := peer.AuthenticateHost(ctx, token); err != nil {
		errc := make(chan error, 1)
		errc <- err
		return svc, nil, errc
	}
	// Hello exchange: receive ClientHello, then send ServerHello.
	if _, err := peer.Receive(ctx); err != nil {
		errc := make(chan error, 1)
		errc <- err
		return svc, nil, errc
	}
	if err := peer.Send(ctx, protocol.ServerHello{Version: protocol.Version1}); err != nil {
		errc := make(chan error, 1)
		errc <- err
		return svc, nil, errc
	}
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		defer cancel()
		errc <- svc.Run(runCtx, &peer)
	}()
	return svc, &peer, errc
}
