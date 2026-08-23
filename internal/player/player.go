// Package player owns per-guild playback state: the queue, the currently
// playing track, and the goroutine that drives one track after another
// through the audio pipeline. All state lives in memory only — there is
// no persistence, so a restart simply starts every guild back at idle.
package player

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/bot"
	voiceapi "github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/krezh/noctune/internal/audio"
	"github.com/krezh/noctune/internal/config"
)

type Source string

const (
	SourceYouTube Source = "youtube"
	SourceSpotify Source = "spotify"
)

type Status string

const (
	StatusIdle    Status = "idle"
	StatusLoading Status = "loading"
	StatusPlaying Status = "playing"
	StatusPaused  Status = "paused"
)

type LoopMode string

const (
	LoopOff   LoopMode = "off"
	LoopTrack LoopMode = "track"
	LoopQueue LoopMode = "queue"
)

var trackIDCounter atomic.Uint64

// Track is a queued or playing item. WatchURL is the stable YouTube watch
// URL used to resolve a fresh, playable stream just before playback —
// direct stream URLs from yt-dlp expire, so they are never stored here.
type Track struct {
	ID                   string
	Title                string
	Artist               string
	Album                string
	ArtworkURL           string
	Duration             time.Duration
	WatchURL             string
	SourceURL            string
	Source               Source
	RequestedBy          string
	RequestedByAvatarURL string
}

func NewTrack(title, artist, album, artworkURL, watchURL, sourceURL string, duration time.Duration, source Source, requestedBy, requestedByAvatarURL string) *Track {
	return &Track{
		ID:                   fmt.Sprintf("t%d", trackIDCounter.Add(1)),
		Title:                title,
		Artist:               artist,
		Album:                album,
		ArtworkURL:           artworkURL,
		Duration:             duration,
		WatchURL:             watchURL,
		SourceURL:            sourceURL,
		Source:               source,
		RequestedBy:          requestedBy,
		RequestedByAvatarURL: requestedByAvatarURL,
	}
}

// StreamResolver opens a live, readable audio stream for a track's
// stable watch URL. Implemented by internal/youtube, backed by a yt-dlp
// subprocess piping its own fetch of the audio into the returned
// ReadCloser — deliberately not a resolved URL handed to ffmpeg, since
// YouTube's CDN URLs are locked to the client that resolved them and
// ffmpeg fetching one independently gets cut off mid-stream.
type StreamResolver interface {
	OpenStream(ctx context.Context, watchURL string) (io.ReadCloser, error)
}

type State struct {
	GuildID        string
	VoiceChannelID string
	Status         Status
	Volume         int
	Loop           LoopMode
	Current        *Track
	Position       time.Duration
	Queue          []*Track
	// History is tracks that have started playing, most recent first,
	// capped at historyMaxSize. It's for display and re-adding a track —
	// nothing reads it to drive playback.
	History []*Track
}

// historyMaxSize bounds GuildPlayer.history — like everything else in
// noctune, it's memory-only and resets on restart.
const historyMaxSize = 50

type GuildPlayer struct {
	GuildID string

	client   *bot.Client
	resolver StreamResolver
	cfg      *config.Config

	mu             sync.Mutex
	voiceConn      voiceapi.Conn
	voiceChannelID string
	queue          []*Track
	current        *Track
	history        []*Track
	status         Status
	volume         int
	loop           LoopMode
	handle         *audio.Handle
	idleTimer      *time.Timer
	subs           map[chan State]struct{}

	startOnce  sync.Once
	playSignal chan struct{}
	stopLoopCh chan struct{}
}

type Manager struct {
	mu       sync.Mutex
	players  map[string]*GuildPlayer
	client   *bot.Client
	resolver StreamResolver
	cfg      *config.Config
}

func NewManager(client *bot.Client, resolver StreamResolver, cfg *config.Config) *Manager {
	return &Manager{
		players:  make(map[string]*GuildPlayer),
		client:   client,
		resolver: resolver,
		cfg:      cfg,
	}
}

