package audio

import (
	"sync"
	"testing"
)

func TestStopIsConcurrentSafe(t *testing.T) {
	h := &Handle{stopCh: make(chan struct{})}

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(h.Stop)
	}
	wg.Wait()

	select {
	case <-h.stopCh:
	default:
		t.Fatal("Stop did not close stopCh")
	}
}
