package player

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

const (
	persistDebounce = 500 * time.Millisecond
	// resumeTailSkip drops a restored track instead of resuming it when this
	// little of it was left.
	resumeTailSkip = 3 * time.Second
)

type savedGuild struct {
	VoiceChannelID string        `json:"voiceChannelId,omitempty"`
	Volume         int           `json:"volume"`
	Loop           LoopMode      `json:"loop"`
	Current        *Track        `json:"current,omitempty"`
	Position       time.Duration `json:"positionNs,omitempty"`
	Queue          []*Track      `json:"queue,omitempty"`
	Loads          []LoadSpec    `json:"loads,omitempty"`
}

func (s *historyStore) saveGuild(guildID string, state savedGuild) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode player state: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO guild_state (guild_id, state, updated_at_ms) VALUES (?, ?, ?)
		ON CONFLICT (guild_id) DO UPDATE SET state = excluded.state, updated_at_ms = excluded.updated_at_ms
	`, guildID, string(data), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("save player state: %w", err)
	}
	return nil
}

func (s *historyStore) loadGuilds() (map[string]savedGuild, error) {
	rows, err := s.db.Query(`SELECT guild_id, state FROM guild_state`)
	if err != nil {
		return nil, fmt.Errorf("query player state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]savedGuild)
	for rows.Next() {
		var guildID, data string
		if err := rows.Scan(&guildID, &data); err != nil {
			return nil, fmt.Errorf("read player state: %w", err)
		}
		var state savedGuild
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			log.Printf("noctune: discard unreadable player state for guild %s: %v", guildID, err)
			continue
		}
		out[guildID] = state
		for _, t := range state.Queue {
			advanceTrackIDCounter(trackIDNumber(t))
		}
		advanceTrackIDCounter(trackIDNumber(state.Current))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read player state: %w", err)
	}
	return out, nil
}

func trackIDNumber(t *Track) uint64 {
	if t == nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(t.ID, "t"), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// restore applies state saved by a previous process. The interrupted track
// goes back to the front of the queue, resuming where it stopped.
func (gp *GuildPlayer) restore(s savedGuild) {
	gp.volume = s.Volume
	if s.Loop != "" {
		gp.loop = s.Loop
	}
	gp.queue = s.Queue
	if c := s.Current; c != nil && (c.Duration <= 0 || s.Position < c.Duration-resumeTailSkip) {
		c.startAt = s.Position
		gp.queue = append([]*Track{c}, gp.queue...)
	}
	gp.restoreChannelID = s.VoiceChannelID
	gp.restoredLoads = s.Loads
}

// TakeRestoreChannel returns the voice channel the player was in before the
// last restart, once; "" if there is none or it has since joined one.
func (gp *GuildPlayer) TakeRestoreChannel() string {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	if len(gp.queue) == 0 && len(gp.restoredLoads) == 0 {
		gp.restoreChannelID = ""
	}
	ch := gp.restoreChannelID
	gp.restoreChannelID = ""
	return ch
}

func (gp *GuildPlayer) savedLocked() savedGuild {
	s := savedGuild{
		VoiceChannelID: gp.voiceChannelID,
		Volume:         gp.volume,
		Loop:           gp.loop,
		Current:        gp.current,
		Queue:          append([]*Track(nil), gp.queue...),
		Loads:          append([]LoadSpec(nil), gp.restoredLoads...),
	}
	if s.VoiceChannelID == "" {
		s.VoiceChannelID = gp.restoreChannelID
	}
	if gp.handle != nil {
		s.Position = gp.handle.Position()
	}
	for _, spec := range gp.loads {
		s.Loads = append(s.Loads, *spec)
	}
	return s
}

func (gp *GuildPlayer) persist() {
	gp.persistMu.Lock()
	defer gp.persistMu.Unlock()
	gp.persistLocked()
}

// freeze saves the player's state one last time and stops further saves,
// so shutting the player down doesn't overwrite it with an empty queue.
func (gp *GuildPlayer) freeze() {
	gp.persistMu.Lock()
	defer gp.persistMu.Unlock()
	gp.persistLocked()
	gp.persistFrozen = true
}

func (gp *GuildPlayer) persistLocked() {
	if gp.historyStore == nil || gp.persistFrozen {
		return
	}
	gp.mu.Lock()
	s := gp.savedLocked()
	gp.mu.Unlock()
	if err := gp.historyStore.saveGuild(gp.GuildID, s); err != nil {
		log.Printf("noctune: guild %s: %v", gp.GuildID, err)
	}
}

func (gp *GuildPlayer) persistLoop() {
	for {
		select {
		case <-gp.saveCh:
		case <-gp.stopLoopCh:
			return
		}
		select {
		case <-time.After(persistDebounce):
		case <-gp.stopLoopCh:
			return
		}
		gp.persist()
	}
}
