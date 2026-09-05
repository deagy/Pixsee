//go:build fyne

package client

import (
	"image"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"virtualdesktop/internal/protocol"
)

// FyneRenderer presents immutable snapshots on Fyne's UI thread and captures
// only the protocol-approved keyboard and pointer interactions.
type FyneRenderer struct {
	app    fyne.App
	window fyne.Window
	view   *desktopView
	status *widget.Label
	input  *InputState
	mu     sync.Mutex
	closed bool
}

func NewFyneRenderer(title string, input *InputState) *FyneRenderer {
	a := app.NewWithID("com.virtualdesktop.client")
	w := a.NewWindow(title)
	status := widget.NewLabel("Disconnected")
	view := newDesktopView(input)
	w.SetContent(container.NewBorder(status, nil, nil, nil, view))
	w.Resize(fyne.NewSize(1280, 720))
	r := &FyneRenderer{app: a, window: w, view: view, status: status, input: input}
	w.SetCloseIntercept(func() { r.Quit() })
	a.Lifecycle().SetOnExitedForeground(func() { _ = input.SetFocused(false) })
	if keys, ok := w.Canvas().(desktop.Canvas); ok {
		keys.SetOnKeyDown(view.keyDown)
		keys.SetOnKeyUp(view.keyUp)
	}
	return r
}
func (r *FyneRenderer) Present(frame Snapshot) {
	fyne.Do(func() {
		pixels := make([]byte, len(frame.Pixels))
		for i := 0; i+3 < len(frame.Pixels); i += 4 {
			pixels[i] = frame.Pixels[i+2]
			pixels[i+1] = frame.Pixels[i+1]
			pixels[i+2] = frame.Pixels[i]
			pixels[i+3] = 0xff
		}
		r.view.image.Image = &image.NRGBA{Pix: pixels, Stride: int(frame.Width) * 4, Rect: image.Rect(0, 0, int(frame.Width), int(frame.Height))}
		r.view.image.Refresh()
	})
}
func (r *FyneRenderer) ConnectionState(state ConnectionState, err error) {
	fyne.Do(func() {
		if err != nil {
			r.status.SetText("Error: " + err.Error())
			return
		}
		r.status.SetText(connectionStateText(state))
	})
}
func connectionStateText(state ConnectionState) string {
	switch state {
	case StateConnecting:
		return "Connecting…"
	case StateAuthenticating:
		return "Authenticating…"
	case StateNegotiating:
		return "Negotiating…"
	case StateConnected:
		return "Connected"
	case StateReconnecting:
		return "Reconnecting…"
	case StateClosed:
		return "Closed"
	case StateError:
		return "Connection error"
	default:
		return "Disconnected"
	}
}
func (r *FyneRenderer) Run() { r.window.Show(); r.window.Canvas().Focus(r.view); r.app.Run() }
func (r *FyneRenderer) Quit() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	_ = r.input.Disconnect()
	fyne.Do(func() { r.window.SetCloseIntercept(nil); r.app.Quit() })
}

type desktopView struct {
	widget.BaseWidget
	image     *canvas.Image
	input     *InputState
	modifiers uint8
}

func newDesktopView(input *InputState) *desktopView {
	v := &desktopView{image: canvas.NewImageFromImage(image.NewNRGBA(image.Rect(0, 0, 1, 1))), input: input}
	v.image.FillMode = canvas.ImageFillContain
	v.image.ScaleMode = canvas.ImageScalePixels
	v.ExtendBaseWidget(v)
	return v
}
func (v *desktopView) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(v.image) }
func (v *desktopView) FocusGained()                        { _ = v.input.SetFocused(true) }
func (v *desktopView) FocusLost()                          { _ = v.input.SetFocused(false); v.modifiers = 0 }
func (v *desktopView) TypedRune(rune)                      {}
func (v *desktopView) TypedKey(*fyne.KeyEvent)             {}
func (v *desktopView) AcceptsTab() bool                    { return true }
func (v *desktopView) keyDown(e *fyne.KeyEvent) {
	name := string(e.Name)
	usage, ok := HIDUsage(name)
	if !ok {
		return
	}
	if usage >= 0xe0 && usage <= 0xe7 {
		v.modifiers |= modifierForUsage(usage)
	}
	_ = v.input.Key(usage, protocol.ActionDown, v.modifiers)
}
func (v *desktopView) keyUp(e *fyne.KeyEvent) {
	name := string(e.Name)
	usage, ok := HIDUsage(name)
	if !ok {
		return
	}
	_ = v.input.Key(usage, protocol.ActionUp, v.modifiers)
	if usage >= 0xe0 && usage <= 0xe7 {
		v.modifiers &^= modifierForUsage(usage)
	}
}
func (v *desktopView) MouseDown(e *desktop.MouseEvent) {
	if b, ok := fyneButton(e.Button); ok {
		_ = v.input.Button(b, protocol.ActionDown)
	}
}
func (v *desktopView) MouseUp(e *desktop.MouseEvent) {
	if b, ok := fyneButton(e.Button); ok {
		_ = v.input.Button(b, protocol.ActionUp)
	}
}
func (v *desktopView) MouseIn(e *desktop.MouseEvent) { _ = v.input.SetFocused(true); v.MouseMoved(e) }
func (v *desktopView) MouseMoved(e *desktop.MouseEvent) {
	_ = v.input.Pointer(float64(e.Position.X), float64(e.Position.Y), float64(v.Size().Width), float64(v.Size().Height))
}
func (v *desktopView) MouseOut() { _ = v.input.SetFocused(false) }
func (v *desktopView) Scrolled(e *fyne.ScrollEvent) {
	h := wheelUnits(e.Scrolled.DX)
	vertical := wheelUnits(e.Scrolled.DY)
	if h != 0 || vertical != 0 {
		_ = v.input.Wheel(h, vertical)
	}
}
func wheelUnits(v float32) int16 {
	if v == 0 {
		return 0
	}
	if v > 0 {
		return 120
	}
	return -120
}
func fyneButton(b desktop.MouseButton) (protocol.Button, bool) {
	switch b {
	case desktop.MouseButtonPrimary:
		return protocol.ButtonLeft, true
	case desktop.MouseButtonTertiary:
		return protocol.ButtonMiddle, true
	case desktop.MouseButtonSecondary:
		return protocol.ButtonRight, true
	case desktop.MouseButton(8):
		return protocol.ButtonBack, true
	case desktop.MouseButton(16):
		return protocol.ButtonForward, true
	default:
		return 0, false
	}
}
func modifierForUsage(usage uint16) uint8 {
	switch usage {
	case 0xe1, 0xe5:
		return 1
	case 0xe0, 0xe4:
		return 2
	case 0xe2, 0xe6:
		return 4
	case 0xe3, 0xe7:
		return 8
	default:
		return 0
	}
}
func modifierBits(mod fyne.KeyModifier) uint8 {
	var out uint8
	if mod&fyne.KeyModifierShift != 0 {
		out |= 1
	}
	if mod&fyne.KeyModifierControl != 0 {
		out |= 2
	}
	if mod&fyne.KeyModifierAlt != 0 {
		out |= 4
	}
	if mod&fyne.KeyModifierSuper != 0 {
		out |= 8
	}
	return out
}
