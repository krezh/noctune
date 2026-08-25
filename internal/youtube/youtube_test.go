package youtube

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenStreamReportsProcessFailureWithoutStderr(t *testing.T) {
	dir := t.TempDir()
	ytDlp := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(ytDlp, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}

	stream, err := New(ytDlp, "").OpenStream(context.Background(), "https://www.youtube.com/watch?v=test")
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
	c := New("yt-dlp", "")
	if got := c.cachePath("https://www.youtube.com/watch?v=abc"); got != "" {
		t.Fatalf("cachePath() = %q, want empty when cacheDir is unset", got)
	}
}

func TestCachePathStableAndDistinct(t *testing.T) {
	c := New("yt-dlp", "/cache")
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
