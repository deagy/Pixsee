package main

import (
	"fmt"
	"time"

	"virtualdesktop/internal/client"
)

// loggingObserver prints connection-state transitions and errors so the
// operator can see connection and error states without a GUI.
type loggingObserver struct{}

func (l *loggingObserver) ConnectionState(state client.ConnectionState, err error) {
	if err != nil {
		fmt.Printf("%s error: %s\n", time.Now().Format("15:04:05"), err.Error())
		return
	}
	fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), connectionStateText(state))
}

func (l *loggingObserver) log(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// connectionStateText renders a connection state for the operator-facing log
// lines above. It is defined here (untagged) so both the headless and the
// "fyne" GUI builds of the client share one definition.
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
