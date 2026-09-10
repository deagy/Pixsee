package client_test

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/client/mocks"
)

// This file exercises Session.Run against mockery-generated MockRenderer and
// MockStateObserver, using testify assert/require. It lives in the
// client_test package to avoid an import cycle: internal/client/mocks
// imports internal/client for the Renderer/StateObserver method signatures.

func minimalTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

// TestSessionRunRequiresDialFunction proves Run rejects a Session configured
// without a Dial function up front, without ever touching the renderer or
// observer.
func TestSessionRunRequiresDialFunction(t *testing.T) {
	renderer := mocks.NewMockRenderer(t)
	observer := mocks.NewMockStateObserver(t)

	var token [32]byte
	token[0] = 1
	session, err := client.NewSession(client.Config{
		Token:     token,
		TLSConfig: minimalTLSConfig(),
	}, renderer, observer)
	require.NoError(t, err)

	err = session.Run(context.Background())
	assert.Error(t, err)
	renderer.AssertNotCalled(t, "Present", mock.Anything)
	observer.AssertNotCalled(t, "ConnectionState", mock.Anything, mock.Anything)
}

// TestSessionRunNotifiesObserverOnDialFailure proves a Dial failure notifies
// the observer with connection states and Run returns once the context is
// cancelled, without ever calling the renderer.
func TestSessionRunNotifiesObserverOnDialFailure(t *testing.T) {
	renderer := mocks.NewMockRenderer(t)
	observer := mocks.NewMockStateObserver(t)

	observer.EXPECT().ConnectionState(mock.Anything, mock.Anything).Return()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var token [32]byte
	token[0] = 1
	dialCalls := 0
	session, err := client.NewSession(client.Config{
		Token:          token,
		TLSConfig:      minimalTLSConfig(),
		ReconnectDelay: time.Millisecond,
		Dial: func(context.Context) (net.Conn, error) {
			dialCalls++
			if dialCalls >= 2 {
				cancel()
			}
			return nil, assert.AnError
		},
	}, renderer, observer)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- session.Run(ctx) }()

	require.Eventually(t, func() bool { return dialCalls >= 2 }, 2*time.Second, time.Millisecond)
	err = <-done
	assert.ErrorIs(t, err, context.Canceled)
	renderer.AssertNotCalled(t, "Present", mock.Anything)
}
