// Package youtube resolves search queries and YouTube URLs to metadata,
// and resolves a stable watch URL to a short-lived direct audio stream
// URL. It shells out to yt-dlp rather than linking a library, since
// yt-dlp's extractors are updated far more frequently than any Go
// binding could track.
package youtube

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	YtDlpPath     string
	CacheDir      string
	CacheMaxBytes int64
	MaxProcesses  int
}

type Client struct {
	ytDlpPath     string
	cacheDir      string
	cacheMaxBytes int64
	processTokens chan struct{}
	cacheMu       sync.Mutex
}

// New creates a client. A zero CacheMaxBytes disables disk caching.
func New(opts Options) *Client {
	if opts.YtDlpPath == "" {
		opts.YtDlpPath = "yt-dlp"
	}
	if opts.MaxProcesses <= 0 {
		opts.MaxProcesses = 8
	}
	c := &Client{
		ytDlpPath:     opts.YtDlpPath,
		cacheDir:      opts.CacheDir,
		cacheMaxBytes: opts.CacheMaxBytes,
		processTokens: make(chan struct{}, opts.MaxProcesses),
	}
	if c.cacheDir != "" && c.cacheMaxBytes > 0 {
		if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
			log.Printf("youtube: cache dir: %v", err)
			c.cacheDir = ""
		} else {
			if err := c.cleanupStaleCacheTemps(); err != nil {
				log.Printf("youtube: clean stale cache temp files: %v", err)
			}
			if err := c.enforceCacheLimit(); err != nil {
				log.Printf("youtube: enforce cache limit: %v", err)
			}
		}
	}
	return c
}

func (c *Client) acquireProcess(ctx context.Context) (func(), error) {
	select {
	case c.processTokens <- struct{}{}:
		return func() { <-c.processTokens }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type Result struct {
	ID           string
	Title        string
	Uploader     string
	WatchURL     string
	ThumbnailURL string
	Duration     time.Duration
}

type ytDlpEntry struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Uploader   string  `json:"uploader"`
	Channel    string  `json:"channel"`
	Duration   float64 `json:"duration"`
	WebpageURL string  `json:"webpage_url"`
	Thumbnail  string  `json:"thumbnail"`
	// Thumbnails is only populated by --flat-playlist search results (see
	// SearchList) — a full run() lookup instead fills the singular
	// Thumbnail field above.
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
}

// toResult applies the same uploader/watch-URL/thumbnail fallbacks
// regardless of whether the entry came from a full run() lookup (which
// has Thumbnail but no Thumbnails) or a flat SearchList result (the
// reverse).
func (e ytDlpEntry) toResult() *Result {
	uploader := e.Uploader
	if uploader == "" {
		uploader = e.Channel
	}
	watchURL := e.WebpageURL
	if watchURL == "" {
		watchURL = "https://www.youtube.com/watch?v=" + e.ID
	}
	thumb := e.Thumbnail
	if thumb == "" && len(e.Thumbnails) > 0 {
		thumb = e.Thumbnails[0].URL
	}
	return &Result{
		ID:           e.ID,
		Title:        e.Title,
		Uploader:     uploader,
		WatchURL:     watchURL,
		ThumbnailURL: thumb,
		Duration:     time.Duration(e.Duration * float64(time.Second)),
	}
}

// IsURL reports whether input looks like a youtube.com/youtu.be link
// rather than a free-text search query.
func IsURL(input string) bool {
	u, err := url.Parse(strings.TrimSpace(input))
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	host = strings.TrimPrefix(host, "m.")
	return host == "youtube.com" || host == "youtu.be" || host == "music.youtube.com"
}

// IsPlaylistURL reports whether input is a bare YouTube playlist URL
// (youtube.com/playlist?list=…) as opposed to a watch URL that merely
// references a playlist alongside a video ID.
func IsPlaylistURL(input string) bool {
	u, err := url.Parse(strings.TrimSpace(input))
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	host = strings.TrimPrefix(host, "m.")
	if host != "youtube.com" && host != "music.youtube.com" {
		return false
	}
	return strings.TrimPrefix(u.Path, "/") == "playlist"
}

// ResolvePlaylistEach calls fn for each video in a YouTube playlist URL as
// yt-dlp outputs it, rather than buffering all results first. fn is called
// from the same goroutine in order; return from ResolvePlaylistEach means
// all entries have been delivered (or an error aborted the run).
func (c *Client) ResolvePlaylistEach(ctx context.Context, playlistURL string, fn func(*Result) error) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	release, err := c.acquireProcess(ctx)
	if err != nil {
		return err
	}
	defer release()

	cmd := exec.CommandContext(ctx, c.ytDlpPath,
		"-j",
		"--flat-playlist",
		"--no-warnings",
		"--skip-download",
		playlistURL,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("yt-dlp stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start yt-dlp: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry ytDlpEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if err := fn(entry.toResult()); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("yt-dlp playlist read: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("yt-dlp playlist %q: %w: %s", playlistURL, err, stderr.String())
		}
		return fmt.Errorf("yt-dlp playlist %q: %w", playlistURL, err)
	}
	return nil
}

