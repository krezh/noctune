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
