package restore

import (
	"context"
	"testing"
	"time"
)

func TestWaitForPrefillAlreadyDoneAddsNoTimeout(t *testing.T) {
	done := make(chan struct{})
	close(done)
	waited, timedOut, err := waitForPrefill(context.Background(), done, nil, 100*time.Millisecond)
	if err != nil || timedOut || waited >= 10*time.Millisecond {
		t.Fatalf("waited=%v timedOut=%v err=%v", waited, timedOut, err)
	}
}

func TestWaitForPrefillTimeoutCancelsAndJoins(t *testing.T) {
	done := make(chan struct{})
	canceled := make(chan struct{})
	cancel := func() {
		close(canceled)
	}
	go func() {
		<-canceled
		// Model a COPY already inside the kernel when cancellation arrives.
		time.Sleep(5 * time.Millisecond)
		close(done)
	}()
	waited, timedOut, err := waitForPrefill(context.Background(), done, cancel, 10*time.Millisecond)
	if err != nil || !timedOut || waited < 10*time.Millisecond {
		t.Fatalf("waited=%v timedOut=%v err=%v", waited, timedOut, err)
	}
}
