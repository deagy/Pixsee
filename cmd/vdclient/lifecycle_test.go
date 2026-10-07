package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeHost is a deterministic stand-in for the renderer host's event loop. Its
// run blocks until requestQuit is called; requestQuit is idempotent (like the
// real hosts' Quit) but counts every call so a test can assert the UI was asked
// to exit exactly once.
type fakeHost struct {
	quit     chan struct{}
	quitOnce sync.Once
	mu       sync.Mutex
	quits    int
}

func newFakeHost() *fakeHost { return &fakeHost{quit: make(chan struct{})} }

func (h *fakeHost) requestQuit() {
	h.mu.Lock()
	h.quits++
	h.mu.Unlock()
	h.quitOnce.Do(func() { close(h.quit) })
}

func (h *fakeHost) quitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quits
}

// TestRunGUILifecycleWindowCloseCancelsAndJoinsSession covers the "user closes
// the Fyne window" direction: the UI loop (hostRun) returns on its own, and
// runGUILifecycle must cancel the session and wait for it to return before
// returning itself.
func TestRunGUILifecycleWindowCloseCancelsAndJoinsSession(t *testing.T) {
	host := newFakeHost()
	sessionReturned := make(chan struct{})
	sessionRun := func(ctx context.Context) error {
		<-ctx.Done()
		close(sessionReturned)
		return ctx.Err()
	}
	// The window is already closed: the UI loop returns without being asked.
	hostRun := func() {}

	err := runGUILifecycle(context.Background(), hostRun, host.requestQuit, sessionRun)

	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-sessionReturned:
	default:
		t.Fatal("runGUILifecycle returned before the session goroutine completed")
	}
}

// TestRunGUILifecycleContextCancelQuitsUI covers the signal/SIGINT direction:
// cancelling the parent context must ask the UI to exit so the blocking UI loop
// returns, and the cancellation surfaces as context.Canceled.
func TestRunGUILifecycleContextCancelQuitsUI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := newFakeHost()
	hostRunning := make(chan struct{})
	hostRun := func() {
		close(hostRunning)
		<-host.quit
	}
	sessionRun := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	hostStarted := make(chan struct{})
	go func() {
		<-hostRunning
		cancel()
		close(hostStarted)
	}()

	err := runGUILifecycle(ctx, hostRun, host.requestQuit, sessionRun)
	<-hostStarted

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, host.quitCount(), "UI must be asked to quit exactly once")
}

// TestRunGUILifecycleSessionErrorQuitsUIAndPropagates covers a session ending
// on its own with an error: the UI is asked to exit, and the error is returned
// unchanged so the caller can report it.
func TestRunGUILifecycleSessionErrorQuitsUIAndPropagates(t *testing.T) {
	host := newFakeHost()
	wantErr := errors.New("session exploded")
	hostRun := func() { <-host.quit }
	sessionRun := func(context.Context) error { return wantErr }

	err := runGUILifecycle(context.Background(), hostRun, host.requestQuit, sessionRun)

	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 1, host.quitCount(), "UI must be asked to quit exactly once")
}

// TestRunGUILifecycleSessionCleanExitQuitsUI covers a session that returns
// cleanly (the host sent CLOSE): the UI is asked to exit and no error is
// produced.
func TestRunGUILifecycleSessionCleanExitQuitsUI(t *testing.T) {
	host := newFakeHost()
	hostRun := func() { <-host.quit }
	sessionRun := func(context.Context) error { return nil }

	err := runGUILifecycle(context.Background(), hostRun, host.requestQuit, sessionRun)

	require.NoError(t, err)
	require.Equal(t, 1, host.quitCount(), "UI must be asked to quit exactly once")
}

// TestRunGUILifecycleConcurrentUIExitAndSessionCompletion drives the case the
// review flagged: the UI loop exiting and the session completing (sessionDone
// closing) become ready at the same instant, so the watcher's select has both
// the uiExited and sessionDone cases ready. The helper must still return, join
// both goroutines, and never ask the UI to quit more than once even when the
// watcher races the UI's own exit.
//
// The barrier (bothReached/release) makes the two terminal events ready
// together without sleeps: both loops park on release and are let go as one.
func TestRunGUILifecycleConcurrentUIExitAndSessionCompletion(t *testing.T) {
	host := newFakeHost()

	release := make(chan struct{})
	var bothReached sync.WaitGroup
	bothReached.Add(2)
	go func() {
		bothReached.Wait()
		close(release)
	}()

	hostReturned := make(chan struct{})
	hostRun := func() {
		bothReached.Done()
		<-release
		close(hostReturned)
	}

	sessionReturned := make(chan struct{})
	sessionRun := func(context.Context) error {
		bothReached.Done()
		<-release
		close(sessionReturned)
		return nil
	}

	err := runGUILifecycle(context.Background(), hostRun, host.requestQuit, sessionRun)

	require.NoError(t, err)
	select {
	case <-hostReturned:
	default:
		t.Fatal("runGUILifecycle returned before the UI loop completed")
	}
	select {
	case <-sessionReturned:
	default:
		t.Fatal("runGUILifecycle returned before the session goroutine completed")
	}
	require.LessOrEqual(t, host.quitCount(), 1, "UI must be asked to quit at most once")
}

// TestFinishSessionMapsCancellationToNil proves a cancelled session is a normal
// shutdown (nil) and that the historical "shutting down" line is still emitted.
func TestFinishSessionMapsCancellationToNil(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	got := finishSession(context.Canceled, &loggingObserver{})

	require.NoError(t, w.Close())
	os.Stdout = orig
	out, _ := io.ReadAll(r)

	require.NoError(t, got)
	require.Contains(t, string(out), "shutting down")
}

// TestFinishSessionMapsWrappedCancellationToNil proves a wrapped context
// cancellation is still treated as a normal shutdown.
func TestFinishSessionMapsWrappedCancellationToNil(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	got := finishSession(fmt.Errorf("client: connect: %w", context.Canceled), &loggingObserver{})

	require.NoError(t, w.Close())
	os.Stdout = orig
	_, _ = io.ReadAll(r)

	require.NoError(t, got)
}

// TestFinishSessionPropagatesOtherErrors proves any non-cancellation error is
// returned unchanged.
func TestFinishSessionPropagatesOtherErrors(t *testing.T) {
	want := errors.New("boom")
	require.ErrorIs(t, finishSession(want, &loggingObserver{}), want)
}
