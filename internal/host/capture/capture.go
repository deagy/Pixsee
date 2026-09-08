// Package capture reads a selected display on the host and converts it to a
// packed, opaque BGRA8888 image for the damage detector. It is platform
// neutral: the same package compiles on linux, windows, and darwin, delegating
// to whatever the screenshot library supports on each. The linux path uses X11
// through the shared screenshot dependency; windows and darwin use their
// platform capture backends.
package capture

import (
	"context"
	"fmt"
	"image"

	"github.com/kbinani/screenshot"
	"virtualdesktop/internal/damage"
)

// Capture reads one display and converts it to packed, opaque BGRA8888.
type Capture struct{}

// New returns a Capture bound to the host's display back end.
func New() *Capture { return &Capture{} }

// Capture reads the selected display and converts it to packed, opaque
// BGRA8888. displayID is the zero-based index into the host's active displays.
func (c *Capture) Capture(ctx context.Context, displayID uint32) (damage.Image, error) {
	if err := ctx.Err(); err != nil {
		return damage.Image{}, err
	}
	count := screenshot.NumActiveDisplays()
	if displayID >= uint32(count) {
		return damage.Image{}, fmt.Errorf("display %d is unavailable (active displays: %d)", displayID, count)
	}
	bounds := screenshot.GetDisplayBounds(int(displayID))
	rgba, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return damage.Image{}, fmt.Errorf("capture display %d: %w", displayID, err)
	}
	return convert(bounds, rgba), nil
}

// convert reorders the library's RGBA pixels into the wire's BGRA8888 layout
// with alpha fixed to 0xff.
func convert(bounds image.Rectangle, rgba *image.RGBA) damage.Image {
	width, height := bounds.Dx(), bounds.Dy()
	pixels := make([]byte, width*height*4)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src := rgba.PixOffset(rgba.Rect.Min.X+x, rgba.Rect.Min.Y+y)
			dst := (y*width + x) * 4
			pixels[dst], pixels[dst+1], pixels[dst+2], pixels[dst+3] = rgba.Pix[src+2], rgba.Pix[src+1], rgba.Pix[src], 0xff
		}
	}
	return damage.Image{Width: uint32(width), Height: uint32(height), Pixels: pixels}
}
