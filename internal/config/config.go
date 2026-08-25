// Package config loads noctune's runtime configuration from environment
// variables. There is no config file and no persisted state — every
// setting is provided at process start, which is what keeps the bot
// stateless and container-friendly.
package config

import (
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	DiscordBotToken    string `env:"DISCORD_BOT_TOKEN,required"`
	DiscordTestGuildID string `env:"DISCORD_TEST_GUILD_ID"`

	SpotifyClientID     string `env:"SPOTIFY_CLIENT_ID,required"`
	SpotifyClientSecret string `env:"SPOTIFY_CLIENT_SECRET,required"`

	WebListenAddr string `env:"WEB_LISTEN_ADDR" envDefault:":8080"`
	WebAuthToken  string `env:"WEB_AUTH_TOKEN"`
	// WebPublicURL is the externally reachable base URL for the web GUI
	// (e.g. behind a reverse proxy), used to link back to it from Discord
	// chat replies. Left unset, those replies just omit the link, since
	// WebListenAddr is a bind address and often not what's reachable from
	// outside the container.
	WebPublicURL string `env:"WEB_PUBLIC_URL"`

	// Discord OAuth2 login for the web GUI (authorization code flow,
	// scope "identify guilds"). Leave both unset to fall back to
	// WEB_AUTH_TOKEN (or no auth at all if that's unset too) — see
	// DiscordOAuthRedirectURL for how the callback URL is derived.
	DiscordClientID     string `env:"DISCORD_CLIENT_ID"`
	DiscordClientSecret string `env:"DISCORD_CLIENT_SECRET"`
	// DiscordOAuthRedirectURL is the callback URL registered in the
	// Discord Developer Portal's OAuth2 settings for this application.
	// Defaults to WebPublicURL + "/auth/discord/callback" when
	// WebPublicURL is set; required explicitly otherwise.
	DiscordOAuthRedirectURL string `env:"DISCORD_OAUTH_REDIRECT_URL"`
	// SessionSecret signs web GUI session cookies (HMAC-SHA256). Left unset,
	// noctune generates a random value at startup. Sessions are memory-backed
	// and therefore expire on restart either way.
	SessionSecret string `env:"SESSION_SECRET"`

	DefaultVolume         int    `env:"DEFAULT_VOLUME" envDefault:"100"`
	MaxQueueSize          int    `env:"MAX_QUEUE_SIZE" envDefault:"500"`
	IdleDisconnectSeconds int    `env:"IDLE_DISCONNECT_SECONDS" envDefault:"300"`
	LogLevel              string `env:"LOG_LEVEL" envDefault:"info"`
	FFmpegPath            string `env:"FFMPEG_PATH" envDefault:"ffmpeg"`
	YtDlpPath             string `env:"YTDLP_PATH" envDefault:"yt-dlp"`
	// CacheDir stores downloaded track audio on disk so replaying the same
	// track (loop mode, requeuing it, a volume change mid-track restarting
	// the encoder) skips re-fetching it from YouTube. Empty disables
	// caching; tracks are always streamed fresh.
	CacheDir string `env:"CACHE_DIR" envDefault:"/tmp/noctune-cache"`
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.DefaultVolume < 0 || cfg.DefaultVolume > 100 {
		return nil, fmt.Errorf("DEFAULT_VOLUME must be between 0 and 100")
	}
	if (cfg.DiscordClientID == "") != (cfg.DiscordClientSecret == "") {
		return nil, fmt.Errorf("DISCORD_CLIENT_ID and DISCORD_CLIENT_SECRET must be set together")
	}
	if cfg.DiscordOAuthEnabled() {
		if cfg.DiscordOAuthRedirectURL == "" {
			if cfg.WebPublicURL == "" {
				return nil, fmt.Errorf("DISCORD_OAUTH_REDIRECT_URL is required when DISCORD_CLIENT_ID/DISCORD_CLIENT_SECRET are set and WEB_PUBLIC_URL isn't")
			}
			cfg.DiscordOAuthRedirectURL = strings.TrimRight(cfg.WebPublicURL, "/") + "/auth/discord/callback"
		}
	}
	if cfg.SessionSecret != "" && len(cfg.SessionSecret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 characters (try `openssl rand -hex 32`)")
	}
	return cfg, nil
}

// DiscordOAuthEnabled reports whether Discord OAuth2 login is configured.
func (c *Config) DiscordOAuthEnabled() bool {
	return c.DiscordClientID != "" && c.DiscordClientSecret != ""
}
