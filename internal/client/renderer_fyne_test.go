//go:build fyne

package client

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
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
