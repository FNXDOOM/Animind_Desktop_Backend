package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/database"
)

// discardLogger keeps migration and repository output out of the test log.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Integration tests run against a real Postgres because the SQL itself is what matters:
// constraint names, cascades and ownership filters. Set POSTGRES_TEST_DATABASE_URL to run them.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POSTGRES_TEST_DATABASE_URL to a disposable database to run repository tests")
	}

	t.Setenv("DATABASE_URL", url)
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
	// Keep the pool small: packages test concurrently and each opens its own pool, so a
	// production-sized pool can exhaust max_connections and stall unrelated suites.
	t.Setenv("DB_MAX_CONNS", "2")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	logger := discardLogger()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	db, err := database.Open(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	t.Cleanup(db.Close)

	store := New(db)
	// Each run starts clean: emails are unique per test, but sessions and library rows accumulate.
	if _, err := db.Pool.Exec(ctx, `
		truncate table watch_parties, watch_party_participants, user_preferences,
		         watch_history, progress, watchlist, desktop_auth_sessions,
		         animind_web_sessions, animind_auth_tokens, episodes, shows,
		         animind_user_identities, animind_users restart identity cascade`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store
}

func TestUserLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	hash := "argon2id$v=19$m=2048,t=1,p=1$c2FsdGRhdGF0aGF0aXMxMA$ZHVtbXloYXNoZm9ydGVzdGluZ29ubHk"
	user, err := store.CreateUser(ctx, "Nik@Example.com", "nik@example.com", "nik", strPtr("https://avatar"), &hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if !IsUUID(user.ID) {
		t.Fatalf("generated id is not a uuid: %q", user.ID)
	}
	if user.IsAdmin {
		t.Error("new accounts must not be administrators")
	}

	found, err := store.UserByEmailNormalized(ctx, "nik@example.com")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found.ID != user.ID {
		t.Errorf("lookup returned %q, want %q", found.ID, user.ID)
	}
	if found.PasswordHash == nil {
		t.Error("the stored hash must come back for verification")
	}

	if _, err := store.CreateUser(ctx, "other@example.com", "nik@example.com", "dup", nil, &hash); err == nil {
		t.Fatal("email_normalized must be unique, otherwise two accounts share one identity")
	} else if !IsUniqueViolation(err) {
		t.Errorf("expected a unique violation, got %v", err)
	}

	if err := store.SetAdmin(ctx, user.ID, true); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	updated, _ := store.UserByID(ctx, user.ID)
	if !updated.IsAdmin {
		t.Error("admin flag was not stored")
	}

	// A lockout must engage at the configured threshold and clear on success.
	locked := false
	for i := 0; i < 10; i++ {
		locked, err = store.RegisterFailedLogin(ctx, user.ID, 3, time.Minute)
		if err != nil {
			t.Fatalf("RegisterFailedLogin: %v", err)
		}
	}
	if !locked {
		t.Error("the account should be locked after repeated failures")
	}
	reloaded, _ := store.UserByID(ctx, user.ID)
	if reloaded.LockedUntil == nil {
		t.Error("locked_until must be recorded so login can answer 423")
	}
	if err := store.ClearFailedLogins(ctx, user.ID); err != nil {
		t.Fatalf("ClearFailedLogins: %v", err)
	}
	reloaded, _ = store.UserByID(ctx, user.ID)
	if reloaded.FailedLogins != 0 || reloaded.LockedUntil != nil {
		t.Errorf("lockout did not clear: %+v", reloaded)
	}
}

func TestWebSessionExpiryAndRevocation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	user := mustUser(t, store, "sessions@example.com")
	session, err := store.CreateWebSession(ctx, user.ID, "hash-live", "web", "Go-test", "127.0.0.1", false, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateWebSession: %v", err)
	}
	if _, err := store.CreateWebSession(ctx, user.ID, "hash-expired", "web", "Go-test", "", false, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("second session: %v", err)
	}

	found, err := store.ActiveWebSession(ctx, "hash-live")
	if err != nil {
		t.Fatalf("ActiveWebSession: %v", err)
	}
	if found.ID != session.ID {
		t.Errorf("found %q want %q", found.ID, session.ID)
	}
	if _, err := store.ActiveWebSession(ctx, "hash-expired"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired session must not resolve, got %v", err)
	}

	// A session id alone is not authority: another account must not be able to revoke it.
	intruder := mustUser(t, store, "intruder@example.com")
	if err := store.RevokeWebSession(ctx, intruder.ID, session.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-account revoke must fail, got %v", err)
	}
	if _, err := store.ActiveWebSession(ctx, "hash-live"); err != nil {
		t.Fatalf("the session survived an unrelated account's revoke attempt: %v", err)
	}

	if err := store.RevokeWebSession(ctx, user.ID, session.ID); err != nil {
		t.Fatalf("RevokeWebSession: %v", err)
	}
	if _, err := store.ActiveWebSession(ctx, "hash-live"); !errors.Is(err, ErrNotFound) {
		t.Error("a revoked session must stop resolving")
	}

	live, err := store.ListWebSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("revoked and expired sessions must not be listed, got %d", len(live))
	}
}

