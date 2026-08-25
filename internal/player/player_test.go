package player

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	voiceapi "github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/krezh/noctune/internal/config"
)

type blockingVoiceConn struct {
	openStarted chan struct{}
	openRelease chan struct{}
	closed      atomic.Int32
}

func (c *blockingVoiceConn) Open(context.Context, snowflake.ID, bool, bool) error {
	close(c.openStarted)
	<-c.openRelease
	return nil
}

func (c *blockingVoiceConn) Close(context.Context) {
	c.closed.Add(1)
}

func (c *blockingVoiceConn) SetOpusFrameProvider(voiceapi.OpusFrameProvider) {}

type unusedResolver struct{}

func (unusedResolver) OpenStream(context.Context, string) (io.ReadCloser, error) {
	panic("unexpected OpenStream call")
}

type blockingPlaybackHandle struct {
	pauseStarted chan struct{}
	pauseRelease chan struct{}
	done         chan error
}

func (h *blockingPlaybackHandle) ProvideOpusFrame() ([]byte, error) { return nil, io.EOF }
func (h *blockingPlaybackHandle) Close()                            {}
func (h *blockingPlaybackHandle) Pause() {
	close(h.pauseStarted)
	<-h.pauseRelease
}
func (h *blockingPlaybackHandle) Resume()                 {}
func (h *blockingPlaybackHandle) Stop()                   {}
func (h *blockingPlaybackHandle) SetVolume(int) error     { return nil }
func (h *blockingPlaybackHandle) Done() <-chan error      { return h.done }
func (h *blockingPlaybackHandle) Position() time.Duration { return 0 }

func TestStopCancelsTrackStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gp := &GuildPlayer{trackCancel: cancel, loop: LoopTrack, subs: make(map[chan State]struct{})}

	if err := gp.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Stop did not cancel track startup")
	}
	if gp.loop != LoopOff {
		t.Fatalf("loop = %q, want %q", gp.loop, LoopOff)
	}
}

func TestLeaveCancelsTrackStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gp := &GuildPlayer{trackCancel: cancel, subs: make(map[chan State]struct{})}

	if err := gp.Leave(); err != nil {
		t.Fatalf("Leave() error = %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Leave did not cancel track startup")
	}
}

func TestStopInvalidatesResolutions(t *testing.T) {
	gp := &GuildPlayer{
		loop:             LoopOff,
		subs:             make(map[chan State]struct{}),
		resolutionCancel: make(map[uint64]context.CancelFunc),
	}
	generation := gp.ResolutionGeneration()
	ctx, finish, ok := gp.BeginResolution(context.Background(), generation)
	if !ok {
		t.Fatal("BeginResolution rejected current generation")
	}
	defer finish()

	if err := gp.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Stop did not cancel active resolution")
	}
	if _, _, ok := gp.BeginResolution(context.Background(), generation); ok {
		t.Fatal("BeginResolution accepted stale generation")
	}
}

func TestLeaveWaitsForPendingJoin(t *testing.T) {
	conn := &blockingVoiceConn{openStarted: make(chan struct{}), openRelease: make(chan struct{})}
	gp := &GuildPlayer{
		GuildID:          "123",
		createVoiceConn:  func(snowflake.ID) voiceConnection { return conn },
		resolver:         unusedResolver{},
		cfg:              &config.Config{IdleDisconnectSeconds: 0},
		subs:             make(map[chan State]struct{}),
		playSignal:       make(chan struct{}, 1),
		stopLoopCh:       make(chan struct{}),
		resolutionCancel: make(map[uint64]context.CancelFunc),
	}
	t.Cleanup(func() { close(gp.stopLoopCh) })

	joinDone := make(chan error, 1)
	go func() { joinDone <- gp.Join(context.Background(), "456") }()
	<-conn.openStarted

	leaveDone := make(chan error, 1)
	go func() { leaveDone <- gp.Leave() }()
	select {
	case <-leaveDone:
		t.Fatal("Leave returned while Join was still opening")
	case <-time.After(10 * time.Millisecond):
	}

	close(conn.openRelease)
	if err := <-joinDone; err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	if err := <-leaveDone; err != nil {
		t.Fatalf("Leave() error = %v", err)
	}
	if got := conn.closed.Load(); got != 1 {
		t.Fatalf("connection closed %d times, want 1", got)
	}
	if got := gp.Snapshot().VoiceChannelID; got != "" {
		t.Fatalf("VoiceChannelID = %q after Leave", got)
	}
}

func TestPauseDoesNotOverwriteNewPlaybackState(t *testing.T) {
	oldHandle := &blockingPlaybackHandle{
		pauseStarted: make(chan struct{}),
		pauseRelease: make(chan struct{}),
		done:         make(chan error, 1),
	}
	newHandle := &blockingPlaybackHandle{done: make(chan error, 1)}
	gp := &GuildPlayer{handle: oldHandle, status: StatusPlaying, subs: make(map[chan State]struct{})}

	pauseDone := make(chan error, 1)
	go func() { pauseDone <- gp.Pause() }()
	<-oldHandle.pauseStarted
	gp.mu.Lock()
	gp.handle = newHandle
	gp.status = StatusLoading
	gp.mu.Unlock()
	close(oldHandle.pauseRelease)

	if err := <-pauseDone; err == nil {
		t.Fatal("Pause succeeded after the playback handle changed")
	}
	if got := gp.Snapshot().Status; got != StatusLoading {
		t.Fatalf("status = %q, want %q", got, StatusLoading)
	}
}

func TestStalePlaybackCompletionDoesNotRequeue(t *testing.T) {
	oldHandle := &blockingPlaybackHandle{done: make(chan error, 1)}
	newHandle := &blockingPlaybackHandle{done: make(chan error, 1)}
	track := &Track{ID: "old"}
	gp := &GuildPlayer{
		handle:      newHandle,
		playbackGen: 2,
		loop:        LoopTrack,
		subs:        make(map[chan State]struct{}),
	}

	if gp.finishPlayback(oldHandle, track, 1, nil) {
		t.Fatal("stale completion was accepted")
	}
	if len(gp.Snapshot().Queue) != 0 {
		t.Fatal("stale completion requeued its track")
	}
	if gp.handle != newHandle {
		t.Fatal("stale completion cleared the current handle")
	}
}
