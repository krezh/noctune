// Package api serves the htmx-based web GUI: a guild picker and, per
// guild, a control panel (join/leave, search-and-queue, transport
// controls, live queue) that pushes updates over SSE. It drives the same
// player.Manager the Discord bot does, so either surface reflects the
// other in real time.
//
// Two independent login modes produce a session (see session.go):
// Discord OAuth2 (oauth.go) attaches a real Discord user ID to the
// session, which lets mutating playback actions be gated on that user
// currently sitting in the same voice channel the bot is connected to
// (see requireVoicePresence); WEB_AUTH_TOKEN is a Trusted fallback with
// no per-user identity, granting full, ungated control to anyone who has
// the token — same trust model as the old shared-token cookie. A
// session is a signed cookie, not a server-side table — see sessionStore
// — so unlike the rest of noctune's state, logins survive a restart as
// long as SESSION_SECRET is set to a fixed value.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"golang.org/x/oauth2"

	"github.com/krezh/noctune/internal/config"
	"github.com/krezh/noctune/internal/player"
	"github.com/krezh/noctune/internal/resolve"
	"github.com/krezh/noctune/internal/spotify"
	"github.com/krezh/noctune/internal/youtube"
	"github.com/krezh/noctune/web"
)

const sessionCookie = "noctune_session"

type GuildView struct {
	ID   string
	Name string
	Icon string
	// Initials is the fallback shown in the guild-icon circle when Icon
	// is empty (a guild with no custom icon set).
	Initials string
	// Live is true when the bot currently holds a voice connection in
	// this guild — drives the sidebar/index "live" status dot.
	Live bool
}

// guildInitials mirrors Discord's own fallback-avatar rule: the first
// letter of the first two words, or the first two letters of a single
// word. Operates on runes so multi-byte names (e.g. non-Latin scripts)
// don't get truncated mid-character.
func guildInitials(name string) string {
	words := strings.Fields(name)
	switch len(words) {
	case 0:
		return ""
	case 1:
		r := []rune(words[0])
		if len(r) > 2 {
			r = r[:2]
		}
		return strings.ToUpper(string(r))
	default:
		a := []rune(words[0])[:1]
		b := []rune(words[1])[:1]
		return strings.ToUpper(string(a) + string(b))
	}
}

type ChannelView struct {
	ID   string
	Name string
}

// SessionView is the display-ready projection of a session for
// templates — nav shows the signed-in user (or just a sign-out link for
// a Trusted WEB_AUTH_TOKEN session, which has no Discord identity).
type SessionView struct {
	Username  string
	AvatarURL string
	Trusted   bool
}

func (srv *Server) sessionView(r *http.Request) *SessionView {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		return nil
	}
	return &SessionView{Username: sess.Username, AvatarURL: sess.AvatarURL, Trusted: sess.Trusted}
}

// mustSnowflake parses a Discord snowflake ID, returning the zero value
// (which never matches a real cache entry) on failure instead of an
// error — used where a bad ID should just look like "not found".
func mustSnowflake(s string) snowflake.ID {
	id, _ := snowflake.Parse(s)
	return id
}

// renderToast writes an OOB-swapped #toast fragment so htmx surfaces a
// rejection even though action handlers normally return a bare 204 and
// let the SSE stream (player.GuildPlayer.notify) render everything. Most
// of those forms carry no hx-swap of their own (204 means "swap
// nothing"), so without HX-Reswap: none here, htmx would swap this
// response's leftover (empty, once the OOB fragment is extracted) body
// into the form itself and blank out the button that was just clicked.
func (srv *Server) renderToast(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Reswap", "none")
	if err := srv.tmpl.ExecuteTemplate(w, "toast", message); err != nil {
		log.Printf("noctune: render toast: %v", err)
	}
}

// ProgressView is a display-ready projection of State.Position against
// the current track's duration. The client seeds a CSS keyframe animation
// with PositionSeconds as a negative animation-delay against TotalSeconds
// as its duration, so the bar is already at the right point and keeps
// moving with no polling and no JS-driven width writes.
type ProgressView struct {
	Visible         bool
	PercentStart    float64
	PositionSeconds float64
	TotalSeconds    float64
	ElapsedText     string
	TotalText       string
}

