package player

import (
	"context"
	"fmt"
	"testing"

	"github.com/krezh/noctune/internal/config"
)

func TestHistoryPersistsAndPaginatesAcrossManagerRestarts(t *testing.T) {
	cfg := &config.Config{
		CacheDir:     t.TempDir(),
		MaxQueueSize: 500,
	}
	manager, err := NewManager(nil, unusedResolver{}, cfg)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	guild, err := manager.Get("guild-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	const trackCount = 75
	for i := range trackCount {
		guild.addHistory(NewTrack(
			fmt.Sprintf("Track %d", i),
			"Artist",
			"Album",
			"artwork",
			fmt.Sprintf("https://youtube.example/watch/%d", i),
			fmt.Sprintf("https://source.example/track/%d", i),
			0,
			SourceYouTube,
			"original requester",
			"original avatar",
		))
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("first Manager.Close() error = %v", err)
	}

	restarted, err := NewManager(nil, unusedResolver{}, cfg)
	if err != nil {
		t.Fatalf("restarted NewManager() error = %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(context.Background()); err != nil {
			t.Errorf("restarted Manager.Close() error = %v", err)
		}
	})
	restoredGuild, err := restarted.Get("guild-1")
	if err != nil {
		t.Fatalf("restarted Get() error = %v", err)
	}

	first, err := restoredGuild.History(0, 50)
	if err != nil {
		t.Fatalf("first History() error = %v", err)
	}
	if first.Total != trackCount || len(first.Entries) != 50 || first.NextCursor == 0 {
		t.Fatalf("first history page = total %d, entries %d, cursor %d", first.Total, len(first.Entries), first.NextCursor)
	}
	if got, want := first.Entries[0].Track.Title, "Track 74"; got != want {
		t.Errorf("newest history title = %q, want %q", got, want)
	}
	if got, want := first.Entries[49].Track.Title, "Track 25"; got != want {
		t.Errorf("first page oldest title = %q, want %q", got, want)
	}

	second, err := restoredGuild.History(first.NextCursor, 50)
	if err != nil {
		t.Fatalf("second History() error = %v", err)
	}
	if second.Total != trackCount || len(second.Entries) != 25 || second.NextCursor != 0 {
		t.Fatalf("second history page = total %d, entries %d, cursor %d", second.Total, len(second.Entries), second.NextCursor)
	}
	if got, want := second.Entries[0].Track.Title, "Track 24"; got != want {
		t.Errorf("second page newest title = %q, want %q", got, want)
	}
	oldest := second.Entries[len(second.Entries)-1].Track
	if got, want := oldest.Title, "Track 0"; got != want {
		t.Errorf("oldest history title = %q, want %q", got, want)
	}

	if err := restoredGuild.Requeue(oldest.ID, "new requester", "new avatar"); err != nil {
		t.Fatalf("Requeue() error = %v", err)
	}
	queued := restoredGuild.Snapshot().Queue
	if len(queued) != 1 || queued[0].Title != oldest.Title || queued[0].RequestedBy != "new requester" {
		t.Fatalf("requeued track = %#v", queued)
	}
}