func TestDesktopSessionRotationAndCleanup(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user := mustUser(t, store, "desktop@example.com")

	session, err := store.CreateDesktopSession(ctx, user.ID, "session-1", "machine-name", "Animind Qt Desktop",
		"refresh-hash-1", "access-hash-1", time.Now().Add(time.Hour), time.Now().Add(720*time.Hour))
	if err != nil {
		t.Fatalf("CreateDesktopSession: %v", err)
	}
	if session.DeviceName == nil || *session.DeviceName != "Animind Qt Desktop" {
		t.Errorf("device label = %v", session.DeviceName)
	}

	found, err := store.DesktopSessionByAccessHash(ctx, "access-hash-1")
	if err != nil {
		t.Fatalf("by access hash: %v", err)
	}
	if found.UserID != user.ID {
		t.Errorf("user = %q", found.UserID)
	}

	// Rotation invalidates the presented credentials.
	if err := store.RotateDesktopSession(ctx, session.ID, "refresh-hash-2", "access-hash-2",
		time.Now().Add(time.Hour), time.Now().Add(720*time.Hour)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := store.DesktopSessionByAccessHash(ctx, "access-hash-1"); !errors.Is(err, ErrNotFound) {
		t.Error("the replaced access token must stop working")
	}
	if _, err := store.DesktopSessionByRefreshHash(ctx, "refresh-hash-1"); !errors.Is(err, ErrNotFound) {
		t.Error("the replaced refresh token must stop working")
	}
	if _, err := store.DesktopSessionByRefreshHash(ctx, "refresh-hash-2"); err != nil {
		t.Errorf("the new refresh token must work: %v", err)
	}

	// A second desktop session for the same account is allowed, which is what device lists are for.
	if _, err := store.CreateDesktopSession(ctx, user.ID, "session-2", "other-machine", "Laptop",
		"refresh-hash-3", "access-hash-3", time.Now().Add(time.Hour), time.Now().Add(720*time.Hour)); err != nil {
		t.Fatalf("second device: %v", err)
	}
	devices, err := store.ListDesktopSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Errorf("device count = %d, want 2", len(devices))
	}

	revoked, err := store.RevokeDesktopSessionByRefreshHash(ctx, "refresh-hash-2")
	if err != nil || !revoked {
		t.Fatalf("revoke: %v %v", revoked, err)
	}
	if _, err := store.DesktopSessionByAccessHash(ctx, "access-hash-2"); !errors.Is(err, ErrNotFound) {
		t.Error("revoking by refresh token must also kill its access token")
	}

	deleted, err := store.CleanupDesktopSessions(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted == 0 {
		t.Error("a revoked session must be pruned by the cleanup job")
	}
}

func TestEpisodeUpsertIsIdempotentOnTheIndexedKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	show, err := store.CreateShow(ctx, Show{Title: "Frieren: Beyond Journey's End", Genres: []string{"Adventure"}})
	if err != nil {
		t.Fatalf("CreateShow: %v", err)
	}
	number := 1.0

	first, err := store.UpsertEpisode(ctx, Episode{
		ShowID: show.ID, SeasonNumber: 1, ContentType: "tv", EpisodeNumber: &number,
		FilePath: "Frieren/[SubsPlease] Frieren - 01 (1080p).mkv", BucketName: "local",
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}

	// Re-scanning the same file must update the row, not add a duplicate episode.
	relocated := "Frieren/Season 1/Frieren - 01.mkv"
	second, err := store.UpsertEpisode(ctx, Episode{
		ShowID: show.ID, SeasonNumber: 1, ContentType: "tv", EpisodeNumber: &number,
		FilePath: relocated, BucketName: "local", OriginalTitle: strPtr("frieren-01.mkv"),
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("expected the same row, got %q then %q", first.ID, second.ID)
	}
	if second.FilePath != relocated {
		t.Errorf("file_path = %q, want the updated path", second.FilePath)
	}
	if second.OriginalTitle == nil || *second.OriginalTitle != "frieren-01.mkv" {
		t.Errorf("scanner metadata was not stored: %v", second.OriginalTitle)
	}

	// Season and content type are part of the key, so a special numbered 1 is its own row.
	special, err := store.UpsertEpisode(ctx, Episode{
		ShowID: show.ID, SeasonNumber: 0, ContentType: "ova", EpisodeNumber: &number,
		FilePath: "Frieren/OVA.mkv", BucketName: "local",
	})
	if err != nil {
		t.Fatalf("ova upsert: %v", err)
	}
	if special.ID == first.ID {
		t.Error("an OVA numbered 1 must not overwrite the TV episode")
	}

	episodes, err := store.EpisodesForShow(ctx, show.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Errorf("episode count = %d, want 2", len(episodes))
	}
}

func TestShowTitleIsUniqueCaseInsensitively(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.CreateShow(ctx, Show{Title: "Cowboy Bebop", Genres: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateShow(ctx, Show{Title: "COWBOY BEBOP", Genres: []string{}}); err == nil {
		t.Fatal("shows.title must be unique regardless of case, or the scanner duplicates libraries")
	}

	// A re-scan of the same title must find the existing row.
	found, err := store.ShowByTitle(ctx, "cowboy bebop")
	if err != nil {
		t.Fatalf("ShowByTitle: %v", err)
	}
	if found.Title != "Cowboy Bebop" {
		t.Errorf("title = %q, want the stored spelling", found.Title)
	}
}

func TestFillShowMetadataOnlyWritesEmptyFields(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	created, err := store.CreateShow(ctx, Show{Title: "Mob Psycho 100", Synopsis: strPtr("kept"), Genres: []string{"Comedy"}})
	if err != nil {
		t.Fatal(err)
	}
	patch := Show{
		Title: "Mob Psycho 100", Synopsis: strPtr("overwritten"), CoverImageURL: strPtr("https://cdn/new.jpg"),
		Genres: []string{"Action"}, Rating: floatPtr(8.5), EpisodeCount: intPtr(12), Studio: strPtr("Bones"),
		Status: strPtr("FINISHED"), Year: strPtr("2016"), AnilistID: intPtr(24),
	}
	if err := store.FillShowMetadata(ctx, created.ID, patch); err != nil {
		t.Fatalf("FillShowMetadata: %v", err)
	}

	loaded, err := store.ShowByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *loaded.Synopsis != "kept" {
		t.Errorf("synopsis = %q; existing metadata must never be overwritten", *loaded.Synopsis)
	}
	if len(loaded.Genres) != 1 || loaded.Genres[0] != "Comedy" {
		t.Errorf("genres = %v; a populated array must be left alone", loaded.Genres)
	}
	if loaded.CoverImageURL == nil || *loaded.CoverImageURL != "https://cdn/new.jpg" {
		t.Errorf("cover = %v; an empty field must be filled", loaded.CoverImageURL)
	}
	if loaded.Rating == nil || *loaded.Rating != 8.5 {
		t.Errorf("rating = %v", loaded.Rating)
	}
	if loaded.AnilistID == nil || *loaded.AnilistID != 24 {
		t.Errorf("anilist id = %v", loaded.AnilistID)
	}
}

func TestUserDataIsScopedToItsOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	owner := mustUser(t, store, "owner@example.com")
	other := mustUser(t, store, "other@example.com")
	payload := json.RawMessage(`{"id":"153564","title":"Death Note"}`)

	if _, err := store.UpsertWatchlist(ctx, owner.ID, "153564", payload, "Watching"); err != nil {
		t.Fatalf("owner upsert: %v", err)
	}
	// A different account writing the same anime id creates its own row.
	if _, err := store.UpsertWatchlist(ctx, other.ID, "153564", payload, "Completed"); err != nil {
		t.Fatalf("other upsert: %v", err)
	}

	ownerItems, err := store.ListWatchlist(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerItems) != 1 {
		t.Fatalf("owner watchlist length = %d, want 1", len(ownerItems))
	}
	if ownerItems[0].Status != "Watching" {
		t.Errorf("status = %q", ownerItems[0].Status)
	}
	if !json.Valid(ownerItems[0].AnimeData) {
		t.Error("anime_data must come back as JSON")
	}

	// An update keyed only by anime id must not touch another user's row.
	if err := store.UpdateWatchlistStatus(ctx, owner.ID, "153564", "Dropped"); err != nil {
		t.Fatal(err)
	}
	otherItems, _ := store.ListWatchlist(ctx, other.ID)
	if otherItems[0].Status != "Completed" {
		t.Error("a status change leaked across accounts")
	}

	if err := store.SaveProgress(ctx, owner.ID, "153564", 3, 412.5); err != nil {
		t.Fatal(err)
	}
	if seconds, err := store.ProgressSeconds(ctx, owner.ID, "153564", 3); err != nil || seconds != 412.5 {
		t.Errorf("progress = %v/%v", seconds, err)
	}
	if seconds, err := store.ProgressSeconds(ctx, other.ID, "153564", 3); err != nil || seconds != 0 {
		t.Errorf("another account must see no progress, got %v/%v", seconds, err)
	}
	// Saving twice updates rather than duplicates.
	if err := store.SaveProgress(ctx, owner.ID, "153564", 3, 500); err != nil {
		t.Fatal(err)
	}
	if seconds, _ := store.ProgressSeconds(ctx, owner.ID, "153564", 3); seconds != 500 {
		t.Errorf("progress after re-save = %v, want 500", seconds)
	}
	recent, err := store.RecentProgress(ctx, owner.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].EpisodeIndex != 3 {
		t.Errorf("recent = %+v", recent)
	}

	if err := store.DeleteWatchlistItem(ctx, owner.ID, "153564"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWatchlistItem(ctx, owner.ID, "153564"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice should report not found, got %v", err)
	}
}

func TestDeleteAccountCascadesEverything(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	user := mustUser(t, store, "closing@example.com")
	show, err := store.CreateShow(ctx, Show{Title: "Violet Evergarden", Genres: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	number := 1.0
	episode, err := store.UpsertEpisode(ctx, Episode{
		ShowID: show.ID, SeasonNumber: 1, ContentType: "tv", EpisodeNumber: &number,
		FilePath: "Violet/01.mkv", BucketName: "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.UpsertWatchlist(ctx, user.ID, show.ID, json.RawMessage(`{"id":"1"}`), "Watching"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProgress(ctx, user.ID, show.ID, 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertHistory(ctx, HistoryItem{UserID: user.ID, ShowTitle: "Violet Evergarden", ProgressPct: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePreferences(ctx, user.ID, json.RawMessage(`{"volume":0.5}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWebSession(ctx, user.ID, "hash-x", "web", "ua", "", false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDesktopSession(ctx, user.ID, "s-1", "d", "d", "r-1", "a-1",
		time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateParty(ctx, "VIOLE1", user.ID, "Violet", &episode.ID); err != nil {
		t.Fatalf("CreateParty: %v", err)
	}
	if err := store.RecordPartyParticipant(ctx, "VIOLE1", user.ID); err != nil {
		t.Fatalf("RecordPartyParticipant: %v", err)
	}

	if err := store.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	for table, query := range map[string]string{
		"watchlist":             `select count(*) from watchlist where user_id = $1::uuid`,
		"progress":              `select count(*) from progress where user_id = $1::uuid`,
		"watch_history":         `select count(*) from watch_history where user_id = $1::uuid`,
		"user_preferences":      `select count(*) from user_preferences where user_id = $1::uuid`,
		"animind_web_sessions":  `select count(*) from animind_web_sessions where user_id = $1::uuid`,
		"desktop_auth_sessions": `select count(*) from desktop_auth_sessions where user_id = $1::uuid`,
		"watch_parties":         `select count(*) from watch_parties where host_user_id = $1::uuid`,
	} {
		var count int64
		if err := store.pool.QueryRow(ctx, query, user.ID).Scan(&count); err != nil {
			t.Errorf("%s: %v", table, err)
			continue
		}
		if count != 0 {
			t.Errorf("%s still holds %d rows after the account was deleted", table, count)
		}
	}

	if _, err := store.UserByID(ctx, user.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the account row must be gone, got %v", err)
	}
}

func TestWatchPartyRecordsAndTTLDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user := mustUser(t, store, "party@example.com")
	other := mustUser(t, store, "member@example.com")

	number := 7.0
	show, err := store.CreateShow(ctx, Show{Title: "Barakamon", Genres: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	episode, err := store.UpsertEpisode(ctx, Episode{
		ShowID: show.ID, SeasonNumber: 1, ContentType: "tv", EpisodeNumber: &number,
		FilePath: "Barakamon/07.mkv", BucketName: "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.CreateParty(ctx, "ABCDE", user.ID, "Host", &episode.ID); err != nil {
		t.Fatalf("CreateParty: %v", err)
	}
	if err := store.RecordPartyParticipant(ctx, "ABCDE", other.ID); err != nil {
		t.Fatalf("participant: %v", err)
	}
	// A duplicate room code is a primary-key violation, which is how a collision surfaces.
	if err := store.CreateParty(ctx, "ABCDE", other.ID, "Other", nil); err == nil {
		t.Error("a colliding room code must not overwrite the live room")
	}

	loaded, err := store.PartyByCode(ctx, "ABCDE")
	if err != nil {
		t.Fatalf("PartyByCode: %v", err)
	}
	if loaded.Status != "active" {
		t.Errorf("status = %q", loaded.Status)
	}
	if loaded.EpisodeID == nil || *loaded.EpisodeID != episode.ID {
		t.Errorf("episode = %v", loaded.EpisodeID)
	}
	if !store.EpisodeExists(ctx, episode.ID) {
		t.Error("EpisodeExists must confirm a real row")
	}
	if store.EpisodeExists(ctx, "not-a-uuid") {
		t.Error("a malformed id must be false rather than a database error")
	}

	// A restart marks rooms as ended instead of leaving them active forever.
	ended, err := store.EndAllActiveParties(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ended == 0 {
		t.Error("the active room should have been closed")
	}
	reloaded, _ := store.PartyByCode(ctx, "ABCDE")
	if reloaded.EndedAt == nil {
		t.Error("ended_at must be recorded so the TTL sweep can delete the row")
	}

	rooms, participants, err := store.DeleteEndedParties(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("DeleteEndedParties: %v", err)
	}
	if rooms != 1 || participants != 1 {
		t.Errorf("deleted rooms=%d participants=%d, want 1 and 1", rooms, participants)
	}
	if _, err := store.PartyByCode(ctx, "ABCDE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the room row must be gone, got %v", err)
	}

	// A room that ended recently is retained, which is what the TTL is for.
	if err := store.CreateParty(ctx, "FGHIJ", user.ID, "Host", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.EndParty(ctx, "FGHIJ"); err != nil {
		t.Fatal(err)
	}
	rooms, _, err = store.DeleteEndedParties(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rooms != 0 {
		t.Error("a recently ended room must survive the sweep")
	}
}

func mustUser(t *testing.T, store *Store, email string) *User {
	t.Helper()
	hash := "argon2id$v=19$m=2048,t=1,p=1$c2FsdGRhdGF0aGF0aXMxMA$ZHVtbXloYXNoZm9ydGVzdGluZ29ubHk"
	user, err := store.CreateUser(context.Background(), email, email, strings.SplitN(email, "@", 2)[0], nil, &hash)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return user
}

func strPtr(value string) *string     { return &value }
func intPtr(value int) *int           { return &value }
func floatPtr(value float64) *float64 { return &value }

func TestMigrationsAreRecordedAndIdempotent(t *testing.T) {
	url := os.Getenv("POSTGRES_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POSTGRES_TEST_DATABASE_URL to run migration tests")
	}
	store := newTestStore(t)

	var applied []string
	rows, err := store.pool.Query(context.Background(), `select name from schema_migrations order by name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, name)
	}
	if len(applied) < 2 {
		t.Fatalf("expected the schema migrations to be recorded, got %v", applied)
	}
	for _, name := range applied {
		if !strings.HasSuffix(name, ".sql") {
			t.Errorf("unexpected migration record %q", name)
		}
	}
	fmt.Fprintf(os.Stderr, "applied migrations: %v\n", applied)
}
