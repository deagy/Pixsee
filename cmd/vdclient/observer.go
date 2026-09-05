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
