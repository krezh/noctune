package bot

import (
	"context"
	"testing"
	"time"
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
