package api

import "testing"

func TestSessionRoundTrip(t *testing.T) {
	store := newSessionStore("a-fixed-32-byte-plus-test-secret!!")

	cookie, err := store.create(&session{
		DiscordUserID:  "42",
		Username:       "someone",
		AvatarURL:      "https://example.com/a.png",
		AllowedGuildID: map[string]struct{}{"123": {}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, ok := store.get(cookie)
	if !ok {
		t.Fatalf("get: expected a valid session")
	}
	if got.DiscordUserID != "42" || got.Username != "someone" || got.AvatarURL != "https://example.com/a.png" {
		t.Errorf("session fields didn't survive round-trip: %+v", got)
	}
	if got.Trusted {
		t.Errorf("Trusted should be false for an OAuth session")
	}
	if !got.canAccessGuild("123") {
		t.Errorf("canAccessGuild(123) = false, want true")
	}
	if got.canAccessGuild("456") {
		t.Errorf("canAccessGuild(456) = true, want false")
	}
}

func TestSessionTrusted(t *testing.T) {
	store := newSessionStore("a-fixed-32-byte-plus-test-secret!!")

	cookie, err := store.create(&session{Trusted: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok := store.get(cookie)
	if !ok {
		t.Fatalf("get: expected a valid session")
	}
	if !got.Trusted {
		t.Errorf("Trusted = false, want true")
	}
	if !got.canAccessGuild("anything") {
		t.Errorf("a Trusted session should have access to every guild")
	}
}

func TestSessionTamperedRejected(t *testing.T) {
	store := newSessionStore("a-fixed-32-byte-plus-test-secret!!")

	cookie, err := store.create(&session{DiscordUserID: "42"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := store.get(cookie + "x"); ok {
		t.Errorf("a tampered cookie must not be accepted")
	}

	otherStore := newSessionStore("a-completely-different-32-byte-key")
	if _, ok := otherStore.get(cookie); ok {
		t.Errorf("a cookie signed with a different key must not be accepted")
	}
}

func TestSessionRevocation(t *testing.T) {
	store := newSessionStore("a-fixed-32-byte-plus-test-secret!!")
	cookie, err := store.create(&session{DiscordUserID: "42"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	store.revoke(cookie)
	if _, ok := store.get(cookie); ok {
		t.Fatal("revoked session was accepted")
	}
}

func TestSessionSurvivesStoreRestart(t *testing.T) {
	secret := "a-fixed-32-byte-plus-test-secret!!"
	store := newSessionStore(secret)
	cookie, err := store.create(&session{DiscordUserID: "42"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	restarted := newSessionStore(secret)
	if _, ok := restarted.get(cookie); !ok {
		t.Fatal("valid signed session was rejected after store restart")
	}
}

func TestSessionRevocationIsProcessLocal(t *testing.T) {
	secret := "a-fixed-32-byte-plus-test-secret!!"
	store := newSessionStore(secret)
	cookie, err := store.create(&session{DiscordUserID: "42"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	store.revoke(cookie)

	restarted := newSessionStore(secret)
	if _, ok := restarted.get(cookie); !ok {
		t.Fatal("process-local revocation survived store restart")
	}
}

func TestSessionRevocationSignal(t *testing.T) {
	store := newSessionStore("a-fixed-32-byte-plus-test-secret!!")
	cookie, err := store.create(&session{DiscordUserID: "42"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sess, ok := store.get(cookie)
	if !ok {
		t.Fatal("get: expected a valid session")
	}
	done, active := store.revocationSignal(sess.ID)
	if !active {
		t.Fatal("revocationSignal rejected active session")
	}

	store.revoke(cookie)
	select {
	case <-done:
	default:
		t.Fatal("revocation did not close the session signal")
	}
}
