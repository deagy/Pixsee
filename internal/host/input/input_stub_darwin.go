//go:build darwin && !cgo

// Package input injects keyboard and pointer events on the host. This file is a
// stub used when cgo is disabled on darwin, so the package still compiles. Real
// input injection on macOS requires cgo (CoreGraphics), so when cgo is off the
// injector returns an error from every method. The platform file
// input_darwin.go provides the real adapter when cgo is enabled.
package input

import (
	"context"
	"errors"

	"virtualdesktop/internal/protocol"
)

// stubError is returned by the darwin cgo-disabled stub.
var stubError = errors.New("macOS input injection requires cgo; rebuild with CGO_ENABLED=1")

// Injector is a no-op injector used when cgo is disabled on darwin.
type Injector struct{}

// New returns a stub injector.
func New() (*Injector, error) { return &Injector{}, nil }

// Close is a no-op.
func (i *Injector) Close() {}

// Key returns an error; real injection requires cgo.
func (i *Injector) Key(context.Context, uint16, protocol.Action, uint8) error { return stubError }

// Move returns an error; real injection requires cgo.
func (i *Injector) Move(context.Context, uint32, uint32) error { return stubError }

// Button returns an error; real injection requires cgo.
func (i *Injector) Button(context.Context, protocol.Button, protocol.Action) error { return stubError }

// Wheel returns an error; real injection requires cgo.
func (i *Injector) Wheel(context.Context, int16, int16) error { return stubError }

// ReleaseAll returns an error; real injection requires cgo.
func (i *Injector) ReleaseAll(context.Context) error { return stubError }
