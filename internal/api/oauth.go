package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

const oauthStateCookie = "noctune_oauth_state"

var discordEndpoint = oauth2.Endpoint{
	AuthURL:  "https://discord.com/oauth2/authorize",
	TokenURL: "https://discord.com/api/oauth2/token",
}

func newOAuthConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     discordEndpoint,
		Scopes:       []string{"identify", "guilds"},
	}
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Avatar   string `json:"avatar"`
}

func (u discordUser) avatarURL() string {
	if u.Avatar == "" {
		return ""
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png", u.ID, u.Avatar)
}

type discordGuild struct {
	ID string `json:"id"`
}

// handleDiscordLogin starts the Discord OAuth2 authorization code flow: a
// random state value is stashed in a short-lived cookie (checked back
// against the callback's state param, our only CSRF defense here) and
// the browser is sent to Discord's consent screen.
func (srv *Server) handleDiscordLogin(w http.ResponseWriter, r *http.Request) {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   10 * 60,
	})
	http.Redirect(w, r, srv.oauthCfg.AuthCodeURL(state), http.StatusSeeOther)
}

// handleDiscordCallback exchanges the authorization code for a token,
// fetches the user's identity and guild list with it, computes which of
// those guilds the bot is also in (this becomes the session's read
// access list — see session.canAccessGuild), and starts a session.
func (srv *Server) handleDiscordCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || r.FormValue("state") == "" || stateCookie.Value != r.FormValue("state") {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	token, err := srv.oauthCfg.Exchange(ctx, r.FormValue("code"))
	if err != nil {
		log.Printf("noctune: discord oauth exchange: %v", err)
		http.Error(w, "Discord login failed.", http.StatusBadGateway)
		return
	}

	httpClient := srv.oauthCfg.Client(ctx, token)
	user, err := fetchDiscordJSON[discordUser](httpClient, "https://discord.com/api/users/@me")
	if err != nil {
		log.Printf("noctune: fetch discord user: %v", err)
		http.Error(w, "Discord login failed.", http.StatusBadGateway)
		return
	}
	guilds, err := fetchDiscordJSON[[]discordGuild](httpClient, "https://discord.com/api/users/@me/guilds")
	if err != nil {
		log.Printf("noctune: fetch discord user guilds: %v", err)
		http.Error(w, "Discord login failed.", http.StatusBadGateway)
		return
	}

	allowed := make(map[string]struct{})
	for _, g := range guilds {
		if _, ok := srv.client.Caches.Guild(mustSnowflake(g.ID)); ok {
			allowed[g.ID] = struct{}{}
		}
	}

	sessID, err := srv.sessions.create(&session{
		DiscordUserID:  user.ID,
		Username:       user.Username,
		AvatarURL:      user.avatarURL(),
		AllowedGuildID: allowed,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessID,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	// Skip the server picker and land straight on the guild the user is
	// currently sitting in voice for, if any — a Discord user can only be
	// connected to one voice channel at a time, so there's never more
	// than one candidate here.
	redirectTo := "/"
	for guildID := range allowed {
		if vs, ok := srv.client.Caches.VoiceState(mustSnowflake(guildID), mustSnowflake(user.ID)); ok && vs.ChannelID != nil {
			redirectTo = "/g/" + guildID
			break
		}
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}

func fetchDiscordJSON[T any](client *http.Client, url string) (T, error) {
	var zero T
	resp, err := client.Get(url)
	if err != nil {
		return zero, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("noctune: close discord api response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return zero, fmt.Errorf("%s: %d: %s", url, resp.StatusCode, body)
	}
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return zero, err
	}
	return v, nil
}
