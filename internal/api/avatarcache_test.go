package api

import "testing"

func TestDiscordAvatarURLPattern(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://cdn.discordapp.com/avatars/123456789012345678/abcdef0123456789abcdef0123456789.png", true},
		{"https://cdn.discordapp.com/avatars/1/a_1234567890abcdef1234567890abcdef.gif", true},
		{"https://cdn.discordapp.com/avatars/1/abc.webp", true},
		{"https://cdn.discordapp.com/avatars/1/abc.jpg", true},
		// not a per-user avatar URL at all
		{"https://cdn.discordapp.com/embed/avatars/0.png", false},
		{"https://cdn.discordapp.com/icons/123/abc.png", false},
		// wrong host — the exact shape an SSRF attempt would take
		{"https://evil.example.com/avatars/1/abc.png", false},
		{"https://cdn.discordapp.com.evil.example.com/avatars/1/abc.png", false},
		{"http://cdn.discordapp.com/avatars/1/abc.png", false}, // not https
		{"https://cdn.discordapp.com/avatars/1/abc.exe", false},
		{"", false},
		{"not a url", false},
		// internal/private targets an attacker might try to smuggle through
		{"https://cdn.discordapp.com/avatars/1/../../169.254.169.254/abc.png", false},
	}
	for _, tc := range cases {
		if got := discordAvatarURLPattern.MatchString(tc.url); got != tc.want {
			t.Errorf("discordAvatarURLPattern.MatchString(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestAvatarCacheGetPut(t *testing.T) {
	c := newAvatarCache(1024)
	url := "https://cdn.discordapp.com/avatars/1/abc.png"

	if _, ok := c.get(url); ok {
		t.Fatal("get on empty cache should miss")
	}

	want := cachedAvatar{data: []byte("fake-image-bytes"), contentType: "image/png"}
	c.put(url, want)

	got, ok := c.get(url)
	if !ok {
		t.Fatal("get after put should hit")
	}
	if string(got.data) != string(want.data) || got.contentType != want.contentType {
		t.Errorf("get() = %+v, want %+v", got, want)
	}
}
func TestAvatarCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newAvatarCache(5)
	c.put("a", cachedAvatar{data: []byte("aa")})
	c.put("b", cachedAvatar{data: []byte("bb")})
	if _, ok := c.get("a"); !ok {
		t.Fatal("expected a cache hit")
	}
	c.put("c", cachedAvatar{data: []byte("cc")})

	if _, ok := c.get("b"); ok {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("new entry was evicted")
	}
}
