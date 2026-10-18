package api

import (
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// discordAvatarURLPattern strictly allowlists what handleAvatar will ever
// fetch — a real Discord CDN avatar URL, nothing else. Without this,
// proxying a caller-supplied "src" would be an open SSRF vector; with
// it, the server only ever makes requests it would already trust.
var discordAvatarURLPattern = regexp.MustCompile(`^https://cdn\.discordapp\.com/avatars/\d+/(?:a_)?[0-9a-fA-F]+\.(?:png|webp|jpg|jpeg|gif)$`)

type cachedAvatar struct {
	data        []byte
	contentType string
	usedAt      uint64
}

type avatarCache struct {
	mu       sync.Mutex
	byURL    map[string]cachedAvatar
	maxBytes int64
	bytes    int64
	tick     uint64
}

func newAvatarCache(maxBytes int64) *avatarCache {
	return &avatarCache{byURL: make(map[string]cachedAvatar), maxBytes: maxBytes}
}

func (c *avatarCache) get(url string) (cachedAvatar, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.byURL[url]
	if !ok {
		return cachedAvatar{}, false
	}
	c.tick++
	a.usedAt = c.tick
	c.byURL[url] = a
	return a, true
}

func (c *avatarCache) put(url string, a cachedAvatar) {
	size := int64(len(a.data))
	if c.maxBytes <= 0 || size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byURL[url]; ok {
		c.bytes -= int64(len(old.data))
	}
	c.tick++
	a.usedAt = c.tick
	c.byURL[url] = a
	c.bytes += size
	for c.bytes > c.maxBytes {
		var oldestURL string
		var oldest cachedAvatar
		for candidateURL, candidate := range c.byURL {
			if oldestURL == "" || candidate.usedAt < oldest.usedAt {
				oldestURL, oldest = candidateURL, candidate
			}
		}
		delete(c.byURL, oldestURL)
		c.bytes -= int64(len(oldest.data))
	}
}

// avatarFetchTimeout bounds a single upstream fetch to Discord's CDN so
// a slow/unreachable CDN can't hang the request indefinitely.
const avatarFetchTimeout = 10 * time.Second

// avatarMaxBytes caps how much of the response body handleAvatar will
// read — comfortably larger than any real Discord avatar image, small
// enough that a misbehaving/malicious response can't exhaust memory.
const avatarMaxBytes = 2 << 20 // 2 MiB

func (srv *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	if !discordAvatarURLPattern.MatchString(src) {
		http.Error(w, "invalid avatar url", http.StatusBadRequest)
		return
	}

	if a, ok := srv.avatars.get(src); ok {
		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if _, err := w.Write(a.data); err != nil {
			log.Printf("noctune: write cached avatar response: %v", err)
		}
		return
	}

	client := http.Client{Timeout: avatarFetchTimeout}
	resp, err := client.Get(src)
	if err != nil {
		http.Error(w, "avatar fetch failed", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("noctune: close avatar fetch response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "avatar fetch failed", http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes))
	if err != nil {
		http.Error(w, "avatar fetch failed", http.StatusBadGateway)
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/png"
	}

	a := cachedAvatar{data: data, contentType: contentType}
	srv.avatars.put(src, a)

	w.Header().Set("Content-Type", a.contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if _, err := w.Write(a.data); err != nil {
		log.Printf("noctune: write avatar response: %v", err)
	}
}
