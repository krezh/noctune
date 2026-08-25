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
	"sync"
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
	ID             string              `json:"i"`
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

// sessionStore signs cookie payloads and tracks process-local revocations.
// Signed sessions survive restarts; revocations intentionally do not.
type sessionStore struct {
	key     []byte
	mu      sync.Mutex
	revoked map[string]time.Time
	signals map[string]chan struct{}
}

// newSessionStore builds the signer from cfg.SessionSecret. Left unset, it
// generates a random key for this process's lifetime.
func newSessionStore(secret string) *sessionStore {
	key := []byte(secret)
	if secret == "" {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("noctune: generate session signing key: " + err.Error())
		}
		log.Print("noctune: SESSION_SECRET is not set — generated a random key for this run")
	}
	return &sessionStore{
		key:     key,
		revoked: make(map[string]time.Time),
		signals: make(map[string]chan struct{}),
	}
}

func (s *sessionStore) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *sessionStore) create(sess *session) (string, error) {
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	sess.ID = base64.RawURLEncoding.EncodeToString(id)
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
	sess, ok := s.decode(cookieValue)
	now := time.Now()
	if !ok || sess.ID == "" || now.After(sess.ExpiresAt) {
		return nil, false
	}
	s.mu.Lock()
	s.cleanupRevocationsLocked(now)
	_, revoked := s.revoked[sess.ID]
	s.mu.Unlock()
	if revoked {
		return nil, false
	}
	return sess, true
}

func (s *sessionStore) decode(cookieValue string) (*session, bool) {
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
	return &sess, true
}

func (s *sessionStore) revoke(cookieValue string) {
	sess, ok := s.decode(cookieValue)
	if !ok {
		return
	}
	s.mu.Lock()
	s.cleanupRevocationsLocked(time.Now())
	s.revoked[sess.ID] = sess.ExpiresAt
	if done, exists := s.signals[sess.ID]; exists {
		delete(s.signals, sess.ID)
		close(done)
	}
	s.mu.Unlock()
}

func (s *sessionStore) revocationSignal(sessionID string) (<-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupRevocationsLocked(time.Now())
	if _, revoked := s.revoked[sessionID]; revoked {
		return nil, false
	}
	if done, exists := s.signals[sessionID]; exists {
		return done, true
	}
	done := make(chan struct{})
	s.signals[sessionID] = done
	return done, true
}

func (s *sessionStore) cleanupRevocationsLocked(now time.Time) {
	for id, expiresAt := range s.revoked {
		if now.After(expiresAt) {
			delete(s.revoked, id)
		}
	}
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