// playerSectionChanged reports whether #now-playing-controls (transport
// buttons, volume slider, loop-mode buttons, join/leave) and the
// #status-volume/#status-loop text spans need a fresh render.
func playerSectionChanged(prev *player.State, cur player.State) bool {
	return prev == nil ||
		prev.VoiceChannelID != cur.VoiceChannelID ||
		prev.Status != cur.Status ||
		prev.Volume != cur.Volume ||
		prev.Loop != cur.Loop ||
		prev.Current != cur.Current
}

// mediaSectionChanged reports whether #now-playing-media (art, EQ icon,
// title/artist, and the progress bar) needs a fresh render — only the
// fields it actually shows. Volume and Loop are deliberately not here:
// changing either would otherwise tear down and restart the art image
// and the running/wavy progress bar for no visible reason. The "· 68% ·
// loop queue" text embedded in that block's status line still stays
// correct on a volume- or loop-only change via the much smaller
// #status-volume/#status-loop spans playerSectionChanged drives instead
// (see panel-status-text in templates.html).
func mediaSectionChanged(prev *player.State, cur player.State) bool {
	return prev == nil ||
		prev.VoiceChannelID != cur.VoiceChannelID ||
		prev.Status != cur.Status ||
		prev.Current != cur.Current
}

func computeProgress(st player.State) ProgressView {
	if st.Current == nil || st.Current.Duration <= 0 {
		return ProgressView{}
	}
	if st.Status != player.StatusPlaying && st.Status != player.StatusPaused {
		return ProgressView{}
	}
	total := st.Current.Duration
	pos := max(min(st.Position, total), 0)
	pct := math.Round(float64(pos)/float64(total)*1000) / 10
	return ProgressView{
		Visible:         true,
		PercentStart:    pct,
		PositionSeconds: pos.Seconds(),
		TotalSeconds:    total.Seconds(),
		ElapsedText:     formatDuration(pos),
		TotalText:       formatDuration(total),
	}
}

func formatDuration(d time.Duration) string {
	total := int(d.Seconds())
	m, s := total/60, total%60
	return fmt.Sprintf("%d:%02d", m, s)
}

type PanelData struct {
	GuildID  string
	Channels []ChannelView
	State    player.State
	Progress ProgressView
	// ShowChannelPicker is true for sessions with no Discord identity to
	// resolve a "my channel" from (Trusted WEB_AUTH_TOKEN logins, or no
	// auth configured at all) — they get the old dropdown-of-any-channel
	// Join control. A Discord OAuth session instead gets a single "join
	// my channel" button; see handleJoin.
	ShowChannelPicker bool
}

type Server struct {
	cfg      *config.Config
	client   *bot.Client
	players  *player.Manager
	resolver *resolve.Resolver
	tmpl     *template.Template

	sessions *sessionStore
	oauthCfg *oauth2.Config
	avatars  *avatarCache

	searchMu sync.Mutex
	searchCh map[string]chan searchJob
}

func New(cfg *config.Config, client *bot.Client, players *player.Manager, resolver *resolve.Resolver) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}).ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	srv := &Server{
		cfg: cfg, client: client, players: players, resolver: resolver, tmpl: tmpl,
		sessions: newSessionStore(cfg.SessionSecret),
		avatars:  newAvatarCache(),
		searchCh: make(map[string]chan searchJob),
	}
	if cfg.DiscordOAuthEnabled() {
		srv.oauthCfg = newOAuthConfig(cfg.DiscordClientID, cfg.DiscordClientSecret, cfg.DiscordOAuthRedirectURL)
		if cfg.WebAuthToken != "" {
			log.Print("noctune: WEB_AUTH_TOKEN is set alongside Discord OAuth — token sign-ins get full control with no voice-channel restriction")
		}
	}
	return srv, nil
}

type searchJob struct {
	query                string
	requestedBy          string
	requestedByAvatarURL string
	generation           uint64
}

