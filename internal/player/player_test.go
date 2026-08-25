package player

import (
	"context"
	"testing"
)

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