// ResolvePlaylist returns all videos in a YouTube playlist URL. For
// streaming results as they arrive, use ResolvePlaylistEach instead.
func (c *Client) ResolvePlaylist(ctx context.Context, playlistURL string) ([]*Result, error) {
	var out []*Result
	if err := c.ResolvePlaylistEach(ctx, playlistURL, func(r *Result) error {
		out = append(out, r)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Search returns metadata for the top result of a free-text query.
func (c *Client) Search(ctx context.Context, query string) (*Result, error) {
	return c.run(ctx, "ytsearch1:"+query)
}

// SearchList returns up to n lightweight results for a free-text query,
// each with a thumbnail, to back the web GUI's live autocomplete
// dropdown. It uses yt-dlp's --flat-playlist extraction, which parses the
// search-results page itself instead of resolving every video
// individually — unlike Search (used only at actual queue time), that's
// fast enough to run once per debounced keystroke pause.
func (c *Client) SearchList(ctx context.Context, query string, n int) ([]*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	release, err := c.acquireProcess(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	cmd := exec.CommandContext(ctx, c.ytDlpPath,
		"-j",
		"--flat-playlist",
		"--no-warnings",
		"--skip-download",
		fmt.Sprintf("ytsearch%d:%s", n, query),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp search %q: %w: %s", query, err, stderr.String())
	}

	var results []*Result
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var entry ytDlpEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		results = append(results, entry.toResult())
	}
	return results, nil
}

// Resolve returns metadata for a direct YouTube URL.
func (c *Client) Resolve(ctx context.Context, watchURL string) (*Result, error) {
	return c.run(ctx, watchURL)
}

// OpenStream implements player.StreamResolver: it starts a yt-dlp process
// that downloads and streams a track's audio to its stdout, and returns
// that as a ReadCloser. This is deliberately not "resolve a URL and let
// ffmpeg fetch it directly" — YouTube's CDN URLs are signature/throttling
// -locked to the client that resolved them, so ffmpeg fetching one
// independently gets cut off mid-stream. Piping yt-dlp's own fetch avoids
// that entirely. Closing the returned stream kills the yt-dlp process;
// callers must always close it, even on error mid-read, to avoid leaking
// processes.
//
// If caching is enabled, a watch URL already fully downloaded once is
// served straight from disk with no yt-dlp process at all; otherwise the
// fresh yt-dlp stream is tee'd to disk as it plays so the next OpenStream
// for the same watch URL is a cache hit.
func (c *Client) OpenStream(ctx context.Context, watchURL string) (io.ReadCloser, error) {
	path := c.cachePath(watchURL)
	if path != "" {
		if f, ok := c.openCached(path); ok {
			return f, nil
		}
	}

	release, err := c.acquireProcess(ctx)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, c.ytDlpPath,
		"--extractor-args", "youtube:player_client=web_embedded,android",
		"-f", "bestaudio/best",
		"-o", "-",
		"--no-playlist",
		"--no-warnings",
		"--quiet",
		watchURL,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		return nil, fmt.Errorf("yt-dlp stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		release()
		return nil, fmt.Errorf("start yt-dlp stream: %w", err)
	}

	var stream io.ReadCloser = &processStream{ReadCloser: stdout, cmd: cmd, stderr: &stderr, release: release}
	if path != "" {
		stream = newCachingStreamWithFinalize(stream, path, c.finalizeCache)
	}
	return stream, nil
}

// cachePath returns the cache location for watchURL, or "" when disabled.
func (c *Client) cachePath(watchURL string) string {
	if c.cacheDir == "" || c.cacheMaxBytes <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(watchURL))
	return filepath.Join(c.cacheDir, hex.EncodeToString(sum[:])+".audio")
}

func (c *Client) openCached(path string) (io.ReadCloser, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		log.Printf("youtube: update cache access time: %v", err)
	}
	return f, true
}

func (c *Client) finalizeCache(tmpPath, finalPath string) error {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	if err := c.enforceCacheLimitLocked(); err != nil {
		log.Printf("youtube: enforce cache limit: %v", err)
	}
	return nil
}

func (c *Client) enforceCacheLimit() error {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	return c.enforceCacheLimitLocked()
}
func (c *Client) cleanupStaleCacheTemps() error {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.Contains(entry.Name(), ".audio.tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(c.cacheDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *Client) enforceCacheLimitLocked() error {
	dirEntries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return err
	}
	entries := make([]cacheEntry, 0, len(dirEntries))
	var total int64
	for _, entry := range dirEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".audio") {
			continue
		}
		path := filepath.Join(c.cacheDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		entries = append(entries, cacheEntry{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modTime.Before(entries[j].modTime)
	})
	for _, entry := range entries {
		if total <= c.cacheMaxBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= entry.size
	}
	return nil
}

// cachingStream publishes only a complete, successfully downloaded stream.
type cachingStream struct {
	io.ReadCloser
	mu         sync.Mutex
	tmp        *os.File
	finalPath  string
	finalize   func(string, string) error
	reachedEOF bool
	writeErr   error
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

func newCachingStream(src io.ReadCloser, finalPath string) io.ReadCloser {
	return newCachingStreamWithFinalize(src, finalPath, os.Rename)
}

func newCachingStreamWithFinalize(src io.ReadCloser, finalPath string, finalize func(string, string) error) io.ReadCloser {
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		log.Printf("youtube: cache dir: %v", err)
		return src
	}
	tmp, err := os.CreateTemp(filepath.Dir(finalPath), filepath.Base(finalPath)+".tmp*")
	if err != nil {
		log.Printf("youtube: cache create temp file: %v", err)
		return src
	}
	return &cachingStream{ReadCloser: src, tmp: tmp, finalPath: finalPath, finalize: finalize}
}

func (c *cachingStream) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > 0 && !c.closed && c.writeErr == nil {
		if _, werr := c.tmp.Write(p[:n]); werr != nil {
			c.writeErr = werr
			log.Printf("youtube: cache write: %v", werr)
		}
	}
	if err == io.EOF {
		c.reachedEOF = true
	}
	return n, err
}