// queueSearch hands a query off to a per-guild worker and returns
// immediately — resolving a query (a yt-dlp lookup) can take seconds, and
// callers shouldn't block on it, or block each other: several people
// queuing tracks around the same time all get an instant response, while
// the worker resolves and enqueues them one at a time, in submission
// order, same as if they'd been typed in one after another.
func (srv *Server) queueSearch(guildID, query, requestedBy, requestedByAvatarURL string) {
	srv.searchMu.Lock()
	ch, ok := srv.searchCh[guildID]
	if !ok {
		ch = make(chan searchJob, 64)
		srv.searchCh[guildID] = ch
		go srv.runSearchWorker(guildID, ch)
	}
	srv.searchMu.Unlock()
	generation := srv.players.Get(guildID).ResolutionGeneration()
	ch <- searchJob{query: query, requestedBy: requestedBy, requestedByAvatarURL: requestedByAvatarURL, generation: generation}
}

func (srv *Server) runSearchWorker(guildID string, ch <-chan searchJob) {
	gp := srv.players.Get(guildID)
	for job := range ch {
		ctx, finish, ok := gp.BeginResolution(context.Background(), job.generation)
		if !ok {
			continue
		}
		isMulti := resolve.IsMultiTrack(job.query)
		if isMulti {
			gp.SetLoadingPlaylist(true)
		}
		err := srv.resolver.ResolveEach(ctx, job.query, job.requestedBy, job.requestedByAvatarURL, func(t *player.Track) error {
			return gp.EnqueueResolved(t, job.generation)
		})
		finish()
		if isMulti {
			gp.SetLoadingPlaylist(false)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("noctune: web resolve %q: %v", job.query, err)
		}
	}
}

func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	post := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc("POST "+pattern, srv.requireSameOrigin(handler))
	}

	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		log.Fatalf("noctune: static assets: %v", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	mux.HandleFunc("GET /avatar", srv.requireAuth(srv.handleAvatar))

	mux.HandleFunc("GET /login", srv.handleLoginPage)
	post("/login", srv.handleLoginSubmit)
	post("/logout", srv.handleLogout)
	if srv.oauthCfg != nil {
		mux.HandleFunc("GET /auth/discord/login", srv.handleDiscordLogin)
		mux.HandleFunc("GET /auth/discord/callback", srv.handleDiscordCallback)
	}

	mux.HandleFunc("GET /{$}", srv.requireAuth(srv.handleIndex))
	mux.HandleFunc("GET /g/{guildID}", srv.requireAuth(srv.requireGuildAccess(srv.handleGuildPage)))
	mux.HandleFunc("GET /g/{guildID}/events", srv.requireAuth(srv.requireGuildAccess(srv.handleEvents)))
	post("/g/{guildID}/join", srv.requireAuth(srv.requireGuildAccess(srv.handleJoin)))
	post("/g/{guildID}/leave", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleLeave))))
	post("/g/{guildID}/play", srv.requireAuth(srv.requireGuildAccess(srv.handlePlay)))
	mux.HandleFunc("GET /g/{guildID}/suggest", srv.requireAuth(srv.requireGuildAccess(srv.handleSuggest)))
	post("/g/{guildID}/pause", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handlePause))))
	post("/g/{guildID}/resume", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleResume))))
	post("/g/{guildID}/skip", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleSkip))))
	post("/g/{guildID}/stop", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleStop))))
	post("/g/{guildID}/volume", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleVolume))))
	post("/g/{guildID}/loop", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleLoop))))
	post("/g/{guildID}/queue/remove", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleQueueRemove))))
	post("/g/{guildID}/queue/play", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleQueuePlayNow))))
	post("/g/{guildID}/queue/reorder", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleQueueReorder))))
	post("/g/{guildID}/history/requeue", srv.requireAuth(srv.requireGuildAccess(srv.requireVoicePresence(srv.handleHistoryRequeue))))

	return mux
}

