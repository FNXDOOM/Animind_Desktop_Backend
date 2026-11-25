package scanner

import (
	"testing"

	"animind-backend-go/internal/metadata"
	"animind-backend-go/internal/repository"
)

type (
	Metadata = metadata.Metadata
	Trailer  = metadata.Trailer
)

// The scanner scores loose title matches to decide whether a folder is a known show. That
// ranking is arithmetic, not SQL, so it is tested here; SQL has the repository suite.

func TestScoreShowCandidateRanksLinkedRicherRowsFirst(t *testing.T) {
	linked := repository.Show{
		Title: "Re:Zero", AnilistID: intP(31240),
		CoverImageURL: strP("https://cdn/a.jpg"), Synopsis: strP("A boy is thrown back in time."),
	}
	artworkOnly := repository.Show{Title: "Re Zero", CoverImageURL: strP("https://cdn/a.jpg")}
	bare := repository.Show{Title: "Re Zero"}

	if scoreShowCandidate(linked, "Re:Zero") <= scoreShowCandidate(artworkOnly, "Re:Zero") {
		t.Error("an AniList-linked row must outrank one that only has artwork")
	}
	if scoreShowCandidate(artworkOnly, "Re:Zero") <= scoreShowCandidate(bare, "Re:Zero") {
		t.Error("artwork must beat an empty row")
	}
	// The insert threshold in resolveShow is 9, so an unrelated title must stay below it.
	if score := scoreShowCandidate(bare, "That Other Show"); score >= 9 {
		t.Errorf("an unrelated title scored %d, which would wrongly reuse the row", score)
	}
	if score := scoreShowCandidate(linked, "Re:Zero"); score < 9 {
		t.Errorf("a linked row with artwork and synopsis scored %d, below the adoption threshold", score)
	}
}

func TestScoreShowCandidateMatchesNormalizedTitles(t *testing.T) {
	// Punctuation and case differ between a release folder and the official title; the score must not.
	withPunctuation := repository.Show{Title: "Steins;Gate"}
	if a, b := scoreShowCandidate(withPunctuation, "Steins Gate"), scoreShowCandidate(withPunctuation, "steins;gate"); a != b {
		t.Errorf("normalization changed the score: %d vs %d", a, b)
	}
	// A substring either way counts as a partial match, so a long release name finds a short title.
	long := repository.Show{Title: "Re:Zero - Starting Life in Another World"}
	if score := scoreShowCandidate(long, "Re:Zero"); score < 3 {
		t.Errorf("substring match scored %d, want the partial credit", score)
	}
}

func TestShowIsIncompleteDrivesReEnrichment(t *testing.T) {
	complete := repository.Show{
		Title: "Frieren", CoverImageURL: strP("https://cdn/a.jpg"), Synopsis: strP("text"),
		Genres: []string{"Adventure"}, Rating: floatP(8.9), EpisodeCount: intP(28),
		Studio: strP("Madhouse"), Status: strP("RELEASING"), Year: strP("2023"), AnilistID: intP(51140),
	}
	if showIsIncomplete(&complete) {
		t.Error("a fully populated show must not be re-fetched on every scan")
	}

	// Each field on its own is enough to mark the row incomplete.
	for name, mutate := range map[string]func(*repository.Show){
		"cover":        func(s *repository.Show) { s.CoverImageURL = nil },
		"synopsis":     func(s *repository.Show) { s.Synopsis = strP("  ") },
		"genres":       func(s *repository.Show) { s.Genres = nil },
		"rating":       func(s *repository.Show) { s.Rating = nil },
		"episodes":     func(s *repository.Show) { s.EpisodeCount = nil },
		"studio":       func(s *repository.Show) { s.Studio = nil },
		"status":       func(s *repository.Show) { s.Status = nil },
		"year":         func(s *repository.Show) { s.Year = nil },
		"anilist link": func(s *repository.Show) { s.AnilistID = nil },
	} {
		show := complete
		mutate(&show)
		if !showIsIncomplete(&show) {
			t.Errorf("a show missing %s should be re-enriched", name)
		}
	}
}

func TestShowFromMetadataFallsBackToTheFolderTitle(t *testing.T) {
	// No provider answer must still produce a usable row, or an offline library would index nothing.
	fallback := showFromMetadata(nil, "Mob Psycho 100")
	if fallback.Title != "Mob Psycho 100" {
		t.Errorf("title = %q", fallback.Title)
	}
	if fallback.Genres == nil {
		t.Error("genres must default to an empty array, not NULL, so the card renders")
	}

	meta := &Metadata{}
	meta.Title = "Frieren: Beyond Journey's End"
	meta.Synopsis = strP("text")
	meta.AnilistID = intP(51140)
	meta.Status = strP("RELEASING")
	meta.Trailer = &Trailer{ID: "abc", Site: "youtube"}
	fromMeta := showFromMetadata(meta, "frieren")
	if fromMeta.Title != "Frieren: Beyond Journey's End" {
		t.Errorf("title = %q, want the canonical provider title", fromMeta.Title)
	}
	if fromMeta.AnilistID == nil || *fromMeta.AnilistID != 51140 {
		t.Errorf("anilist id = %v", fromMeta.AnilistID)
	}
	if fromMeta.TrailerID == nil || fromMeta.TrailerSite == nil || *fromMeta.TrailerSite != "youtube" {
		t.Errorf("trailer = %v / %v", fromMeta.TrailerID, fromMeta.TrailerSite)
	}
	if fromMeta.Status == nil {
		t.Error("status must be carried through")
	}
}

func intP(v int) *int           { return &v }
func strP(v string) *string     { return &v }
func floatP(v float64) *float64 { return &v }
