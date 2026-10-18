package bot

import (
	"context"
	"testing"
	"time"

	"github.com/krezh/noctune/internal/player"
)

func TestCloseContextWaitsForWatchers(t *testing.T) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	b := &Bot{watchCtx: watchCtx, watchCancel: watchCancel}
	b.watchWG.Add(1)
	go func() {
		defer b.watchWG.Done()
		<-watchCtx.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.CloseContext(ctx); err != nil {
		t.Fatalf("CloseContext() error = %v", err)
	}
	if !b.closed {
		t.Fatal("bot was not marked closed")
	}
}

func TestClosedBotRejectsNewWatchers(t *testing.T) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	b := &Bot{
		watchCtx:      watchCtx,
		watchCancel:   watchCancel,
		watchedGuilds: make(map[string]struct{}),
	}
	if err := b.CloseContext(context.Background()); err != nil {
		t.Fatalf("CloseContext() error = %v", err)
	}

	b.watchGuild("123")
	if len(b.watchedGuilds) != 0 {
		t.Fatal("closed bot registered a new guild watcher")
	}
}
func TestPresenceFallsBackToAnotherActiveGuild(t *testing.T) {
	b := &Bot{
		activeTracks:   make(map[string]*player.Track),
		presenceSignal: make(chan struct{}, 1),
	}
	first := &player.Track{ID: "first"}
	second := &player.Track{ID: "second"}
	b.queuePresence("guild-1", player.State{Status: player.StatusPlaying, Current: first})
	b.queuePresence("guild-2", player.State{Status: player.StatusPlaying, Current: second})
	b.queuePresence("guild-1", player.State{Status: player.StatusIdle})

	got, _ := b.desiredPresence()
	if got != second {
		t.Fatalf("desired presence = %v, want second guild track", got)
	}
	b.queuePresence("idle-guild", player.State{Status: player.StatusIdle})
	got, _ = b.desiredPresence()
	if got != second {
		t.Fatal("an unrelated idle guild cleared the active presence")
	}
}