func (c *cachingStream) Close() error {
	c.closeOnce.Do(func() {
		closeErr := c.ReadCloser.Close()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.closed = true
		c.closeErr = closeErr
		_ = c.tmp.Close()
		if c.reachedEOF && c.writeErr == nil && closeErr == nil {
			if err := c.finalize(c.tmp.Name(), c.finalPath); err == nil {
				return
			} else {
				log.Printf("youtube: cache finalize: %v", err)
			}
		}
		if err := os.Remove(c.tmp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("youtube: remove cache temp file: %v", err)
		}
	})
	return c.closeErr
}

// processStream ties the lifetime of a yt-dlp subprocess to its stdout
// pipe: closing it (or draining it to EOF and then closing it) always
// stops the process, so callers can't leak one by forgetting a separate
// cleanup step.
type processStream struct {
	io.ReadCloser
	cmd        *exec.Cmd
	stderr     *bytes.Buffer
	release    func()
	reachedEOF atomic.Bool
	closeOnce  sync.Once
	closeErr   error
}

func (p *processStream) Read(buf []byte) (int, error) {
	n, err := p.ReadCloser.Read(buf)
	if errors.Is(err, io.EOF) {
		p.reachedEOF.Store(true)
	}
	return n, err
}

func (p *processStream) Close() error {
	p.closeOnce.Do(func() {
		if p.release != nil {
			defer p.release()
		}
		closeErr := p.ReadCloser.Close()
		naturalEOF := p.reachedEOF.Load()
		if !naturalEOF {
			_ = p.cmd.Process.Kill()
		}
		waitErr := p.cmd.Wait()
		switch {
		case closeErr != nil:
			p.closeErr = closeErr
		case waitErr != nil && p.stderr.Len() > 0:
			p.closeErr = fmt.Errorf("yt-dlp: %w: %s", waitErr, strings.TrimSpace(p.stderr.String()))
		case naturalEOF && waitErr != nil:
			p.closeErr = fmt.Errorf("yt-dlp: %w", waitErr)
		}
	})
	return p.closeErr
}

func (c *Client) run(ctx context.Context, target string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	release, err := c.acquireProcess(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	cmd := exec.CommandContext(ctx, c.ytDlpPath,
		"-j",
		"--no-playlist",
		"--no-warnings",
		"--skip-download",
		"--extractor-args", "youtube:player_client=web_embedded,android",
		target,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp lookup %q: %w: %s", target, err, stderr.String())
	}

	// ytsearch1: can still emit nothing if there were no results.
	line := strings.TrimSpace(stdout.String())
	if line == "" {
		return nil, fmt.Errorf("no results for %q", target)
	}
	if idx := strings.IndexByte(line, '\n'); idx != -1 {
		line = line[:idx]
	}

	var entry ytDlpEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	return entry.toResult(), nil
}
