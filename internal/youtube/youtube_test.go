package youtube

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenStreamReportsProcessFailureWithoutStderr(t *testing.T) {
	dir := t.TempDir()
	ytDlp := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(ytDlp, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}

	stream, err := New(Options{YtDlpPath: ytDlp, MaxProcesses: 1}).OpenStream(context.Background(), "https://www.youtube.com/watch?v=test")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if err := stream.Close(); err == nil || !strings.Contains(err.Error(), "exit status 42") {
		t.Fatalf("Close() error = %v, want yt-dlp exit failure", err)
	}
}

func TestOpenStreamReportsProcessFailureWithStderrBeforeEOF(t *testing.T) {
	dir := t.TempDir()
	ytDlp := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(ytDlp, []byte("#!/bin/sh\necho 'HTTP Error 403: Forbidden' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}

	stream, err := New(Options{YtDlpPath: ytDlp, MaxProcesses: 1}).OpenStream(context.Background(), "https://www.youtube.com/watch?v=test")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	buf := make([]byte, 16)
	_, _ = stream.Read(buf)
	if err := stream.Close(); err == nil || !strings.Contains(err.Error(), "HTTP Error 403: Forbidden") {
		t.Fatalf("Close() error = %v, want yt-dlp stderr error message", err)
	}
}

func TestOpenStreamPassesExtractorArgs(t *testing.T) {
	dir := t.TempDir()
	ytDlp := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(ytDlp, []byte("#!/bin/sh\necho \"$*\"\n"), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}

	stream, err := New(Options{YtDlpPath: ytDlp, MaxProcesses: 1}).OpenStream(context.Background(), "https://www.youtube.com/watch?v=test")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(out), "youtube:player_client=web_embedded,android") {
		t.Errorf("OpenStream did not pass expected extractor args, got: %s", string(out))
	}
}

func TestCachingStreamFinalizesOnEOF(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "track.audio")
	content := "fake opus bytes"

	stream := newCachingStream(io.NopCloser(strings.NewReader(content)), finalPath)
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Fatalf("read content = %q, want %q", got, content)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cached, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("expected cache file at %s: %v", finalPath, err)
	}
	if string(cached) != content {
		t.Fatalf("cached content = %q, want %q", cached, content)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the finalized file in cache dir, got %v", entries)
	}
}

func TestCachingStreamDiscardsOnEarlyClose(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "track.audio")
	content := "fake opus bytes long enough to partially read"

	stream := newCachingStream(io.NopCloser(strings.NewReader(content)), finalPath)
	buf := make([]byte, 4)
	if _, err := stream.Read(buf); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("expected no cache file for a stream stopped before EOF, stat err = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected temp file to be cleaned up, got %v", entries)
	}
}

func TestCachePathEmptyWhenCachingDisabled(t *testing.T) {
	c := New(Options{YtDlpPath: "yt-dlp", MaxProcesses: 1})
	if got := c.cachePath("https://www.youtube.com/watch?v=abc"); got != "" {
		t.Fatalf("cachePath() = %q, want empty when cacheDir is unset", got)
	}
}

func TestCachePathStableAndDistinct(t *testing.T) {
	c := New(Options{YtDlpPath: "yt-dlp", CacheDir: t.TempDir(), CacheMaxBytes: 1024, MaxProcesses: 1})
	a := c.cachePath("https://www.youtube.com/watch?v=aaa")
	b := c.cachePath("https://www.youtube.com/watch?v=aaa")
	if a != b {
		t.Fatalf("cachePath() not stable for the same URL: %q != %q", a, b)
	}
	other := c.cachePath("https://www.youtube.com/watch?v=bbb")
	if a == other {
		t.Fatalf("cachePath() collided for different URLs: %q", a)
	}
}
func TestFailedDownloadIsNotCached(t *testing.T) {
	dir := t.TempDir()
	ytDlp := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(ytDlp, []byte("#!/bin/sh\nprintf 'partial audio'\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}
	client := New(Options{
		YtDlpPath:     ytDlp,
		CacheDir:      filepath.Join(dir, "cache"),
		CacheMaxBytes: 1024,
		MaxProcesses:  1,
	})
	watchURL := "https://www.youtube.com/watch?v=test"
	stream, err := client.OpenStream(context.Background(), watchURL)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if err := stream.Close(); err == nil {
		t.Fatal("Close() succeeded after yt-dlp failed")
	}
	if _, err := os.Stat(client.cachePath(watchURL)); !os.IsNotExist(err) {
		t.Fatalf("failed download was published to cache: %v", err)
	}
}

func TestCacheLimitEvictsOldestEntry(t *testing.T) {
	dir := t.TempDir()
	client := New(Options{CacheDir: dir, CacheMaxBytes: 5, MaxProcesses: 1})
	oldPath := filepath.Join(dir, "old.audio")
	newPath := filepath.Join(dir, "new.audio")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := client.enforceCacheLimit(); err != nil {
		t.Fatalf("enforceCacheLimit() error = %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("oldest cache entry was not evicted: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("newest cache entry was evicted: %v", err)
	}
}

func TestProcessLimitHonorsContext(t *testing.T) {
	client := New(Options{MaxProcesses: 1})
	release, err := client.acquireProcess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.acquireProcess(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireProcess() error = %v, want deadline exceeded", err)
	}
}
