package integration

// R2 AC-7 mechanism test: the SHA-256 value the host publishes for an
// ephemeral certificate (startup_announce in cmd/vdhost prints exactly this
// computation) is usable as a client pin: a client constructed with
// transport.ClientTLSConfigForFingerprint — the same constructor behind the
// -fingerprint flag — establishes a full session against the ephemeral host,
// while a client pinned with a different value is refused at handshake.
// No TLS semantics change; this pins the published-value contract.

import (
	"context"
	"crypto/sha256"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/transport"
)

func TestEphemeralFingerprintPinnedSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cert, err := transport.EphemeralServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := transport.ServerTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(cert.Certificate[0]) // exactly what the host announces
	clientTLS, err := transport.ClientTLSConfigForFingerprint(pin)
	if err != nil {
		t.Fatal(err)
	}

	var token [32]byte
	token[0] = 9

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		cap1 := &fakeCapture{}
		cap1.setImage(8, 8, 0x10)
		hostService(ctx, conn, serverTLS, token, cap1, &recordingInput{}, &messageLog{})
	}()

	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, done := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 10*time.Second, "keyframe over the fingerprint-pinned ephemeral session", func() bool {
		return renderer.count() >= 1
	})
	if !observer.contains(client.StateConnected) {
		t.Fatalf("pinned client never reached Connected; states=%v", observer.states)
	}
	if state, ok := stateAfterConnected(observer); ok {
		t.Fatalf("fingerprint-pinned session regressed to %v", state)
	}
	cancel()
	<-done
}

func TestEphemeralWrongFingerprintRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cert, err := transport.EphemeralServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := transport.ServerTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}

	var token [32]byte
	token[0] = 9

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		cap1 := &fakeCapture{}
		cap1.setImage(8, 8, 0x10)
		hostService(ctx, conn, serverTLS, token, cap1, &recordingInput{}, &messageLog{})
	}()

	badPin := sha256.Sum256([]byte("a different certificate"))
	clientTLS, err := transport.ClientTLSConfigForFingerprint(badPin)
	if err != nil {
		t.Fatal(err)
	}
	renderer := &snapshotRenderer{}
	observer := &stateRecorder{}
	sess, _ := newClient(t, ctx, clientTLS, token, ln.Addr().String(), renderer, observer)
	defer sess.Input().Disconnect()

	waitFor(t, 10*time.Second, "pin-mismatch error state", func() bool {
		return observer.contains(client.StateError)
	})
	if observer.contains(client.StateConnected) || renderer.count() > 0 {
		t.Fatal("client connected despite a certificate fingerprint mismatch")
	}
	cancel()
}
