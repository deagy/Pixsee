package client

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// TestDbgCloseTiming measures how long tls.Conn.Close() blocks when the peer
// never reads, and whether closing the underlying socket returns immediately.
func TestDbgCloseTiming(t *testing.T) {
	serverTLS, clientTLS := sessionTLSConfigs(t)

	// Case 1: peer never closes -> tls.Conn.Close() should block ~5s (close_notify).
	{
		a, b := net.Pipe()
		tc := tls.Client(a, clientTLS)
		ts := tls.Server(b, serverTLS)
		errs := make(chan error, 2)
		go func() { errs <- tc.HandshakeContext(context.Background()) }()
		go func() { errs <- ts.HandshakeContext(context.Background()) }()
		<-errs
		<-errs
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- tc.Close() }()
		select {
		case <-done:
			t.Logf("tls.Conn.Close() (peer idle) returned in %s", time.Since(start))
		case <-time.After(6 * time.Second):
			t.Logf("tls.Conn.Close() (peer idle) BLOCKED >6s")
		}
	}

	// Case 2: peer closes first -> client's tls.Conn.Close() should return fast.
	{
		a, b := net.Pipe()
		tc := tls.Client(a, clientTLS)
		ts := tls.Server(b, serverTLS)
		errs := make(chan error, 2)
		go func() { errs <- tc.HandshakeContext(context.Background()) }()
		go func() { errs <- ts.HandshakeContext(context.Background()) }()
		<-errs
		<-errs
		go func() { time.Sleep(50 * time.Millisecond); _ = ts.Close() }()
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- tc.Close() }()
		select {
		case <-done:
			t.Logf("tls.Conn.Close() (peer closed first) returned in %s", time.Since(start))
		case <-time.After(6 * time.Second):
			t.Logf("tls.Conn.Close() (peer closed first) BLOCKED >6s")
		}
	}
}
