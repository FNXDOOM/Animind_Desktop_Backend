package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"animind-backend-go/internal/repository"
)

// These rules decide what a viewer sees in their list; they are pure, so testing them needs
// no database, session or request.

func TestNormalizeWatchlistStatus(t *testing.T) {
	cases := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "", want: "Plan to Watch"},
		{input: "   ", want: "Plan to Watch"},
		{input: "Watching", want: "Watching"},
		{input: "Completed", want: "Completed"},
		{input: "Plan to Watch", want: "Plan to Watch"},
		{input: "Dropped", want: "Dropped"},
		{input: " watching ", wantErr: true}, // case matters: clients compare exactly
		{input: "On Hold", wantErr: true},
		{input: "DROPPED", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeWatchlistStatus(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeWatchlistStatus(%q) accepted an unknown status", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeWatchlistStatus(%q) = %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeWatchlistStatus(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeProgress(t *testing.T) {
	// A client seeking backwards can report a negative position; storing it would make the
	// next resume invalid, so it clamps rather than rejects.
	id, index, seconds, err := normalizeProgress(" 153564 ", 3, -12)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "153564" || index != 3 || seconds != 0 {
		t.Errorf("got (%q, %d, %v)", id, index, seconds)
	}

	if _, _, _, err := normalizeProgress("  ", 0, 10); err == nil {
		t.Error("an empty anime_id must be rejected")
	}
	for _, index := range []int{-1, 100001} {
		if _, _, _, err := normalizeProgress("1", index, 10); err == nil {
			t.Errorf("episode_index %d must be rejected", index)
		}
	}
	// The bounds are inclusive: a long-running series indexes past 1000.
	if _, _, _, err := normalizeProgress("1", 100000, 10); err != nil {
		t.Errorf("episode_index 100000 should be accepted: %v", err)
	}
}

func TestClampPercent(t *testing.T) {
	cases := map[int]int{-5: 0, 0: 0, 42: 42, 100: 100, 101: 100, 9000: 100}
	for input, want := range cases {
		if got := clampPercent(input); got != want {
			t.Errorf("clampPercent(%d) = %d, want %d", input, got, want)
		}
	}
}

func TestDedupeShowsKeepsTheRicherRow(t *testing.T) {
	linked := repository.Show{
		ID: "a", Title: "Frieren", AnilistID: intP(51140),
		CoverImageURL: strP("https://cdn/large.jpg"), Synopsis: strP("An elf reviews her life."),
	}
	bare := repository.Show{ID: "b", Title: "Frieren"}

	deduped := dedupeShows([]repository.Show{bare, linked})
	if len(deduped) != 1 {
		t.Fatalf("expected one show, got %d: %+v", len(deduped), deduped)
	}
	// The linked row wins regardless of arrival order, and the survivor keeps its group's
	// position so a title-ordered page stays ordered.
	if deduped[0].ID != "a" {
		t.Errorf("kept %q, want the row with an AniList link and artwork", deduped[0].ID)
	}

	// Two rows with different AniList ids under one title are a data anomaly: only one link
	// can be right, and the title pass keeps the richer row.
	other := repository.Show{ID: "c", Title: "Frieren", AnilistID: intP(99999)}
	if got := dedupeShows([]repository.Show{linked, other}); len(got) != 1 {
		t.Errorf("same title with two anilist ids produced %d cards, want 1", len(got))
	}

	// Distinct titles sharing one AniList id (the same entry imported twice) collapse on it.
	sameLinkedOtherSpelling := repository.Show{ID: "f", Title: "Frieren: Mahoutsukai no Tabiji", AnilistID: intP(51140)}
	if got := dedupeShows([]repository.Show{linked, sameLinkedOtherSpelling}); len(got) != 1 {
		t.Errorf("one anilist id produced %d cards, want 1", len(got))
	}

	// Two genuinely different shows must never collapse, linked or not.
	distinct := repository.Show{ID: "g", Title: "Mob Psycho 100"}
	if got := dedupeShows([]repository.Show{linked, distinct}); len(got) != 2 {
		t.Errorf("distinct shows collapsed: %+v", got)
	}

	// No link: normalized title is the identity, so punctuation and case differences collapse.
	reZero := repository.Show{ID: "d", Title: "Re:Zero"}
	reZeroAlt := repository.Show{ID: "e", Title: "RE ZERO"}
	if got := dedupeShows([]repository.Show{reZero, reZeroAlt}); len(got) != 1 {
		t.Errorf("title-only identity did not collapse: %+v", got)
	}
}

func TestEpisodeNumberAndSeasonNormalisation(t *testing.T) {
	// 21.0 arrives as a float from a numeric column and must render as 21.
	if got := normalizeEpisodeNumber(21.0); got != 21 {
		t.Errorf("normalizeEpisodeNumber(21.0) = %v", got)
	}
	// A special numbered 5.5 must keep its fraction rather than merging into episode 5.
	if got := normalizeEpisodeNumber(5.5); got != 5.5 {
		t.Errorf("normalizeEpisodeNumber(5.5) = %v, want 5.5", got)
	}
	if got := normalizeSeason(-3); got != 1 {
		t.Errorf("normalizeSeason(-3) = %d, want the default season", got)
	}
	// Season 0 is meaningful: it holds movies and OVAs.
	if got := normalizeSeason(0); got != 0 {
		t.Errorf("normalizeSeason(0) = %d, want 0", got)
	}
}

func TestAcceptsJSONNegotiation(t *testing.T) {
	cases := map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"*/*":                             false,
		"video/mp4":                       false,
		"application/json, video/*":       false, // a media element asks for both; do not JSON it
		"":                                false,
	}
	for accept, want := range cases {
		r := newRequestWithAccept(accept)
		if got := acceptsJSON(r); got != want {
			t.Errorf("Accept %q -> %v, want %v", accept, got, want)
		}
	}
}

func intP(v int) *int       { return &v }
func strP(v string) *string { return &v }

// newRequestWithAccept builds a request whose Accept header is the value under test.
func newRequestWithAccept(value string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/episodes/1/stream", nil)
	if value != "" {
		request.Header.Set("Accept", value)
	}
	return request
}
