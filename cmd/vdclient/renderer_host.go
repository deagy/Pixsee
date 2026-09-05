package main

import (
	"fmt"
	"sync"

	"virtualdesktop/internal/client"
)

// rendererHost is the default, dependency-free renderer host. Without the
// "fyne" build tag there is no window to present pixels on, so it renders no
// image but still captures input and reports connection state. It exists so the
// client builds and runs on headless or non-GUI environments.
type rendererHost struct {
	title     string
	input     *client.InputState
	running   chan struct{}
	stopped   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newRendererHost(title string, input *client.InputState) *rendererHost {
	return &rendererHost{
		title:   title,
		input:   input,
		running: make(chan struct{}),
		stopped: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (h *rendererHost) Present(_ client.Snapshot) {}

func (h *rendererHost) ConnectionState(state client.ConnectionState, err error) {
	if err != nil {
		fmt.Printf("[%s] error: %s\n", h.title, err.Error())
		return
	}
	fmt.Printf("[%s] %s\n", h.title, connectionStateText(state))
}

func (h *rendererHost) Run() {
	close(h.running)
	<-h.closed
}

func (h *rendererHost) Quit() {
	h.closeOnce.Do(func() {
		close(h.closed)
	})
}

func connectionStateText(state client.ConnectionState) string {
	switch state {
	case client.StateConnecting:
		return "Connecting…"
	case client.StateAuthenticating:
		return "Authenticating…"
	case client.StateNegotiating:
		return "Negotiating…"
	case client.StateConnected:
		return "Connected"
	case client.StateReconnecting:
		return "Reconnecting…"
	case client.StateClosed:
		return "Closed"
	case client.StateError:
		return "Connection error"
	default:
		return "Disconnected"
	}
}
