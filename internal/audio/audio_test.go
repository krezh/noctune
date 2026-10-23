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
	h := &Handle{stopCh: make(chan struct{}), doneCh: make(chan error, 1), cleanupDone: make(chan struct{})}

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
	select {
	case err := <-h.Done():
		t.Fatalf("Done fired before cleanup completed: %v", err)
	default:
	}
	close(h.cleanupDone)
	if err := <-h.Done(); err != nil {
		t.Fatalf("Done() error = %v, want nil", err)
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
func TestPlayPreservesZeroVolumeAndConfiguredFFmpeg(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	ffmpeg := filepath.Join(dir, "custom-ffmpeg")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\nexit 42\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("CAPTURE", capture)
	t.Setenv("PATH", dir)

	h, err := Play(io.NopCloser(strings.NewReader("audio")), Options{Volume: 0, FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatalf("Play() error = %v", err)
	}
	_, _ = h.ProvideOpusFrame()
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured arguments: %v", err)
	}
	if !strings.Contains(string(args), "volume=volume=0.000000") {
		t.Fatalf("ffmpeg arguments did not preserve mute: %s", args)
	}
}

func TestPlayLoudnessNormalization(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      Options
		wantChain string
		wantBurst string
	}{
		{
			name:      "enabled",
			opts:      Options{Volume: 50, Normalize: true, LoudnessTarget: -14},
			wantChain: ",loudnorm=I=-14:TP=-1.5:LRA=20,volume=volume=0.500000:eval=frame",
			wantBurst: "5",
		},
		{
			name:      "disabled",
			opts:      Options{Volume: 50, LoudnessTarget: -14},
			wantChain: ",volume=volume=0.500000:eval=frame",
			wantBurst: "2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "args")
			ffmpeg := filepath.Join(dir, "custom-ffmpeg")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\nexit 42\n"
			if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake ffmpeg: %v", err)
			}
			t.Setenv("CAPTURE", capture)
			tc.opts.FFmpegPath = ffmpeg

			h, err := Play(io.NopCloser(strings.NewReader("audio")), tc.opts)
			if err != nil {
				t.Fatalf("Play() error = %v", err)
			}
			_, _ = h.ProvideOpusFrame()
			raw, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("read captured arguments: %v", err)
			}
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			var chain, burst string
			for i, arg := range args[:len(args)-1] {
				switch arg {
				case "-af":
					chain = args[i+1]
				case "-readrate_initial_burst":
					burst = args[i+1]
				}
			}
			if !strings.HasSuffix(chain, tc.wantChain) {
				t.Fatalf("filter chain = %q, want suffix %q", chain, tc.wantChain)
			}
			if burst != tc.wantBurst {
				t.Fatalf("-readrate_initial_burst = %q, want %q", burst, tc.wantBurst)
			}
		})
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
		case err := <-h.Done():
			t.Fatalf("Done fired before buffered frames were consumed: %v", err)
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
