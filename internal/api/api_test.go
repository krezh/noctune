package api

import (
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krezh/noctune/internal/player"
	"github.com/krezh/noctune/web"
)

func TestRequireSameOrigin(t *testing.T) {
	srv := &Server{}
	next := srv.requireSameOrigin(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	for _, tc := range []struct {
		name         string
		origin       string
		secFetchSite string
		want         int
	}{
		{name: "same origin", origin: "https://music.example.com", want: http.StatusNoContent},
		{name: "different origin", origin: "https://evil.example", want: http.StatusForbidden},
		{name: "sibling subdomain", origin: "https://admin.example.com", want: http.StatusForbidden},
		{name: "cross-site without origin", secFetchSite: "cross-site", want: http.StatusForbidden},
		{name: "same-site without origin", secFetchSite: "same-site", want: http.StatusForbidden},
		{name: "non-browser client", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://music.example.com/action", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			rr := httptest.NewRecorder()

			next(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestGuildInitials(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Lo-Fi Lounge", "LL"},
		{"noctune", "NO"},
		{"a", "A"},
		{"", ""},
		{"  Extra   Space  Server", "ES"},
		{"日本語 Server", "日S"},
	}
	for _, tc := range cases {
		if got := guildInitials(tc.name); got != tc.want {
			t.Errorf("guildInitials(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPlayerSectionChanged(t *testing.T) {
	trackA := &player.Track{ID: "a"}
	trackB := &player.Track{ID: "b"}
	base := player.State{VoiceChannelID: "c1", Status: player.StatusPlaying, Volume: 100, Loop: player.LoopOff, Current: trackA}

	cases := []struct {
		name string
		prev *player.State
		cur  player.State
		want bool
	}{
		{"first push, no prev", nil, base, true},
		{"nothing changed", &base, base, false},
		{"queue-only change is not a player change", &base, func() player.State { s := base; s.Queue = []*player.Track{trackA}; return s }(), false},
		{"position ticking is not a player change", &base, func() player.State { s := base; s.Position = 30 * time.Second; return s }(), false},
		{"voice channel changed", &base, func() player.State { s := base; s.VoiceChannelID = "c2"; return s }(), true},
		{"status changed", &base, func() player.State { s := base; s.Status = player.StatusPaused; return s }(), true},
		{"volume changed", &base, func() player.State { s := base; s.Volume = 50; return s }(), true},
		{"loop changed", &base, func() player.State { s := base; s.Loop = player.LoopTrack; return s }(), true},
		{"current track changed", &base, func() player.State { s := base; s.Current = trackB; return s }(), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := playerSectionChanged(tc.prev, tc.cur); got != tc.want {
				t.Errorf("playerSectionChanged() = %v, want %v", got, tc.want)
			}
		})
	}
}

// go build can't catch a broken html/template — templates are parsed at
// runtime, not compiled. This exercises every named template with
// representative data so a bad {{}} fails `go test` instead of a live
// request.
func TestTemplatesParseAndExecute(t *testing.T) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}).ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	track := &player.Track{
		ID:         "t1",
		Title:      "Track Title",
		Artist:     "Some Artist",
		Album:      "Some Album",
		ArtworkURL: "https://example.com/art.png",
		Duration:   3 * time.Minute,
		Source:     player.SourceSpotify,
	}

	panel := PanelData{
		GuildID:           "123",
		Channels:          []ChannelView{{ID: "c1", Name: "General"}},
		ShowChannelPicker: true,
		State: player.State{
			GuildID:        "123",
			VoiceChannelID: "c1",
			Status:         player.StatusPlaying,
			Volume:         100,
			Loop:           player.LoopQueue,
			Current:        track,
			Queue:          []*player.Track{track},
			History:        []*player.Track{{ID: "h1", Title: "Past Track", Artist: "Old Artist"}},
		},
	}
	loggedInSession := &SessionView{Username: "someone", AvatarURL: "https://example.com/a.png"}

	cases := []struct {
		name string
		data any
	}{
		{"page:login", loginPageData{Error: "bad token"}},
		{"page:login", loginPageData{DiscordEnabled: true, TokenEnabled: true}},
		{"page:login", loginPageData{}},
		{"page:index", struct {
			Guilds  []GuildView
			Session *SessionView
		}{Guilds: []GuildView{{ID: "1", Name: "Test Guild", Icon: "https://example.com/i.png"}}, Session: loggedInSession}},
		{"page:index", struct {
			Guilds  []GuildView
			Session *SessionView
		}{}},
		{"page:guild", struct {
			Guild   GuildView
			Guilds  []GuildView
			Panel   PanelData
			Session *SessionView
		}{Guild: GuildView{ID: "123", Name: "Test Guild"}, Guilds: []GuildView{{ID: "123", Name: "Test Guild"}}, Panel: panel, Session: loggedInSession}},
		{"panel-shell", panel},
		{"panel-inner", panel},
		{"panel-inner", PanelData{GuildID: "123", State: player.State{Status: player.StatusIdle, Loop: player.LoopOff}}}, // empty queue, no current track
		{"panel-inner", PanelData{GuildID: "123", State: player.State{
			VoiceChannelID: "c1", Status: player.StatusLoading, Volume: 100, Loop: player.LoopOff, Current: track,
		}}}, // track dequeued but not yet playing — no Progress
		{"panel-inner", PanelData{
			GuildID: "123",
			State: player.State{
				VoiceChannelID: "c1", Status: player.StatusPaused, Volume: 100, Loop: player.LoopOff,
				Current: track, Position: 90 * time.Second,
			},
			Progress: computeProgress(player.State{Status: player.StatusPaused, Current: track, Position: 90 * time.Second}),
		}}, // paused mid-track, with a populated progress bar
		{"panel-media", panel},       // rendered on its own by handleEvents when mediaSectionChanged
		{"panel-controls", panel},    // rendered on its own by handleEvents when playerSectionChanged
		{"panel-status-text", panel}, // the small #status-volume/#status-loop spans, same trigger as panel-controls
		{"panel-queue", panel},       // rendered on its own by handleEvents on every notify, queue-only or not
		{"panel-controls", func() PanelData { p := panel; p.ShowChannelPicker = false; return p }()}, // Discord OAuth session: "join my channel" button instead of the dropdown
	}

	for _, tc := range cases {
		if err := tmpl.ExecuteTemplate(io.Discard, tc.name, tc.data); err != nil {
			t.Errorf("execute %s: %v", tc.name, err)
		}
	}
}
