package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsPartialDiscordOAuthCredentials(t *testing.T) {
	for _, tc := range []struct {
		name         string
		clientID     string
		clientSecret string
	}{
		{name: "client ID only", clientID: "client-id"},
		{name: "client secret only", clientSecret: "client-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DISCORD_BOT_TOKEN", "bot-token")
			t.Setenv("SPOTIFY_CLIENT_ID", "spotify-id")
			t.Setenv("SPOTIFY_CLIENT_SECRET", "spotify-secret")
			t.Setenv("DISCORD_CLIENT_ID", tc.clientID)
			t.Setenv("DISCORD_CLIENT_SECRET", tc.clientSecret)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("Load() error = %v, want mismatched OAuth credentials error", err)
			}
		})
	}
}
func TestLoadRejectsInvalidLimitsAndLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "queue size", key: "MAX_QUEUE_SIZE", value: "0"},
		{name: "idle disconnect", key: "IDLE_DISCONNECT_SECONDS", value: "-1"},
		{name: "yt-dlp processes", key: "MAX_YTDLP_PROCESSES", value: "0"},
		{name: "audio cache", key: "CACHE_MAX_BYTES", value: "-1"},
		{name: "avatar cache", key: "AVATAR_CACHE_MAX_BYTES", value: "-1"},
		{name: "log level", key: "LOG_LEVEL", value: "verbose"},
		{name: "loudness target too loud", key: "LOUDNESS_TARGET_LUFS", value: "0"},
		{name: "loudness target too quiet", key: "LOUDNESS_TARGET_LUFS", value: "-80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DISCORD_BOT_TOKEN", "bot-token")
			t.Setenv("SPOTIFY_CLIENT_ID", "spotify-id")
			t.Setenv("SPOTIFY_CLIENT_SECRET", "spotify-secret")
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.key, tc.value)
			}
		})
	}
}
