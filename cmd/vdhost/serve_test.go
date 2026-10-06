package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// trackingListener wraps a real net.Listener so a test can deterministically
// observe two things the shutdown path must guarantee: that Accept was
// actually reached (so cancellation races only with the blocking accept, not
// with startup), and that Close was called (so no listener is left behind).
type trackingListener struct {
	net.Listener

	acceptOnce sync.Once
	acceptCh   chan struct{}

	closeOnce sync.Once
	closed    bool
	mu        sync.Mutex
}

func newTrackingListener(t *testing.T) *trackingListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return &trackingListener{
		Listener: l,
		acceptCh: make(chan struct{}),
	}
}

func (l *trackingListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.acceptCh) })
	return l.Listener.Accept()
}

func (l *trackingListener) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		closeErr = l.Listener.Close()
	})
	return closeErr
}

func (l *trackingListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// TestServeReturnsAndClosesListenerOnContextCancel is the regression test for
// the "SIGINT leaves vdhost alive" bug: serve blocked in ln.Accept forever
// because canceling the context did not close the listener, so the process
// never returned and the still-open listener kept failing every later
// connection. It fails on the refactored-but-unfixed baseline by timing out
// (serve never returns), and passes once cancellation closes the listener.
func TestServeReturnsAndClosesListenerOnContextCancel(t *testing.T) {
	ln := newTrackingListener(t)
	cfg := &appConfig{
		listen: func(context.Context) (net.Listener, error) { return ln, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg) }()

	// Wait until serve is blocked in Accept before canceling, so this test
	// asserts the shutdown behavior rather than racing it against startup.
	select {
	case <-ln.acceptCh:
	case <-time.After(5 * time.Second):
		t.Fatal("serve never reached Accept")
	}

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "serve must return nil on context cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s of context cancellation: listener was not closed, so Accept stayed blocked")
	}

	require.True(t, ln.isClosed(), "listener must be closed after serve returns")
	_, err := net.DialTimeout("tcp", ln.Addr().String(), 500*time.Millisecond)
	require.Error(t, err, "a closed listener must refuse new connections (the observable SIGINT-aftermath symptom)")
}

// TestServeReturnsWhenContextCanceledBeforeAccept covers the other ordering:
// the context is already canceled before serve reaches Accept. The close
// callback must still make the accept path terminate promptly and cleanly
// instead of blocking or panicking on an already-closed listener.
func TestServeReturnsWhenContextCanceledBeforeAccept(t *testing.T) {
	ln := newTrackingListener(t)
	cfg := &appConfig{
		listen: func(context.Context) (net.Listener, error) { return ln, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // done before serve starts

	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg) }()

	select {
	case err := <-done:
		require.NoError(t, err, "serve must return nil when the context was canceled before Accept")
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s when the context was already canceled")
	}

	require.True(t, ln.isClosed(), "listener must be closed when the context was already canceled")
}

// TestServePropagatesListenError proves the listen-failure path still returns
// the wrapped error. A failed listen returns before the cancellation callback
// is registered, so there is no listener or callback to clean up.
func TestServePropagatesListenError(t *testing.T) {
	wantErr := &net.OpError{Op: "listen", Err: context.Canceled}
	cfg := &appConfig{
		listen: func(context.Context) (net.Listener, error) { return nil, wantErr },
	}
	err := serve(context.Background(), cfg)
	require.Error(t, err)
	require.ErrorIs(t, err, wantErr)
}
