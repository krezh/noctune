package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// sessionTTL is how long a session cookie stays valid, for both Discord
// OAuth logins and WEB_AUTH_TOKEN logins.
const sessionTTL = 7 * 24 * time.Hour

// session is one logged-in browser's identity and permissions.
//
// A Discord OAuth login populates DiscordUserID, Username, AvatarURL and
// AllowedGuildID (the intersection, computed once at login, of the
// user's guilds and the guilds the bot is in) and leaves Trusted false.
// A WEB_AUTH_TOKEN login sets Trusted true and leaves the rest zero —
// it carries no Discord identity, so it cannot be voice-gated and is
// granted access to every guild, exactly like the old shared-token
// cookie did.
type session struct {
	DiscordUserID  string              `json:"u,omitempty"`
	Username       string              `json:"n,omitempty"`
	AvatarURL      string              `json:"a,omitempty"`
	AllowedGuildID map[string]struct{} `json:"g,omitempty"`
	Trusted        bool                `json:"t,omitempty"`
	ExpiresAt      time.Time           `json:"e"`
}

func (s *session) canAccessGuild(guildID string) bool {
	if s.Trusted {
		return true
	}
	_, ok := s.AllowedGuildID[guildID]
	return ok
}

// sessionStore turns a session into a signed cookie value and back
// (HMAC-SHA256 over the JSON payload) rather than keeping sessions in a
// server-side table — the cookie itself is the only place this state
// lives, so a restart (redeploy, crash, or just restarting the dev
// process) doesn't sign anyone out, matching the rest of noctune's
// memory-only, no-persistence design instead of fighting it. The
// tradeoff: logout can only ever clear the cookie client-side, not
// revoke it server-side, so a copied cookie stays valid until it
// naturally expires (sessionTTL) even after "signing out".
type sessionStore struct {
	key []byte
}

// newSessionStore builds the signer from cfg.SessionSecret. Left unset,
// it generates a random key for this process's lifetime and logs a
// warning — sessions still won't survive a restart in that case, since
// every new process re-derives (and this time, invents) a different
// key, invalidating every cookie signed with the old one.
func newSessionStore(secret string) *sessionStore {
	key := []byte(secret)
	if secret == "" {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("noctune: generate session signing key: " + err.Error())
		}
		log.Print("noctune: SESSION_SECRET is not set — generated a random key for this run; every restart will sign everyone out. Set SESSION_SECRET to a fixed value to keep logins across restarts.")
	}
	return &sessionStore{key: key}
}

func (s *sessionStore) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// create returns the signed cookie value for sess. There's nothing to
// store server-side — the returned string is the entire session.
func (s *sessionStore) create(sess *session) (string, error) {
	sess.ExpiresAt = time.Now().Add(sessionTTL)
	payload, err := json.Marshal(sess)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + s.sign(payload), nil
}

// get verifies and decodes a cookie value produced by create.
func (s *sessionStore) get(cookieValue string) (*session, bool) {
	encoded, sig, ok := strings.Cut(cookieValue, ".")
	if !ok {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return nil, false
	}
	var sess session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	return &sess, true
}

type contextKey int

const sessionContextKey contextKey = iota

func contextWithSession(ctx context.Context, sess *session) context.Context {
	return context.WithValue(ctx, sessionContextKey, sess)
}

func sessionFromContext(ctx context.Context) *session {
	sess, _ := ctx.Value(sessionContextKey).(*session)
	return sess
}

// isSecureRequest reports whether the request arrived over TLS, either
// directly or (per a reverse proxy's forwarded-proto header) upstream of
// one — it decides the Secure attribute on cookies we set, so plain http
// local/dev use keeps working without a separate config flag.
func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