func (srv *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			site := r.Header.Get("Sec-Fetch-Site")
			if site == "" || site == "none" || site == "same-origin" {
				next(w, r)
				return
			}
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}

		u, err := url.Parse(origin)
		scheme := "http"
		if isSecureRequest(r) {
			scheme = "https"
		}
		if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) || u.Path != "" || u.User != nil {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// requireAuth is a no-op when neither Discord OAuth nor WEB_AUTH_TOKEN is
// configured. Set one of them whenever the web GUI isn't confined to a
// fully trusted network — otherwise anyone who can reach the port gets
// full control of the bot. When a session is established (either mode),
// it's attached to the request context for requireGuildAccess,
// requireVoicePresence, and the handlers themselves to read.
func (srv *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !srv.cfg.DiscordOAuthEnabled() && srv.cfg.WebAuthToken == "" {
			next(w, r)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			srv.redirectLogin(w, r)
			return
		}
		sess, ok := srv.sessions.get(c.Value)
		if !ok {
			srv.redirectLogin(w, r)
			return
		}
		next(w, r.WithContext(contextWithSession(r.Context(), sess)))
	}
}

func (srv *Server) redirectLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// requireGuildAccess is a no-op when there's no session (auth disabled
// entirely) or the session is Trusted (a WEB_AUTH_TOKEN login, which —
// like the old shared-token cookie — has no per-user identity to scope
// access by, so it sees every guild the bot is in). A Discord OAuth
// session only sees guilds in its AllowedGuildID set, computed at login
// as the intersection of the user's guilds and the bot's — see
// handleDiscordCallback.
func (srv *Server) requireGuildAccess(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r.Context())
		if sess != nil && !sess.canAccessGuild(r.PathValue("guildID")) {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// userVoiceChannel returns the voice channel sess's Discord user is
// currently in for guildID, if sess carries an identity (a Trusted or nil
// session has none to look up) and the user is in a channel at all.
func (srv *Server) userVoiceChannel(sess *session, guildID string) (string, bool) {
	if sess == nil || sess.Trusted {
		return "", false
	}
	gid, err := snowflake.Parse(guildID)
	if err != nil {
		return "", false
	}
	vs, ok := srv.client.Caches.VoiceState(gid, mustSnowflake(sess.DiscordUserID))
	if !ok || vs.ChannelID == nil {
		return "", false
	}
	return vs.ChannelID.String(), true
}

// requireVoicePresence rejects mutating playback actions unless the
// logged-in Discord user is currently sitting in the same voice channel
// the bot is connected to for this guild. It's a no-op when there's no
// session (auth disabled) or the session is Trusted (WEB_AUTH_TOKEN —
// see the package-level note on that mode's tradeoff in New). /play is
// deliberately not wrapped with this — see handlePlay — since it alone
// is allowed to establish the connection itself.
func (srv *Server) requireVoicePresence(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r.Context())
		if sess == nil || sess.Trusted {
			next(w, r)
			return
		}
		guildID := r.PathValue("guildID")
		botChannelID := srv.players.Get(guildID).Snapshot().VoiceChannelID
		if botChannelID == "" {
			srv.renderToast(w, "noctune isn't connected to a voice channel here.")
			return
		}
		channelID, ok := srv.userVoiceChannel(sess, guildID)
		if !ok || channelID != botChannelID {
			srv.renderToast(w, "Join the voice channel noctune is in to control playback.")
			return
		}
		next(w, r)
	}
}

type loginPageData struct {
	Error          string
	DiscordEnabled bool
	TokenEnabled   bool
	Session        *SessionView
}

func (srv *Server) loginPageData(r *http.Request, errMsg string) loginPageData {
	return loginPageData{
		Error:          errMsg,
		DiscordEnabled: srv.cfg.DiscordOAuthEnabled(),
		TokenEnabled:   srv.cfg.WebAuthToken != "",
		Session:        srv.sessionView(r),
	}
}

func (srv *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	srv.render(w, "page:login", srv.loginPageData(r, ""))
}

func (srv *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	if srv.cfg.WebAuthToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(srv.cfg.WebAuthToken)) != 1 {
		srv.render(w, "page:login", srv.loginPageData(r, "Invalid token."))
		return
	}
	sessID, err := srv.sessions.create(&session{Trusted: true})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessID,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout can only ever clear the cookie client-side — sessions are
