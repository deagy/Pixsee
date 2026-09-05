package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"virtualdesktop/internal/protocol"
	"virtualdesktop/internal/transport"
)

func TestDbgServeConnReturn(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)
	serverRaw, clientRaw := net.Pipe()
	go func() {
		serverConn := tls.Server(serverRaw, serverTLS)
		if err := transport.Handshake(context.Background(), serverConn, 2*time.Second); err != nil {
			fmt.Printf("DBG: server handshake: %v\n", err)
			return
		}
		peer := transport.NewPeer(serverConn, protocol.RoleHost, protocol.DefaultLimits(), 2*time.Second)
		var token [32]byte
		token[0] = 1
		peer.AuthenticateHost(context.Background(), token)
		peer.Receive(context.Background())
		peer.Send(context.Background(), protocol.ServerHello{Version: 1})
		peer.Send(context.Background(), protocol.DisplayConfig{Generation: 1, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888})
		fmt.Println("DBG: server sending CLOSE")
		peer.Send(context.Background(), protocol.Close{Code: protocol.CloseNormal})
		fmt.Println("DBG: server sent CLOSE, now returning (idle)")
	}()
	var token [32]byte
	token[0] = 1
	session, _ := NewSession(Config{Token: token, TLSConfig: clientTLS, IOTimeout: 2 * time.Second}, nil, nil)
	done := make(chan error, 1)
	go func() {
		fmt.Println("DBG: client entering ServeConn")
		start := time.Now()
		// Pass the RAW conn so ServeConn wraps it and can close the raw socket.
		err := session.ServeConn(context.Background(), clientRaw)
		fmt.Printf("DBG: ServeConn returned in %s err=%v\n", time.Since(start), err)
		done <- err
	}()
	select {
	case err := <-done:
		t.Logf("got err: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("TIMEOUT - ServeConn did not return")
	}
}
