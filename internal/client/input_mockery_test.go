package client_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"virtualdesktop/internal/client"
	"virtualdesktop/internal/client/mocks"
	"virtualdesktop/internal/protocol"
)

// This file exercises InputState against a mockery-generated MockInputSender,
// using testify assert/require for both happy-path delivery and error
// propagation from the sender. It lives in the client_test package (rather
// than client) because internal/client/mocks also contains mocks for
// interfaces that reference client types (Renderer, StateObserver); importing
// mocks from within package client itself would be an import cycle.

// TestInputStateKeySendsWhenActiveAndFocused proves a key event is delivered
// to the sender once the state is connected, focused, and has a display.
func TestInputStateKeySendsWhenActiveAndFocused(t *testing.T) {
	sender := mocks.NewMockInputSender(t)
	sender.EXPECT().Send(mock.AnythingOfType("protocol.Key")).Return(nil).Once()

	s := client.NewInputState(sender)
	s.SetDisplay(1, 10, 10)
	require.NoError(t, s.SetFocused(true))

	err := s.Key(0x04, protocol.ActionDown, 0)
	require.NoError(t, err)
}

// TestInputStateKeyDoesNotSendWhenNotFocused proves no input reaches the
// sender before the state becomes focused (the zero-value InputState is
// inactive).
func TestInputStateKeyDoesNotSendWhenNotFocused(t *testing.T) {
	sender := mocks.NewMockInputSender(t)

	s := client.NewInputState(sender)
	s.SetDisplay(1, 10, 10)

	err := s.Key(0x04, protocol.ActionDown, 0)
	require.NoError(t, err)
	sender.AssertNotCalled(t, "Send", mock.Anything)
}

// TestInputStatePropagatesSenderError proves a Send failure from the sender
// is returned by Key.
func TestInputStatePropagatesSenderError(t *testing.T) {
	sender := mocks.NewMockInputSender(t)
	boom := assert.AnError
	sender.EXPECT().Send(mock.Anything).Return(boom)

	s := client.NewInputState(sender)
	s.SetDisplay(1, 10, 10)
	require.NoError(t, s.SetFocused(true))

	err := s.Key(0x04, protocol.ActionDown, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

// TestInputStateReleaseAllSendsKeyUpForHeldKeys proves Disconnect releases
// every held key via the sender in ascending usage order.
func TestInputStateReleaseAllSendsKeyUpForHeldKeys(t *testing.T) {
	sender := mocks.NewMockInputSender(t)
	var sentUsages []uint16
	sender.EXPECT().Send(mock.AnythingOfType("protocol.Key")).Run(func(m protocol.Message) {
		k := m.(protocol.Key)
		sentUsages = append(sentUsages, k.Usage)
	}).Return(nil)

	s := client.NewInputState(sender)
	s.SetDisplay(1, 10, 10)
	require.NoError(t, s.SetFocused(true))
	require.NoError(t, s.Key(5, protocol.ActionDown, 0))
	require.NoError(t, s.Key(6, protocol.ActionDown, 0))

	require.NoError(t, s.Disconnect())
	// The two down-key sends plus two release-all up-key sends (ascending
	// usage order: 5 then 6).
	assert.Equal(t, []uint16{5, 6, 5, 6}, sentUsages)
}