// signed cookies with no server-side table to revoke an entry from (see
// sessionStore), so a copied cookie would stay valid until it naturally
// expires even after this.
func (srv *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// visibleGuilds returns every guild the bot is in, filtered down to the
// ones the current session is allowed to see (an untrusted OAuth session
// only sees guilds it shares a voice channel with the bot in — Trusted
// WEB_AUTH_TOKEN sessions and the no-auth case see everything).
func (srv *Server) visibleGuilds(r *http.Request) []GuildView {
	guilds := srv.listGuilds()
	if sess := sessionFromContext(r.Context()); sess != nil && !sess.Trusted {
		visible := make([]GuildView, 0, len(guilds))
		for _, g := range guilds {
			if sess.canAccessGuild(g.ID) {
				visible = append(visible, g)
			}
		}
		guilds = visible
	}
	return guilds
}

// handleIndex is only ever actually seen when a session has zero visible
// guilds (the bot isn't in any server yet, or an untrusted OAuth session
// isn't in voice anywhere it's allowed) — the sidebar on the guild page
// now covers switching between servers, so there's no reason to land on
// a picker page when there's somewhere to go straight to instead.
func (srv *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	guilds := srv.visibleGuilds(r)
	if len(guilds) > 0 {
		target := guilds[0]
		for _, g := range guilds {
			if g.Live {
				target = g
				break
			}
		}
		http.Redirect(w, r, "/g/"+target.ID, http.StatusSeeOther)
		return
	}
	srv.render(w, "page:index", struct {
		Guilds  []GuildView
		Session *SessionView
	}{Guilds: guilds, Session: srv.sessionView(r)})
}

// showChannelPicker reports whether the join control should be the old
// dropdown-of-any-channel (no session, or a Trusted WEB_AUTH_TOKEN
// session — neither has a Discord identity to resolve "my channel"
// from) rather than the OAuth session's single "join my channel" button.
func showChannelPicker(sess *session) bool {
	return sess == nil || sess.Trusted
}

func (srv *Server) handleGuildPage(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	id, err := snowflake.Parse(guildID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	g, ok := srv.client.Caches.Guild(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pd := srv.panelData(guildID, g, showChannelPicker(sessionFromContext(r.Context())))
	srv.render(w, "page:guild", struct {
		Guild   GuildView
		Guilds  []GuildView
		Panel   PanelData
		Session *SessionView
	}{Guild: guildView(g), Guilds: srv.visibleGuilds(r), Panel: pd, Session: srv.sessionView(r)})
}

func (srv *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	id, err := snowflake.Parse(guildID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := srv.client.Caches.Guild(id); !ok {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	gp := srv.players.Get(guildID)
	ch, cancel := gp.Subscribe()
	defer cancel()

	// Captured once per connection: a session doesn't change identity
	// mid-stream, so which join control to render doesn't either.
	showPicker := showChannelPicker(sessionFromContext(r.Context()))

	// Only re-render each fragment when something it actually shows has
	// changed. Queuing/removing/reordering tracks only ever touches
	// State.Queue, so most notify()s should just update #queue-section.
	// #now-playing-media (art, EQ icon, progress bar) only re-renders on
	// mediaSectionChanged, so a loop-mode or volume-only change doesn't
	// tear down and restart its running progress-bar animation for no
	// reason — that case instead only touches #now-playing-controls plus
	// the small #status-volume/#status-loop spans.
	var prev *player.State

	send := func(st player.State) {
		channels := voiceChannels(srv.client, id)
		pd := PanelData{GuildID: guildID, Channels: channels, State: st, Progress: computeProgress(st), ShowChannelPicker: showPicker}

		var buf bytes.Buffer
		if mediaSectionChanged(prev, st) {
			if err := srv.tmpl.ExecuteTemplate(&buf, "panel-media", pd); err != nil {
				log.Printf("noctune: render sse media panel: %v", err)
				return
			}
		}
		if playerSectionChanged(prev, st) {
			if err := srv.tmpl.ExecuteTemplate(&buf, "panel-controls", pd); err != nil {
				log.Printf("noctune: render sse controls panel: %v", err)
				return
			}
			if err := srv.tmpl.ExecuteTemplate(&buf, "panel-status-text", pd); err != nil {
				log.Printf("noctune: render sse status text: %v", err)
				return
			}
		}
		if err := srv.tmpl.ExecuteTemplate(&buf, "panel-queue", pd); err != nil {
			log.Printf("noctune: render sse queue panel: %v", err)
			return
		}
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
				return
			}
		}
		if _, err := fmt.Fprint(w, "\n"); err != nil {
			return
		}
		flusher.Flush()

		stCopy := st
		prev = &stCopy
	}

	send(gp.Snapshot())
	for {
		select {
		case st, ok := <-ch:
			if !ok {
				return
			}
			send(st)
		case <-r.Context().Done():
			return
		}
	}
}

// Action handlers only trigger a state change and return 204 (which
// htmx treats as "don't swap anything"). The SSE stream, driven by
// player.GuildPlayer.notify, is the single source of truth for what the
// panel renders — rendering here too would race the SSE push for the
// same DOM target.

// handleJoin either joins the channel the request names (Trusted
// sessions, which pick from the dropdown-of-any-channel) or, for a
// Discord OAuth session, resolves and joins whichever channel the
// logged-in user is currently sitting in themselves — "summon to my
// channel" rather than letting them park the bot somewhere they aren't.
func (srv *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	sess := sessionFromContext(r.Context())

	var channelID string
	if showChannelPicker(sess) {
		channelID = r.FormValue("channel_id")
	} else {
		var ok bool
		channelID, ok = srv.userVoiceChannel(sess, guildID)
		if !ok {
			srv.renderToast(w, "Join a voice channel in Discord first.")
			return
		}
	}

	if channelID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := srv.players.Get(guildID).Join(r.Context(), channelID); err != nil {
		srv.renderToast(w, fmt.Sprintf("Couldn't join voice channel: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	_ = srv.players.Get(guildID).Leave()
	w.WriteHeader(http.StatusNoContent)
}

// handlePlay is the one mutating action not wrapped by requireVoicePresence
// (see its registration in Handler and the note on requireVoicePresence
// itself): queueing a track is allowed to establish the bot's voice
// connection on the spot rather than requiring a separate Join click
// first, provided a Discord OAuth session's user is themselves currently
// in a voice channel — same "summon to my channel" identity handleJoin
// uses. A Trusted/no-identity session still has to join manually first,
// same as before, since there's no channel to resolve automatically.
func (srv *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	query := strings.TrimSpace(r.FormValue("query"))
	if query == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	gp := srv.players.Get(guildID)
	sess := sessionFromContext(r.Context())
	userChannelID, hasUserChannel := srv.userVoiceChannel(sess, guildID)
	// Read once: gp.Snapshot() again after the branch below decides could
	// observe a different value if another request's Join/Leave lands in
	// between, letting a stale read pass this check against a channel
	// that's no longer the one in play.
	botChannelID := gp.Snapshot().VoiceChannelID

	if botChannelID == "" {
		switch {
		case hasUserChannel:
			if err := gp.Join(r.Context(), userChannelID); err != nil {
				srv.renderToast(w, fmt.Sprintf("Couldn't join voice channel: %v", err))
				return
			}
		case showChannelPicker(sess):
			srv.renderToast(w, "Join a voice channel first, using the picker below.")
			return
		default:
			srv.renderToast(w, "Join a voice channel in Discord first.")
			return
		}
	} else if sess != nil && !sess.Trusted && (!hasUserChannel || userChannelID != botChannelID) {
		// Already connected somewhere, but this OAuth user isn't sitting
		// in that channel — same restriction requireVoicePresence applies
		// to every other action, just checked here instead since /play
		// alone skips that middleware.
		srv.renderToast(w, "Join the voice channel noctune is in to queue tracks.")
		return
	}

	requestedBy, requestedByAvatarURL := "web", ""
	if sess != nil && sess.Username != "" {
		requestedBy, requestedByAvatarURL = sess.Username, sess.AvatarURL
	}
	srv.queueSearch(guildID, query, requestedBy, requestedByAvatarURL)
	w.WriteHeader(http.StatusNoContent)
}

// handleSuggest backs the search box's live autocomplete: real YouTube
// search results (title, uploader, thumbnail), the same ones typing the
// query into YouTube directly and looking at the results page would
// surface. Pasted YouTube/Spotify links skip the lookup entirely — they
// aren't free-text queries, so there's nothing to suggest. Errors
// (yt-dlp hiccup, YouTube rate limit) degrade to an empty list rather
// than surfacing to the user; autocomplete is a nicety, not something
// worth a toast over.
func (srv *Server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	// htmx names a GET param after the triggering element's own name
	// attribute, and #query-input shares "query" with the form's POST
	// /play field — so this reads "query", not the "q" a hand-typed GET
	// would use.
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	var results []*youtube.Result
	if query != "" && !youtube.IsURL(query) {
		if _, _, ok := spotify.ParseURL(query); !ok {
			ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
			defer cancel()
			res, err := srv.resolver.Suggest(ctx, query, 6)
			if err != nil {
				log.Printf("noctune: suggest %q: %v", query, err)
			}
			results = res
		}
	}
	srv.render(w, "suggest-list", results)
}

func (srv *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	_ = srv.players.Get(guildID).Pause()
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	_ = srv.players.Get(guildID).Resume()
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	_ = srv.players.Get(guildID).Skip()
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	_ = srv.players.Get(guildID).Stop()
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleVolume(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	if level, err := strconv.Atoi(r.FormValue("level")); err == nil {
		_ = srv.players.Get(guildID).SetVolume(level)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleLoop(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	srv.players.Get(guildID).SetLoop(player.LoopMode(r.FormValue("mode")))
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleQueueRemove(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	_ = srv.players.Get(guildID).Remove(r.FormValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleQueuePlayNow(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	if err := srv.players.Get(guildID).PlayNow(r.FormValue("id")); err != nil {
		log.Printf("noctune: web play-now: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleQueueReorder applies a drag-and-drop reorder from the client.
// GuildPlayer.Reorder rejects an id list that doesn't exactly match the
// current queue — expected if someone else's action (an add, a remove)
// raced the drag — and the next notify() from that action already
// carries the correct order to every client, so there's nothing useful
// to surface to the user beyond letting the SSE push self-correct it.
func (srv *Server) handleQueueReorder(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := srv.players.Get(guildID).Reorder(r.Form["id"]); err != nil {
		log.Printf("noctune: web queue reorder: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleHistoryRequeue(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	requestedBy, requestedByAvatarURL := "web", ""
	if sess := sessionFromContext(r.Context()); sess != nil && sess.Username != "" {
		requestedBy, requestedByAvatarURL = sess.Username, sess.AvatarURL
	}
	if err := srv.players.Get(guildID).Requeue(r.FormValue("id"), requestedBy, requestedByAvatarURL); err != nil {
		srv.renderToast(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := srv.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("noctune: render %s: %v", name, err)
	}
}

func (srv *Server) panelData(guildID string, g discord.Guild, showPicker bool) PanelData {
	st := srv.players.Get(guildID).Snapshot()
	return PanelData{
		GuildID:           guildID,
		Channels:          voiceChannels(srv.client, g.ID),
		State:             st,
		Progress:          computeProgress(st),
		ShowChannelPicker: showPicker,
	}
}

func (srv *Server) listGuilds() []GuildView {
	var guilds []GuildView
	for g := range srv.client.Caches.Guilds() {
		gv := guildView(g)
		gv.Live = srv.players.Get(gv.ID).Snapshot().VoiceChannelID != ""
		guilds = append(guilds, gv)
	}
	sort.Slice(guilds, func(i, j int) bool { return guilds[i].Name < guilds[j].Name })
	return guilds
}

func guildView(g discord.Guild) GuildView {
	gv := GuildView{ID: g.ID.String(), Name: g.Name, Initials: guildInitials(g.Name)}
	if g.Icon != nil {
		gv.Icon = fmt.Sprintf("https://cdn.discordapp.com/icons/%s/%s.png", g.ID, *g.Icon)
	}
	return gv
}

func voiceChannels(client *bot.Client, guildID snowflake.ID) []ChannelView {
	var out []ChannelView
	for c := range client.Caches.Channels() {
		if c.GuildID() == guildID && c.Type() == discord.ChannelTypeGuildVoice {
			out = append(out, ChannelView{ID: c.ID().String(), Name: c.Name()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