// Get returns the player for a guild, creating it on first use.
func (m *Manager) Get(guildID string) *GuildPlayer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gp, ok := m.players[guildID]; ok {
		return gp
	}
	gp := &GuildPlayer{
		GuildID:    guildID,
		client:     m.client,
		resolver:   m.resolver,
		cfg:        m.cfg,
		volume:     m.cfg.DefaultVolume,
		loop:       LoopOff,
		status:     StatusIdle,
		subs:       make(map[chan State]struct{}),
		playSignal: make(chan struct{}, 1),
		stopLoopCh: make(chan struct{}),
	}
	m.players[guildID] = gp
	return gp
}

func (m *Manager) All() []*GuildPlayer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*GuildPlayer, 0, len(m.players))
	for _, gp := range m.players {
		out = append(out, gp)
	}
	return out
}

func (gp *GuildPlayer) Join(ctx context.Context, channelID string) error {
	gp.mu.Lock()
	if gp.voiceConn != nil && gp.voiceChannelID == channelID {
		gp.mu.Unlock()
		return nil
	}
	gp.mu.Unlock()

	chID, err := snowflake.Parse(channelID)
	if err != nil {
		return fmt.Errorf("parse channel id: %w", err)
	}
	guildID, err := snowflake.Parse(gp.GuildID)
	if err != nil {
		return fmt.Errorf("parse guild id: %w", err)
	}

	conn := gp.client.VoiceManager.CreateConn(guildID)
	if err := conn.Open(ctx, chID, false, true); err != nil {
		conn.Close(context.Background())
		return fmt.Errorf("join voice channel: %w", err)
	}

	gp.mu.Lock()
	gp.voiceConn = conn
	gp.voiceChannelID = channelID
	gp.startOnce.Do(func() { go gp.playbackLoop() })
	gp.mu.Unlock()
	gp.notify()
	return nil
}

func (gp *GuildPlayer) Leave() error {
	gp.mu.Lock()
	handle := gp.handle
	conn := gp.voiceConn
	gp.voiceConn = nil
	gp.voiceChannelID = ""
	gp.queue = nil
	gp.current = nil
	gp.status = StatusIdle
	gp.mu.Unlock()

	if handle != nil {
		handle.Stop()
	}
	if conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn.Close(ctx)
	}
	gp.notify()
	return nil
}

func (gp *GuildPlayer) Enqueue(track *Track) error {
	gp.mu.Lock()
	if len(gp.queue) >= gp.cfg.MaxQueueSize {
		gp.mu.Unlock()
		return fmt.Errorf("queue is full (max %d)", gp.cfg.MaxQueueSize)
	}
	gp.queue = append(gp.queue, track)
	gp.mu.Unlock()
	gp.notify()

	select {
	case gp.playSignal <- struct{}{}:
	default:
	}
	return nil
}

// Requeue finds historyID in history and enqueues a fresh copy of it —
// a new Track (and so a new ID) attributed to requestedBy, since the
// original's ID and requester belong to when it first played.
func (gp *GuildPlayer) Requeue(historyID, requestedBy, requestedByAvatarURL string) error {
	gp.mu.Lock()
	var src *Track
	for _, t := range gp.history {
		if t.ID == historyID {
			src = t
			break
		}
	}
	gp.mu.Unlock()
	if src == nil {
		return fmt.Errorf("track not found in history")
	}
	track := NewTrack(src.Title, src.Artist, src.Album, src.ArtworkURL, src.WatchURL, src.SourceURL, src.Duration, src.Source, requestedBy, requestedByAvatarURL)
	return gp.Enqueue(track)
}

func (gp *GuildPlayer) Skip() error {
	gp.mu.Lock()
	handle := gp.handle
	gp.mu.Unlock()
	if handle == nil {
		return fmt.Errorf("nothing is playing")
	}
	handle.Stop()
	return nil
}

// PlayNow moves a queued track to the front of the queue and skips the
// current one so playbackLoop picks it up next. Anything it jumped ahead
// of stays queued, just pushed back rather than dropped.
func (gp *GuildPlayer) PlayNow(id string) error {
	gp.mu.Lock()
	idx := -1
	for i, t := range gp.queue {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		gp.mu.Unlock()
		return fmt.Errorf("track not found in queue")
	}
	track := gp.queue[idx]
	gp.queue = append(gp.queue[:idx], gp.queue[idx+1:]...)
	gp.queue = append([]*Track{track}, gp.queue...)
	handle := gp.handle
	gp.mu.Unlock()
	gp.notify()

	if handle != nil {
		handle.Stop()
		return nil
	}
	select {
	case gp.playSignal <- struct{}{}:
	default:
	}
	return nil
}

