package main

import (
	"context"
	"fmt"
	"time"

	"virtualdesktop/internal/host/capture"
)

func main() {
	c := capture.New()
	for i := 0; i < 15; i++ {
		img, err := c.Capture(context.Background(), 0)
		if err == nil && img.Width > 0 && img.Height > 0 {
			fmt.Printf("SUCCESS on attempt %d: %dx%d\n", i+1, img.Width, img.Height)
			return
		}
		fmt.Printf("attempt %d failed: w=%d h=%d err=%v\n", i+1, img.Width, img.Height, err)
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Println("FAILED after 15 attempts")
}
