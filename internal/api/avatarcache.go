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
}

// avatarCache proxies and caches Discord CDN avatar images in memory so
// the browser fetches a given requester's avatar from noctune once, not
// once per queue/history row it's shown in (and not once per page load
// on a fresh device/browser, unlike a plain hotlink to Discord's CDN).
// Not persisted to disk — like the rest of noctune's state, a restart
// just means the next request refetches and re-caches it.
type avatarCache struct {
	mu    sync.RWMutex
	byURL map[string]cachedAvatar
}

func newAvatarCache() *avatarCache {
	return &avatarCache{byURL: make(map[string]cachedAvatar)}
}

func (c *avatarCache) get(url string) (cachedAvatar, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.byURL[url]
	return a, ok
}

func (c *avatarCache) put(url string, a cachedAvatar) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byURL[url] = a
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
