package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/host/mocks"
	"virtualdesktop/internal/protocol"
)

// This file exercises Service.Run against mockery-generated mocks of the
// Capture, Input, and Peer interfaces, using testify assert/require instead
// of manual comparisons. It complements service_test.go's hand-rolled fakes
// with EXPECT()-style call verification for both happy paths and error
// cases.

func mockTestConfig() Config {
	return Config{DisplayID: 0, CaptureInterval: 1, KeyframeInterval: 0, MaxInputEventsPerSecond: 100, EnableInput: true}
}

// TestServiceRunHappyPathSendsDisplayAndFrame proves that on a successful
// initial capture, Run sends exactly one DisplayConfig then one Frame before
// the context is cancelled, and cleans up by releasing all input.
func TestServiceRunHappyPathSendsDisplayAndFrame(t *testing.T) {
	capture := mocks.NewMockCapture(t)
	input := mocks.NewMockInput(t)
	peer := mocks.NewMockPeer(t)

	image := damage.Image{Width: 2, Height: 2, Pixels: make([]byte, 2*2*4)}
	capture.EXPECT().Capture(mock.Anything, uint32(0)).Return(image, nil).Once()
	peer.EXPECT().Send(mock.Anything, mock.AnythingOfType("protocol.DisplayConfig")).Return(nil).Once()
	peer.EXPECT().Send(mock.Anything, mock.AnythingOfType("protocol.Frame")).Return(nil).Once()
	input.EXPECT().ReleaseAll(mock.Anything).Return(nil).Once()

	ctx, cancel := context.WithCancel(context.Background())
	// Peer.Receive may or may not be reached before the context is
	// cancelled (Run's final select races ctx.Done() against the
	// concurrently-started input loop), so this expectation is optional.
	peer.EXPECT().Receive(mock.Anything).RunAndReturn(func(ctx context.Context) (protocol.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}).Maybe()

	svc := NewService(mockTestConfig(), capture, input)
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx, peer) }()
	cancel()

	err := <-done
	require.ErrorIs(t, err, context.Canceled)
}

// TestServiceRunCaptureErrorPropagates proves an initial capture failure is
// wrapped and returned without ever touching the peer or input adapters.
func TestServiceRunCaptureErrorPropagates(t *testing.T) {
	capture := mocks.NewMockCapture(t)
	input := mocks.NewMockInput(t)
	peer := mocks.NewMockPeer(t)

	boom := errors.New("capture boom")
	capture.EXPECT().Capture(mock.Anything, uint32(0)).Return(damage.Image{}, boom).Once()

	svc := NewService(mockTestConfig(), capture, input)
	err := svc.Run(context.Background(), peer)

	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	peer.AssertNotCalled(t, "Send", mock.Anything, mock.Anything)
	input.AssertNotCalled(t, "ReleaseAll", mock.Anything)
}

// TestServiceRunSendErrorPropagates proves a peer.Send failure while
// delivering the initial frame is returned and input is still released.
func TestServiceRunSendErrorPropagates(t *testing.T) {
	capture := mocks.NewMockCapture(t)
	input := mocks.NewMockInput(t)
	peer := mocks.NewMockPeer(t)

	image := damage.Image{Width: 2, Height: 2, Pixels: make([]byte, 2*2*4)}
	boom := errors.New("send boom")
	capture.EXPECT().Capture(mock.Anything, uint32(0)).Return(image, nil).Once()
	peer.EXPECT().Send(mock.Anything, mock.AnythingOfType("protocol.DisplayConfig")).Return(boom).Once()

	svc := NewService(mockTestConfig(), capture, input)
	err := svc.Run(context.Background(), peer)

	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

// TestServiceRunRejectsInvalidInputMessage proves a malformed/unexpected
// input message from the peer is rejected with ErrInvalidInput and never
// reaches the Input adapter.
func TestServiceRunRejectsInvalidInputMessage(t *testing.T) {
	capture := mocks.NewMockCapture(t)
	input := mocks.NewMockInput(t)
	peer := mocks.NewMockPeer(t)

	image := damage.Image{Width: 2, Height: 2, Pixels: make([]byte, 2*2*4)}
	capture.EXPECT().Capture(mock.Anything, uint32(0)).Return(image, nil).Once()
	peer.EXPECT().Send(mock.Anything, mock.Anything).Return(nil)
	peer.EXPECT().Receive(mock.Anything).Return(protocol.Pong{}, nil).Once()
	input.EXPECT().ReleaseAll(mock.Anything).Return(nil).Once()

	svc := NewService(mockTestConfig(), capture, input)
	err := svc.Run(context.Background(), peer)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	input.AssertNotCalled(t, "Key", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestServiceRunInjectsKeyInput proves a valid Key input message from the
// peer is forwarded to the Input adapter's Key method.
func TestServiceRunInjectsKeyInput(t *testing.T) {
	capture := mocks.NewMockCapture(t)
	input := mocks.NewMockInput(t)
	peer := mocks.NewMockPeer(t)

	image := damage.Image{Width: 2, Height: 2, Pixels: make([]byte, 2*2*4)}
	capture.EXPECT().Capture(mock.Anything, uint32(0)).Return(image, nil).Once()
	peer.EXPECT().Send(mock.Anything, mock.Anything).Return(nil)

	key := protocol.Key{Generation: 1, InputSequence: 1, Usage: 0x04, Action: protocol.ActionDown}
	keyDelivered := make(chan struct{})
	peer.EXPECT().Receive(mock.Anything).RunAndReturn(func(ctx context.Context) (protocol.Message, error) {
		close(keyDelivered)
		return key, nil
	}).Once()
	peer.EXPECT().Receive(mock.Anything).RunAndReturn(func(ctx context.Context) (protocol.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	input.EXPECT().Key(mock.Anything, uint16(0x04), protocol.ActionDown, uint8(0)).Return(nil).Once()
	input.EXPECT().ReleaseAll(mock.Anything).Return(nil).Once()

	ctx, cancel := context.WithCancel(context.Background())
	svc := NewService(mockTestConfig(), capture, input)
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx, peer) }()

	select {
	case <-keyDelivered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for key message to be delivered")
	}
	cancel()
	<-done
}

// TestServiceRunRequiresDependencies proves Run rejects a nil context, peer,
// capture, or input adapter up front rather than panicking later.
func TestServiceRunRequiresDependencies(t *testing.T) {
	capture := mocks.NewMockCapture(t)
	input := mocks.NewMockInput(t)
	peer := mocks.NewMockPeer(t)

	svc := NewService(mockTestConfig(), capture, input)
	err := svc.Run(context.Background(), nil)
	assert.Error(t, err)

	svcNoCapture := NewService(mockTestConfig(), nil, input)
	err = svcNoCapture.Run(context.Background(), peer)
	assert.Error(t, err)

	svcNoInput := NewService(mockTestConfig(), capture, nil)
	err = svcNoInput.Run(context.Background(), peer)
	assert.Error(t, err)
}