func (gp *GuildPlayer) Pause() error {
	gp.mu.Lock()
	handle := gp.handle
	gp.mu.Unlock()
	if handle == nil {
		return fmt.Errorf("nothing is playing")
	}
	handle.Pause()
	gp.mu.Lock()
	gp.status = StatusPaused
	gp.mu.Unlock()
	gp.notify()
	return nil
}

func (gp *GuildPlayer) Resume() error {
	gp.mu.Lock()
	handle := gp.handle
	gp.mu.Unlock()
	if handle == nil {
		return fmt.Errorf("nothing is playing")
	}
	handle.Resume()
	gp.mu.Lock()
	gp.status = StatusPlaying
	gp.mu.Unlock()
	gp.notify()
	return nil
}

// Stop clears the queue and stops the current track. It does not leave
// the voice channel; use Leave for that.
func (gp *GuildPlayer) Stop() error {
	gp.mu.Lock()
	handle := gp.handle
	gp.queue = nil
	gp.loop = LoopOff
	gp.mu.Unlock()
	if handle != nil {
		handle.Stop()
	}
	gp.notify()
	return nil
}

// SetVolume changes playback volume. A currently playing track's encoder
// is adjusted live over its azmq control socket — see audio.Handle.SetVolume.
func (gp *GuildPlayer) SetVolume(volume int) error {
	if volume < 0 || volume > 100 {
		return fmt.Errorf("volume must be between 0 and 100")
	}
	gp.mu.Lock()
	gp.volume = volume
	handle := gp.handle
	gp.mu.Unlock()
	gp.notify()

	if handle != nil {
		if err := handle.SetVolume(volume); err != nil {
			return fmt.Errorf("change volume: %w", err)
		}
	}
	return nil
}

func (gp *GuildPlayer) SetLoop(mode LoopMode) {
	gp.mu.Lock()
	gp.loop = mode
	gp.mu.Unlock()
	gp.notify()
}

func (gp *GuildPlayer) Remove(id string) error {
	gp.mu.Lock()
	idx := -1
	for i, t := range gp.queue {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		gp.mu.Unlock()
		return fmt.Errorf("track not found in queue")
	}
	gp.queue = append(gp.queue[:idx], gp.queue[idx+1:]...)
	gp.mu.Unlock()
	gp.notify()
	return nil
}

func (gp *GuildPlayer) Clear() {
	gp.mu.Lock()
	gp.queue = nil
	gp.mu.Unlock()
	gp.notify()
}

// Reorder replaces the queue order. ids must contain exactly the IDs
// currently in the queue, in the desired new order.
func (gp *GuildPlayer) Reorder(ids []string) error {
	gp.mu.Lock()
	if len(ids) != len(gp.queue) {
		gp.mu.Unlock()
		return fmt.Errorf("reorder must include all %d queued tracks, got %d", len(gp.queue), len(ids))
	}
	byID := make(map[string]*Track, len(gp.queue))
	for _, t := range gp.queue {
		byID[t.ID] = t
	}
	seen := make(map[string]struct{}, len(ids))
	newQueue := make([]*Track, 0, len(ids))
	for _, id := range ids {
		t, ok := byID[id]
		if !ok {
			gp.mu.Unlock()
			return fmt.Errorf("unknown track id %q", id)
		}
		if _, dup := seen[id]; dup {
			gp.mu.Unlock()
			return fmt.Errorf("duplicate track id %q", id)
		}
		seen[id] = struct{}{}
		newQueue = append(newQueue, t)
	}
	gp.queue = newQueue
	gp.mu.Unlock()
	gp.notify()
	return nil
}

func (gp *GuildPlayer) Snapshot() State {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	q := make([]*Track, len(gp.queue))
	copy(q, gp.queue)
	h := make([]*Track, len(gp.history))
	copy(h, gp.history)
	var pos time.Duration
	if gp.handle != nil {
		pos = gp.handle.Position()
	}
	return State{
		GuildID:        gp.GuildID,
		VoiceChannelID: gp.voiceChannelID,
		Status:         gp.status,
		Volume:         gp.volume,
		Loop:           gp.loop,
		Current:        gp.current,
		Position:       pos,
		Queue:          q,
		History:        h,
	}
}

