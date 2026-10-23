package player

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const historyFilename = "history.db"

// HistoryEntry is a persisted track and its pagination cursor.
type HistoryEntry struct {
	Sequence int64
	Track    *Track
}

// HistoryPage contains one newest-first page and the total guild history size.
type HistoryPage struct {
	Entries    []HistoryEntry
	Total      int64
	NextCursor int64
}

type historyStore struct {
	db *sql.DB
}

func openHistoryStore(cacheDir string) (*historyStore, error) {
	dsn := ":memory:"
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return nil, fmt.Errorf("create cache directory: %w", err)
		}
		dsn = filepath.Join(cacheDir, historyFilename)
		file, err := os.OpenFile(dsn, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create history database: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close history database: %w", err)
		}
		if err := os.Chmod(dsn, 0o600); err != nil {
			return nil, fmt.Errorf("set history database permissions: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &historyStore{db: db}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *historyStore) initialize() error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS playback_history (
			sequence INTEGER PRIMARY KEY,
			guild_id TEXT NOT NULL,
			track_id TEXT NOT NULL,
			title TEXT NOT NULL,
			artist TEXT NOT NULL,
			album TEXT NOT NULL,
			artwork_url TEXT NOT NULL,
			duration_ns INTEGER NOT NULL,
			watch_url TEXT NOT NULL,
			source_url TEXT NOT NULL,
			source TEXT NOT NULL,
			requested_by TEXT NOT NULL,
			requested_by_avatar_url TEXT NOT NULL,
			played_at_ms INTEGER NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS playback_history_guild_sequence
			ON playback_history (guild_id, sequence DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize history database: %w", err)
		}
	}

	var maxTrackID uint64
	if err := s.db.QueryRow(`
		SELECT COALESCE(MAX(CAST(substr(track_id, 2) AS INTEGER)), 0)
		FROM playback_history
		WHERE track_id GLOB 't[0-9]*'
			AND substr(track_id, 2) NOT GLOB '*[^0-9]*'
	`).Scan(&maxTrackID); err != nil {
		return fmt.Errorf("read latest track ID: %w", err)
	}
	advanceTrackIDCounter(maxTrackID)
	return nil
}

func advanceTrackIDCounter(n uint64) {
	for {
		current := trackIDCounter.Load()
		if current >= n || trackIDCounter.CompareAndSwap(current, n) {
			return
		}
	}
}

func (s *historyStore) append(guildID string, track *Track) error {
	_, err := s.db.Exec(`
		INSERT INTO playback_history (
			guild_id, track_id, title, artist, album, artwork_url, duration_ns,
			watch_url, source_url, source, requested_by,
			requested_by_avatar_url, played_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		guildID,
		track.ID,
		track.Title,
		track.Artist,
		track.Album,
		track.ArtworkURL,
		int64(track.Duration),
		track.WatchURL,
		track.SourceURL,
		string(track.Source),
		track.RequestedBy,
		track.RequestedByAvatarURL,
		time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	return nil
}

func (s *historyStore) page(guildID string, before int64, limit int) (HistoryPage, error) {
	if limit <= 0 {
		return HistoryPage{}, fmt.Errorf("history page limit must be greater than zero")
	}
	limit = min(limit, 200)

	var page HistoryPage
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM playback_history WHERE guild_id = ?`,
		guildID,
	).Scan(&page.Total); err != nil {
		return HistoryPage{}, fmt.Errorf("count history: %w", err)
	}

	query := `
		SELECT sequence, track_id, title, artist, album, artwork_url, duration_ns,
			watch_url, source_url, source, requested_by, requested_by_avatar_url
		FROM playback_history
		WHERE guild_id = ?
		ORDER BY sequence DESC
		LIMIT ?
	`
	args := []any{guildID, limit + 1}
	if before > 0 {
		query = `
			SELECT sequence, track_id, title, artist, album, artwork_url, duration_ns,
				watch_url, source_url, source, requested_by, requested_by_avatar_url
			FROM playback_history
			WHERE guild_id = ? AND sequence < ?
			ORDER BY sequence DESC
			LIMIT ?
		`
		args = []any{guildID, before, limit + 1}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("query history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		entry, err := scanHistoryEntry(rows)
		if err != nil {
			return HistoryPage{}, err
		}
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, fmt.Errorf("read history: %w", err)
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		page.NextCursor = page.Entries[len(page.Entries)-1].Sequence
	}
	return page, nil
}

func (s *historyStore) find(guildID, trackID string) (*Track, error) {
	row := s.db.QueryRow(`
		SELECT sequence, track_id, title, artist, album, artwork_url, duration_ns,
			watch_url, source_url, source, requested_by, requested_by_avatar_url
		FROM playback_history
		WHERE guild_id = ? AND track_id = ?
		ORDER BY sequence DESC
		LIMIT 1
	`, guildID, trackID)
	entry, err := scanHistoryEntry(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("track not found in history")
		}
		return nil, err
	}
	return entry.Track, nil
}

type historyScanner interface {
	Scan(dest ...any) error
}

func scanHistoryEntry(scanner historyScanner) (HistoryEntry, error) {
	var (
		entry      HistoryEntry
		durationNS int64
		source     string
	)
	entry.Track = &Track{}
	if err := scanner.Scan(
		&entry.Sequence,
		&entry.Track.ID,
		&entry.Track.Title,
		&entry.Track.Artist,
		&entry.Track.Album,
		&entry.Track.ArtworkURL,
		&durationNS,
		&entry.Track.WatchURL,
		&entry.Track.SourceURL,
		&source,
		&entry.Track.RequestedBy,
		&entry.Track.RequestedByAvatarURL,
	); err != nil {
		return HistoryEntry{}, err
	}
	entry.Track.Duration = time.Duration(durationNS)
	entry.Track.Source = Source(source)
	return entry, nil
}

func (s *historyStore) close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}
