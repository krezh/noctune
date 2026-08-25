// Package resolve turns whatever a user typed — free text, a YouTube
// link, or a Spotify track/album/playlist link — into queueable
// player.Track values. It is the one place internal/bot and internal/api
// share for this, so both surfaces behave identically. Spotify links only
// ever supply metadata: the audio for every track, Spotify-sourced or
// not, is always resolved from YouTube.
package resolve

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/krezh/noctune/internal/player"
	"github.com/krezh/noctune/internal/spotify"
	"github.com/krezh/noctune/internal/youtube"
)

type Resolver struct {
	spotify *spotify.Client
	youtube *youtube.Client
}

func New(sp *spotify.Client, yt *youtube.Client) *Resolver {
	return &Resolver{spotify: sp, youtube: yt}
}

// Resolve returns one or more tracks for a query. A Spotify track link
// returns one track; an album or playlist link returns every track it
// could match to YouTube audio (tracks with no match are skipped, not
// fatal). A YouTube link or free-text search always returns exactly one.
func (r *Resolver) Resolve(ctx context.Context, query, requestedBy, requestedByAvatarURL string) ([]*player.Track, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}

	if kind, id, ok := spotify.ParseURL(query); ok {
		switch kind {
		case spotify.KindTrack:
			st, err := r.spotify.ResolveTrack(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("resolve spotify track: %w", err)
			}
			t, err := r.spotifyToPlayerTrack(ctx, st, requestedBy, requestedByAvatarURL)
			if err != nil {
				return nil, err
			}
			return []*player.Track{t}, nil

		case spotify.KindAlbum:
			sts, err := r.spotify.ResolveAlbum(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("resolve spotify album: %w", err)
			}
			return r.spotifyBatchToPlayerTracks(ctx, sts, requestedBy, requestedByAvatarURL), nil

		case spotify.KindPlaylist:
			sts, err := r.spotify.ResolvePlaylist(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("resolve spotify playlist: %w", err)
			}
			return r.spotifyBatchToPlayerTracks(ctx, sts, requestedBy, requestedByAvatarURL), nil
		}
	}

	if youtube.IsURL(query) {
		if youtube.IsPlaylistURL(query) {
			results, err := r.youtube.ResolvePlaylist(ctx, query)
			if err != nil {
				return nil, fmt.Errorf("resolve youtube playlist: %w", err)
			}
			out := make([]*player.Track, 0, len(results))
			for _, res := range results {
				out = append(out, youtubeToPlayerTrack(res, requestedBy, requestedByAvatarURL))
			}
			return out, nil
		}
		res, err := r.youtube.Resolve(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("resolve youtube url: %w", err)
		}
		return []*player.Track{youtubeToPlayerTrack(res, requestedBy, requestedByAvatarURL)}, nil
	}

	res, err := r.youtube.Search(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search youtube: %w", err)
	}
	return []*player.Track{youtubeToPlayerTrack(res, requestedBy, requestedByAvatarURL)}, nil
}

// Suggest returns up to n lightweight YouTube search results (with
// thumbnails) for a partial free-text query. It backs the web GUI's live
// autocomplete dropdown, not queuing — callers still queue by feeding the
// chosen result's WatchURL through Resolve, same as any pasted link.
func (r *Resolver) Suggest(ctx context.Context, query string, n int) ([]*youtube.Result, error) {
	return r.youtube.SearchList(ctx, query, n)
}

func (r *Resolver) spotifyToPlayerTrack(ctx context.Context, st *spotify.Track, requestedBy, requestedByAvatarURL string) (*player.Track, error) {
	yr, err := r.youtube.Search(ctx, st.Artist+" "+st.Title)
	if err != nil {
		return nil, fmt.Errorf("find youtube audio for %q: %w", st.Title, err)
	}
	return player.NewTrack(st.Title, st.Artist, st.Album, st.ArtworkURL, yr.WatchURL, st.SourceURL, st.Duration, player.SourceSpotify, requestedBy, requestedByAvatarURL), nil
}

func (r *Resolver) spotifyBatchToPlayerTracks(ctx context.Context, sts []*spotify.Track, requestedBy, requestedByAvatarURL string) []*player.Track {
	out := make([]*player.Track, 0, len(sts))
	for _, st := range sts {
		t, err := r.spotifyToPlayerTrack(ctx, st, requestedBy, requestedByAvatarURL)
		if err != nil {
			log.Printf("noctune: skipping %q: %v", st.Title, err)
			continue
		}
		out = append(out, t)
	}
	return out
}

func youtubeToPlayerTrack(res *youtube.Result, requestedBy, requestedByAvatarURL string) *player.Track {
	return player.NewTrack(res.Title, res.Uploader, "", res.ThumbnailURL, res.WatchURL, res.WatchURL, res.Duration, player.SourceYouTube, requestedBy, requestedByAvatarURL)
}

// IsMultiTrack reports whether query will resolve to more than one track —
// a YouTube playlist URL or a Spotify album/playlist link.
func IsMultiTrack(query string) bool {
	if youtube.IsURL(query) && youtube.IsPlaylistURL(query) {
		return true
	}
	kind, _, ok := spotify.ParseURL(query)
	return ok && (kind == spotify.KindAlbum || kind == spotify.KindPlaylist)
}

// ResolveEach calls fn for each resolved track as soon as it's available.
// For YouTube playlists this streams one entry per yt-dlp output line; for
// Spotify albums/playlists it resolves metadata first then finds YouTube
// audio serially; everything else resolves fully then calls fn once.
func (r *Resolver) ResolveEach(ctx context.Context, query, requestedBy, requestedByAvatarURL string, fn func(*player.Track) error) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("empty query")
	}

	if youtube.IsURL(query) && youtube.IsPlaylistURL(query) {
		return r.youtube.ResolvePlaylistEach(ctx, query, func(res *youtube.Result) error {
			return fn(youtubeToPlayerTrack(res, requestedBy, requestedByAvatarURL))
		})
	}

	if kind, id, ok := spotify.ParseURL(query); ok && (kind == spotify.KindAlbum || kind == spotify.KindPlaylist) {
		var (
			sts []*spotify.Track
			err error
		)
		if kind == spotify.KindAlbum {
			sts, err = r.spotify.ResolveAlbum(ctx, id)
		} else {
			sts, err = r.spotify.ResolvePlaylist(ctx, id)
		}
		if err != nil {
			return fmt.Errorf("resolve spotify %s: %w", kind, err)
		}
		for _, st := range sts {
			if err := ctx.Err(); err != nil {
				return err
			}
			t, terr := r.spotifyToPlayerTrack(ctx, st, requestedBy, requestedByAvatarURL)
			if terr != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				log.Printf("noctune: skipping %q: %v", st.Title, terr)
				continue
			}
			if err := fn(t); err != nil {
				return err
			}
		}
		return nil
	}

	tracks, err := r.Resolve(ctx, query, requestedBy, requestedByAvatarURL)
	if err != nil {
		return err
	}
	for _, t := range tracks {
		if err := fn(t); err != nil {
			return err
		}
	}
	return nil
}
