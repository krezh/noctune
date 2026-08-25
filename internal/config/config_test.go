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
