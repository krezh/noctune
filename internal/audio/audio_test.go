package audio

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStopIsConcurrentSafe(t *testing.T) {
	h := &Handle{stopCh: make(chan struct{}), doneCh: make(chan error, 1)}

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

func TestPlayReportsFFmpegFailure(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\necho encoder failed >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h, err := Play(io.NopCloser(strings.NewReader("audio")), Options{Volume: 100})
	if err != nil {
		t.Fatalf("Play() error = %v", err)
	}
	if _, err := h.ProvideOpusFrame(); err == nil || !strings.Contains(err.Error(), "encoder failed") {
		t.Fatalf("ProvideOpusFrame() error = %v, want ffmpeg failure", err)
	}
	if err := <-h.Done(); err == nil || !strings.Contains(err.Error(), "encoder failed") {
		t.Fatalf("Done() error = %v, want ffmpeg failure", err)
	}
}

func TestDoneWaitsForBufferedFrames(t *testing.T) {
	h := &Handle{
		stopCh: make(chan struct{}),
		doneCh: make(chan error, 1),
		frames: make(chan []byte, 2),
	}
	h.frames <- []byte("first")
	h.frames <- []byte("second")
	close(h.frames)

	for range 2 {
		if _, err := h.ProvideOpusFrame(); err != nil {
			t.Fatalf("provide buffered frame: %v", err)
		}
		select {
		case <-h.Done():
			t.Fatal("Done fired before buffered frames were consumed")
		default:
		}
	}

	if _, err := h.ProvideOpusFrame(); err != io.EOF {
		t.Fatalf("ProvideOpusFrame() error = %v, want EOF", err)
	}
	if err := <-h.Done(); err != nil {
		t.Fatalf("Done() error = %v, want nil", err)
	}
}
