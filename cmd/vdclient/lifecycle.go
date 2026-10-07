package main

import (
	"context"
	"errors"
	"sync"
)

// runGUILifecycle runs a blocking UI event loop on the calling goroutine and a
// network session on a background goroutine, coordinating shutdown in both
// directions.
//
// hostRun blocks until the UI loop exits and must run on the process's main
// goroutine: Fyne's GLFW-backed driver panics outright when its event loop is
// started anywhere else (glfw.(*gLDriver).Run asserts main-goroutine), so the
// GUI build cannot keep the headless arrangement of session-on-main,
// UI-on-a-goroutine. hostQuit asks the UI loop to exit; it must be safe to call
// from another goroutine and be idempotent. sessionRun runs the session until
// sessionCtx is cancelled or it ends on its own.
//
// Shutdown coordination:
//   - The UI closing (hostRun returning, e.g. the user closes the Fyne window)
//     cancels the session context and waits for sessionRun to return.
//   - The parent context being cancelled (e.g. SIGINT) or the session ending on
//     its own (clean exit or error) calls hostQuit, which makes hostRun return.
//
// runGUILifecycle returns sessionRun's terminal error unchanged; the caller
// maps a cancelled context to a normal shutdown (see finishSession). It returns
// only after both hostRun and sessionRun have returned, so neither the UI loop
// nor the session goroutine is left behind.
func runGUILifecycle(ctx context.Context, hostRun, hostQuit func(), sessionRun func(context.Context) error) error {
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	var (
		sessionErr error
		wg         sync.WaitGroup
	)
	sessionDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(sessionDone)
		sessionErr = sessionRun(sessionCtx)
	}()

	// Bring the UI down when the parent context is cancelled or the session
	// ends by itself. hostQuit is called from this goroutine, never the main
	// one, so the main goroutine stays free to run the UI event loop.
	uiExited := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			hostQuit()
		case <-sessionDone:
			hostQuit()
		case <-uiExited:
		}
	}()

	hostRun()

	// The UI loop has exited. Stop the watcher before it can ask an
	// already-exited UI to quit, then cancel the session and wait for it to
	// return. sessionErr is read only after wg.Wait, which synchronizes with
	// the session goroutine's write.
	close(uiExited)
	cancelSession()
	wg.Wait()
	<-watcherDone

	return sessionErr
}

// finishSession maps a session's terminal error to the client's process
// outcome: a cancelled context is a normal shutdown (nil, with the historical
// "shutting down" line), and any other error is returned unchanged.
func finishSession(err error, observer *loggingObserver) error {
	if errors.Is(err, context.Canceled) {
		observer.log("shutting down")
		return nil
	}
	return err
}
