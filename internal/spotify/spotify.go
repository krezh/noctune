// Package spotify resolves Spotify links (and free-text search) to track
// metadata using the Web API's client-credentials flow — app-only auth,
// no user login, which is what keeps this stateless. Spotify's API never
// hands back playable audio; internal/bot and internal/api take the
// metadata this package returns and resolve the actual audio from
// YouTube via internal/youtube.
package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	spotifyapi "github.com/zmb3/spotify/v2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	KindTrack    = "track"
	KindAlbum    = "album"
	KindPlaylist = "playlist"

	pageLimit = 50
)

type Track struct {
	Title      string
	Artist     string
	Album      string
	ArtworkURL string
	Duration   time.Duration
	SourceURL  string
}

type Client struct {
	api *spotifyapi.Client
}

func New(ctx context.Context, clientID, clientSecret string) *Client {
	cfg := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     "https://accounts.spotify.com/api/token",
	}
	return &Client{api: spotifyapi.New(cfg.Client(ctx))}
}

// ParseURL recognizes open.spotify.com links and spotify: URIs, returning
// the entity kind ("track", "album", "playlist") and its ID.
func ParseURL(input string) (kind, id string, ok bool) {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "spotify:") {
		parts := strings.Split(input, ":")
		if len(parts) == 3 && isKnownKind(parts[1]) {
			return parts[1], parts[2], true
		}
		return "", "", false
	}
	u, err := url.Parse(input)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host != "open.spotify.com" {
		return "", "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) != 2 || !isKnownKind(segs[0]) {
		return "", "", false
	}
	return segs[0], segs[1], true
}

func isKnownKind(k string) bool {
	return k == KindTrack || k == KindAlbum || k == KindPlaylist
}

func (c *Client) ResolveTrack(ctx context.Context, id string) (*Track, error) {
	t, err := c.api.GetTrack(ctx, spotifyapi.ID(id))
	if err != nil {
		return nil, fmt.Errorf("get track: %w", err)
	}
	return fromFullTrack(*t), nil
}

func (c *Client) ResolveAlbum(ctx context.Context, id string) ([]*Track, error) {
	album, err := c.api.GetAlbum(ctx, spotifyapi.ID(id))
	if err != nil {
		return nil, fmt.Errorf("get album: %w", err)
	}
	artwork := bestImage(album.Images)

	var out []*Track
	offset := 0
	for {
		page, err := c.api.GetAlbumTracks(ctx, spotifyapi.ID(id), spotifyapi.Limit(pageLimit), spotifyapi.Offset(offset))
		if err != nil {
			return nil, fmt.Errorf("get album tracks: %w", err)
		}
		for _, t := range page.Tracks {
			out = append(out, &Track{
				Title:      t.Name,
				Artist:     artistNames(t.Artists),
				Album:      album.Name,
				ArtworkURL: artwork,
				Duration:   t.TimeDuration(),
				SourceURL:  "https://open.spotify.com/track/" + string(t.ID),
			})
		}
		if len(page.Tracks) < pageLimit {
			break
		}
		offset += pageLimit
	}
	return out, nil
}

func (c *Client) ResolvePlaylist(ctx context.Context, id string) ([]*Track, error) {
	var out []*Track
	offset := 0
	for {
		page, err := c.api.GetPlaylistItems(ctx, spotifyapi.ID(id), spotifyapi.Limit(pageLimit), spotifyapi.Offset(offset))
		if err != nil {
			return nil, fmt.Errorf("get playlist items: %w", err)
		}
		for _, item := range page.Items {
			if item.IsLocal || item.Track.Track == nil {
				continue
			}
			out = append(out, fromFullTrack(*item.Track.Track))
		}
		if len(page.Items) < pageLimit {
			break
		}
		offset += pageLimit
	}
	return out, nil
}

// Search returns up to limit track matches for a free-text query.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]*Track, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	res, err := c.api.Search(ctx, query, spotifyapi.SearchTypeTrack, spotifyapi.Limit(limit))
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if res.Tracks == nil {
		return nil, nil
	}
	out := make([]*Track, 0, len(res.Tracks.Tracks))
	for _, t := range res.Tracks.Tracks {
		out = append(out, fromFullTrack(t))
	}
	return out, nil
}

func fromFullTrack(t spotifyapi.FullTrack) *Track {
	return &Track{
		Title:      t.Name,
		Artist:     artistNames(t.Artists),
		Album:      t.Album.Name,
		ArtworkURL: bestImage(t.Album.Images),
		Duration:   t.TimeDuration(),
		SourceURL:  "https://open.spotify.com/track/" + string(t.ID),
	}
}

func artistNames(artists []spotifyapi.SimpleArtist) string {
	names := make([]string, len(artists))
	for i, a := range artists {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

// bestImage returns the widest image, which is what Spotify's API
// guarantees is first in the slice.
func bestImage(images []spotifyapi.Image) string {
	if len(images) == 0 {
		return ""
	}
	return images[0].URL
}
