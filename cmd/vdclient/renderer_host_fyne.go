//go:build fyne

package main

import (
	"sync"

	"virtualdesktop/internal/client"
)

// rendererHost is the Fyne-backed renderer host. It owns the Fyne window and
// event loop and exposes them through the same rendererHost interface used by
// the headless default.
type rendererHost struct {
	fyne      *client.FyneRenderer
	running   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newRendererHost(title string, input *client.InputState) *rendererHost {
	return &rendererHost{
		fyne:    client.NewFyneRenderer(title, input),
		running: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (h *rendererHost) Present(frame client.Snapshot) {
	h.fyne.Present(frame)
}

func (h *rendererHost) ConnectionState(state client.ConnectionState, err error) {
	h.fyne.ConnectionState(state, err)
}

func (h *rendererHost) Run() {
	h.fyne.Run()
}

func (h *rendererHost) Quit() {
	h.fyne.Quit()
}
