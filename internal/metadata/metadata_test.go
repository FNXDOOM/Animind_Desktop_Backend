package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"animind-backend-go/internal/cache"
)

func TestNormalizeTitleKey(t *testing.T) {
	cases := map[string]string{
		"Frieren":        "frieren",
		"  Re:Zero ":     "re zero",
		"Steins;Gate":    "steins gate",
		"Mob Psycho 100": "mob psycho 100",
		"SPY x FAMILY":   "spy x family",
		"---":            "",
		"A  B   C":       "a b c",
	}
	for input, want := range cases {
		if got := NormalizeTitleKey(input); got != want {
			t.Errorf("NormalizeTitleKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStripHTML(t *testing.T) {
	input := "<p>Hello <i>world</i></p>\nLines &amp; entities &#039;quoted&#039;"
	got := StripHTML(input)
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("tags must not survive: %q", got)
	}
	if !strings.Contains(got, "Hello world") {
		t.Errorf("text must survive: %q", got)
	}
	if !strings.Contains(got, "&") || !strings.Contains(got, "'") {
		t.Errorf("entities must be decoded, got %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("whitespace must collapse to single spaces: %q", got)
	}
	if StripHTML("") != "" {
		t.Error("empty input must stay empty")
	}
}

const anilistFixture = `{
  "id": 51140,
  "title": { "romaji": "Sousou no Frieren", "english": "Frieren: Beyond Journey's End", "native": "葬送のフリーレン" },
  "synonyms": ["Frieren", "Mahoutsukai no Frieren", "Sousou no Frieren", "Frieren"],
  "description": "<p>An elf reviews her life.</p>",
  "coverImage": { "large": "https://cdn.example/large.jpg", "medium": "https://cdn.example/medium.jpg" },
  "episodes": 28,
  "genres": ["Adventure", "Fantasy"],
  "averageScore": 89,
  "status": "RELEASING",
  "seasonYear": 2023,
  "studios": { "nodes": [{ "name": "Madhouse" }] },
  "trailer": { "id": "abc123", "site": "youtube", "thumbnail": null }
}`

func TestFromAniListMapsToLibraryShape(t *testing.T) {
	var media Media
	if err := json.Unmarshal([]byte(anilistFixture), &media); err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}

	result := fromAniList(&media)

	if result.Title != "Frieren: Beyond Journey's End" {
		t.Errorf("Title = %q, want the English title both clients display", result.Title)
	}
	if result.CoverImageURL == nil || *result.CoverImageURL != "https://cdn.example/large.jpg" {
		t.Errorf("cover image = %v, want the large variant", result.CoverImageURL)
	}
	// AniList scores are 0-100 while the library and both clients show 0-10.
	if result.Rating == nil || *result.Rating != 8.9 {
		t.Errorf("Rating = %v, want 8.9", result.Rating)
	}
	if result.EpisodeCount == nil || *result.EpisodeCount != 28 {
		t.Errorf("EpisodeCount = %v", result.EpisodeCount)
	}
	if result.Studio == nil || *result.Studio != "Madhouse" {
		t.Errorf("Studio = %v", result.Studio)
	}
	if result.AnilistID == nil || *result.AnilistID != 51140 {
		t.Errorf("AnilistID = %v, must be recorded for de-duplication", result.AnilistID)
	}
	if result.Synopsis == nil || strings.Contains(*result.Synopsis, "<p>") {
		t.Errorf("Synopsis must be plain text, got %v", result.Synopsis)
	}
	if result.Trailer == nil || result.Trailer.ID != "abc123" || result.Trailer.Site != "youtube" {
		t.Errorf("Trailer = %v", result.Trailer)
	}
	if result.Status == nil || *result.Status != "RELEASING" {
		t.Errorf("Status = %v", result.Status)
	}
	if result.Year == nil || *result.Year != "2023" {
		t.Errorf("Year = %v", result.Year)
	}
	if result.Source != "anilist" {
		t.Errorf("Source = %q", result.Source)
	}
	// Romaji, the native title and every distinct synonym must land in AlternateTitles,
	// deduplicated: the repeated "Frieren" synonym and the romaji that repeats a synonym
	// each collapse to one entry.
	wantAlternates := []string{"Frieren", "Mahoutsukai no Frieren", "Sousou no Frieren", "葬送のフリーレン"}
	if !reflect.DeepEqual(result.AlternateTitles, wantAlternates) {
		t.Errorf("AlternateTitles = %v, want %v", result.AlternateTitles, wantAlternates)
	}
}

const malFixture = `{
  "id": 12345,
  "title": "Frieren: Mahoutsukai no Tabiji",
  "main_picture": { "medium": "https://cdn.example/m.jpg", "large": "https://cdn.example/l.jpg" },
  "alternative_titles": { "en": "Frieren", "ja": "葬送のフリーレン", "synonyms": ["Frieren", "Sousou no Frieren", "Frieren"] },
  "synopsis": "An elf reviews her life.",
  "mean": 9.13,
  "num_episodes": 24,
  "status": "finished_airing",
  "start_season": { "year": 2023, "season": "fall" },
  "genres": [{ "id": 1, "name": "Adventure" }, { "id": 2, "name": "Fantasy" }],
  "studios": [{ "id": 3, "name": "Madhouse" }]
}`

func TestFromMALFallsBackWithoutAniListID(t *testing.T) {
	var anime MALAnime
	if err := json.Unmarshal([]byte(malFixture), &anime); err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}

	result := fromMAL(&anime)

	if result.Title != "Frieren" {
		t.Errorf("Title = %q, want the English alternative title", result.Title)
	}
	if result.Source != "myanimelist" {
		t.Errorf("Source = %q", result.Source)
	}
	if result.AnilistID != nil {
		t.Error("a MyAnimeList match must not claim an AniList id, which would mis-link the show")
	}
	if result.Rating == nil || *result.Rating != 9.13 {
		t.Errorf("Rating = %v, want the MAL mean, already on a 0-10 scale", result.Rating)
	}
	if len(result.Genres) != 2 || result.Genres[1] != "Fantasy" {
		t.Errorf("Genres = %v", result.Genres)
	}
	if result.Year == nil || *result.Year != "2023" {
		t.Errorf("Year = %v", result.Year)
	}
	if result.Trailer != nil {
		t.Error("MyAnimeList metadata carries no trailer, so none must be recorded")
	}
	if result.EpisodeCount == nil || *result.EpisodeCount != 24 {
		t.Errorf("EpisodeCount = %v", result.EpisodeCount)
	}
	// Synonyms plus the native title, deduplicated: the repeated "Frieren" synonym
	// collapses to one entry.
	wantAlternates := []string{"Frieren", "Sousou no Frieren", "葬送のフリーレン"}
	if !reflect.DeepEqual(result.AlternateTitles, wantAlternates) {
		t.Errorf("AlternateTitles = %v, want %v", result.AlternateTitles, wantAlternates)
	}
}

// TestMetadataCacheKeyIsUnchanged pins the cache identity (spec §7.9): alternate titles ride
// inside the cached payload, but the key stays the normalized title, so entries written
// before this change and after it address the same slot.
func TestMetadataCacheKeyIsUnchanged(t *testing.T) {
	key := NormalizeTitleKey("Frieren: Beyond Journey's End")
	if key != "frieren beyond journey s end" {
		t.Fatalf("NormalizeTitleKey = %q, want the documented alphanumeric key", key)
	}
	if got := cache.MetadataKey(key); got != "metadata:frieren_beyond_journey_s_end" {
		t.Errorf("MetadataKey = %q, want metadata:frieren_beyond_journey_s_end", got)
	}
}

func TestIsTransientRetryPolicy(t *testing.T) {
	if isTransient(nil) {
		t.Error("nil is never transient")
	}
	if !isTransient(ErrUpstreamRejected) {
		t.Error("a throttled or server-error answer is worth retrying")
	}
	if isTransient(errors.New("anilist HTTP 404: Not Found")) {
		t.Error("a 4xx answer other than 429 is permanent for this title")
	}
	if !isTransient(context.DeadlineExceeded) {
		t.Error("a timeout must be retried")
	}
}
