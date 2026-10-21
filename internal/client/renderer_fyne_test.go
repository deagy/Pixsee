//go:build fyne

package client

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"virtualdesktop/internal/protocol"
)

func TestFyneButtonMappingCoversApprovedButtons(t *testing.T) {
	tests := []struct {
		native desktop.MouseButton
		want   protocol.Button
	}{
		{desktop.MouseButtonPrimary, protocol.ButtonLeft},
		{desktop.MouseButtonTertiary, protocol.ButtonMiddle},
		{desktop.MouseButtonSecondary, protocol.ButtonRight},
		{desktop.MouseButton(8), protocol.ButtonBack},
		{desktop.MouseButton(16), protocol.ButtonForward},
	}
	for _, tc := range tests {
		got, ok := fyneButton(tc.native)
		if !ok || got != tc.want {
			t.Fatalf("button %d: got %d,%v want %d,true", tc.native, got, ok, tc.want)
		}
	}
	if _, ok := fyneButton(desktop.MouseButton(32)); ok {
		t.Fatal("accepted unapproved button")
	}
}

func TestFyneModifierMappingUsesHIDBitmap(t *testing.T) {
	all := fyne.KeyModifierShift | fyne.KeyModifierControl | fyne.KeyModifierAlt | fyne.KeyModifierSuper
	if got := modifierBits(all); got != 0x0f {
		t.Fatalf("got %#x", got)
	}
}

func TestFormatDebugOverlay(t *testing.T) {
	frame := Snapshot{Width: 1920, Height: 1080}
	got := formatDebugOverlay("Pixsee", frame,
		fyne.NewSize(1280, 720), fyne.NewSize(1280, 700), fyne.NewSize(1280, 653), fyne.NewSize(1280, 653), 1.5)
	want := "Pixsee | frame:1920x1080 scale:1.50 content:1280x720 canvas:1280x700 view:1280x653 img:1280x653"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFyneRendererDebugUpdatesTitle(t *testing.T) {
	t.Setenv("PIXSEE_DEBUG_RENDER", "1")
	a := test.NewApp()
	defer a.Quit()
	input := NewInputState(nil)
	r := newFyneRenderer(a, "Pixsee", input)
	frame := Snapshot{Width: 1920, Height: 1080, Pixels: make([]byte, 1920*1080*4)}
	r.Present(frame)
	got := r.window.Title()
	if !strings.Contains(got, "frame:1920x1080") {
		t.Fatalf("title %q missing frame size", got)
	}
	if !strings.Contains(got, "scale:") {
		t.Fatalf("title %q missing scale", got)
	}
	if !strings.Contains(got, "img:") {
		t.Fatalf("title %q missing image size", got)
	}
}

func TestFyneRendererNoDebugKeepsTitle(t *testing.T) {
	t.Setenv("PIXSEE_DEBUG_RENDER", "")
	a := test.NewApp()
	defer a.Quit()
	input := NewInputState(nil)
	r := newFyneRenderer(a, "Pixsee", input)
	frame := Snapshot{Width: 1920, Height: 1080, Pixels: make([]byte, 1920*1080*4)}
	r.Present(frame)
	if got := r.window.Title(); got != "Pixsee" {
		t.Fatalf("title %q, want unchanged title", got)
	}
}