// Subscribe returns a channel that receives the latest state snapshot
// whenever it changes, and a cancel func to unsubscribe. The channel is
// single-slot: a slow consumer only ever sees the most recent state.
func (gp *GuildPlayer) Subscribe() (<-chan State, func()) {
	ch := make(chan State, 1)
	gp.mu.Lock()
	gp.subs[ch] = struct{}{}
	gp.mu.Unlock()
	cancel := func() {
		gp.mu.Lock()
		if _, ok := gp.subs[ch]; ok {
			delete(gp.subs, ch)
			close(ch)
		}
		gp.mu.Unlock()
	}
	return ch, cancel
}

func (gp *GuildPlayer) notify() {
	snap := gp.Snapshot()
	gp.mu.Lock()
	defer gp.mu.Unlock()
	for ch := range gp.subs {
		select {
		case ch <- snap:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- snap
		}
	}
}

func (gp *GuildPlayer) armIdleTimer() {
	if gp.cfg.IdleDisconnectSeconds <= 0 {
		return
	}
	gp.mu.Lock()
	defer gp.mu.Unlock()
	if gp.idleTimer != nil {
		gp.idleTimer.Stop()
	}
	gp.idleTimer = time.AfterFunc(time.Duration(gp.cfg.IdleDisconnectSeconds)*time.Second, func() {
		_ = gp.Leave()
	})
}

func (gp *GuildPlayer) cancelIdleTimer() {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	if gp.idleTimer != nil {
		gp.idleTimer.Stop()
		gp.idleTimer = nil
	}
}

// playbackLoop is the single goroutine that drives playback for a guild,
// started the first time the bot joins a voice channel there.
func (gp *GuildPlayer) playbackLoop() {
	for {
		gp.mu.Lock()
		if len(gp.queue) == 0 {
			gp.status = StatusIdle
			gp.current = nil
			gp.mu.Unlock()
			gp.notify()
			gp.armIdleTimer()
			select {
			case <-gp.playSignal:
				continue
			case <-gp.stopLoopCh:
				return
			}
		}
		track := gp.queue[0]
		gp.queue = gp.queue[1:]
		gp.current = track
		gp.status = StatusLoading
		voiceConn := gp.voiceConn
		volume := gp.volume
		gp.mu.Unlock()
		gp.cancelIdleTimer()
		gp.notify()

		if voiceConn == nil {
			log.Printf("noctune: guild %s has no voice connection, dropping %q", gp.GuildID, track.Title)
			continue
		}

		log.Printf("noctune: opening stream for %q (%s)", track.Title, track.WatchURL)
		stream, err := gp.resolver.OpenStream(context.Background(), track.WatchURL)
		if err != nil {
			log.Printf("noctune: open stream for %q: %v", track.Title, err)
			continue
		}
		log.Printf("noctune: stream opened for %q, starting encode", track.Title)

		handle, err := audio.Play(stream, audio.Options{Volume: volume})
		if err != nil {
			log.Printf("noctune: play %q: %v", track.Title, err)
			continue
		}
		voiceConn.SetOpusFrameProvider(handle)

		gp.mu.Lock()
		gp.handle = handle
		gp.status = StatusPlaying
		gp.history = append([]*Track{track}, gp.history...)
		if len(gp.history) > historyMaxSize {
			gp.history = gp.history[:historyMaxSize]
		}
		gp.mu.Unlock()
		gp.notify()

		playErr := <-handle.Done()
		if playErr != nil {
			log.Printf("noctune: playback error for %q: %v", track.Title, playErr)
		}

		gp.mu.Lock()
		gp.handle = nil
		switch {
		case playErr != nil:
			// dropped: don't requeue a track that failed to play
		case gp.loop == LoopTrack:
			gp.queue = append([]*Track{track}, gp.queue...)
		case gp.loop == LoopQueue:
			gp.queue = append(gp.queue, track)
		}
		gp.mu.Unlock()
		gp.notify()
	}
}
