// Package virtualdesktop is the module root. This file exists to host the
// repository's go:generate directive for mockery-generated test mocks: run
//
//	go generate ./...
//
// from the module root to regenerate every mock under internal/*/mocks
// (defined by .mockery.yaml) after changing any mocked interface
// (currently host.Capture, host.Input, host.Peer, client.Renderer,
// client.StateObserver, client.InputSender). Generated mocks are checked
// into the repository, so running `go generate` is only required after an
// interface changes, not as part of a normal build.
package virtualdesktop

//go:generate go tool mockery
