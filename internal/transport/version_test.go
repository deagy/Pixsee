package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
)

// TestPeerNegotiatesAndPinsV2 drives a full HELLO exchange over TLS peers: the
// decoder window accepts v1/v2 while the negotiation is open, both sides pin to
// the negotiated v2, and v2-enveloped application messages round-trip with
// record-sequence continuity across the pin.
func TestPeerNegotiatesAndPinsV2(t *testing.T) {
	server, client := testTLSPeers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var token [32]byte
	token[0] = 1

	hostResult := make(chan error, 1)
	go func() { hostResult <- server.AuthenticateHost(ctx, token) }()
	if err := client.AuthenticateClient(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}

	// Both sides accept either envelope version until SERVER_HELLO pins it.
	server.SetVersionWindow(protocol.Version1, protocol.Version2)
	client.SetVersionWindow(protocol.Version1, protocol.Version2)

	helloResult := make(chan error, 1)
	go func() {
		message, err := server.Receive(ctx)
		if err == nil {
			hello, ok := message.(protocol.ClientHello)
			if !ok || hello.MinVersion != protocol.Version1 || hello.MaxVersion != protocol.Version2 {
				err = errors.New("expected CLIENT_HELLO {1,2}")
			}
		}
		helloResult <- err
	}()
	if err := client.Send(ctx, protocol.ClientHello{MinVersion: protocol.Version1, MaxVersion: protocol.Version2}); err != nil {
		t.Fatal(err)
	}
	if err := <-helloResult; err != nil {
		t.Fatal(err)
	}

	// Server pins to v2, then sends SERVER_HELLO on a v2 envelope.
	server.PinVersion(protocol.Version2)
	serverSend := make(chan error, 1)
	go func() { serverSend <- server.Send(ctx, protocol.ServerHello{Version: protocol.Version2}) }()
	message, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hello, ok := message.(protocol.ServerHello); !ok || hello.Version != protocol.Version2 {
		t.Fatalf("expected SERVER_HELLO v2, got %#v", message)
	}
	if err := <-serverSend; err != nil {
		t.Fatal(err)
	}
	client.PinVersion(protocol.Version2)

	// DISPLAY_CONFIG carries both sides into Active on the v2 envelope.
	displayResult := make(chan error, 1)
	go func() {
		displayResult <- server.Send(ctx, protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888})
	}()
	if _, err := client.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-displayResult; err != nil {
		t.Fatal(err)
	}

	// A client message on the pinned v2 envelope reaches the server, proving
	// the client encoder was pinned and the server decoder sequence continued.
	inputResult := make(chan protocol.Message, 1)
	go func() {
		message, err := server.Receive(ctx)
		if err != nil {
			inputResult <- nil
			return
		}
		inputResult <- message
	}()
	if err := client.Send(ctx, protocol.Key{Generation: 1, InputSequence: 1, Usage: 4, Action: protocol.ActionDown}); err != nil {
		t.Fatal(err)
	}
	message = <-inputResult
	if _, ok := message.(protocol.Key); !ok {
		t.Fatalf("expected KEY over v2, got %#v", message)
	}
}
