package scanner

import "testing"

// TestParseFolderPath pins the parser against the release layouts that motivated each rule.
// A regression here silently renames shows or mis-numbers episodes across the whole library.
func TestParseFolderPath(t *testing.T) {
	cases := []struct {
		path      string
		wantTitle string
		wantEp    float64
		wantSeas  int
		wantType  ContentType
	}{
		{"Frieren/[SubsPlease] Frieren - 01 (1080p) [ABCDEF12].mkv", "Frieren", 1, 1, ContentTypeTV},
		{"Naruto Shippuden/Season 1/S01E04.mkv", "Naruto Shippuden", 4, 1, ContentTypeTV},
		{"One Piece/One Piece - 1000.mp4", "One Piece", 1000, 1, ContentTypeTV},
		{
			"Re.ZERO.Starting.Life.in.Another.World.S03.1080p.BluRay.Opus.2.0.x265-Headpatter/Re:Zero S03E01.mkv",
			"Re ZERO Starting Life in Another World", 1, 3, ContentTypeTV,
		},
		{"Cowboy Bebop/Cowboy Bebop - 03 [1080p].mkv", "Cowboy Bebop", 3, 1, ContentTypeTV},
		{"Steins;Gate/Season 2/S02E12.mkv", "Steins;Gate", 12, 2, ContentTypeTV},
		{"Mob Psycho 100/Mob Psycho 100 - OVA - 01.mkv", "Mob Psycho 100", 1, 0, ContentTypeOVA},
		{"Gintama/Gintama Movie 2.mkv", "Gintama", 2, 0, ContentTypeMovie},
		{"Show/Episode 07.mkv", "Show", 7, 1, ContentTypeTV},
		{"A Story With No Number/Part One.mkv", "", 0, 0, ContentTypeTV},
	}

	for _, tc := range cases {
		parsed := ParseFolderPath(tc.path)
		if tc.wantEp == 0 {
			if parsed != nil {
				t.Errorf("ParseFolderPath(%q) = %+v, want nil (no episode number)", tc.path, parsed)
			}
			continue
		}
		if parsed == nil {
			t.Fatalf("ParseFolderPath(%q) returned nil", tc.path)
		}
		if parsed.ContentType != tc.wantType {
			t.Errorf("ParseFolderPath(%q) content type = %q, want %q", tc.path, parsed.ContentType, tc.wantType)
		}
		if tc.wantTitle != "" && parsed.Title != tc.wantTitle {
			t.Errorf("ParseFolderPath(%q) title = %q, want %q", tc.path, parsed.Title, tc.wantTitle)
		}
		if parsed.Episode != tc.wantEp {
			t.Errorf("ParseFolderPath(%q) episode = %v, want %v", tc.path, parsed.Episode, tc.wantEp)
		}
		if gotSeason := SeasonForContentType(parsed.Season, parsed.ContentType); gotSeason != tc.wantSeas {
			t.Errorf("ParseFolderPath(%q) season = %d, want %d", tc.path, gotSeason, tc.wantSeas)
		}
	}
}

func TestParseAnimeFilename_GroupsAndNoise(t *testing.T) {
	parsed := ParseAnimeFilename("[HorribleSubs] Attack on Titan - 05 [1080p].mkv")
	if parsed == nil {
		t.Fatal("expected a parse result")
	}
	if parsed.Group != "HorribleSubs" {
		t.Errorf("group = %q, want HorribleSubs", parsed.Group)
	}
	if parsed.Title != "Attack on Titan" {
		t.Errorf("title = %q, want Attack on Titan", parsed.Title)
	}
	if parsed.Episode != 5 {
		t.Errorf("episode = %v, want 5", parsed.Episode)
	}
	if parsed.Quality != "1080p" {
		t.Errorf("quality = %q, want 1080p", parsed.Quality)
	}
}

func TestParseAnimeFilename_NoEpisodeIsRejected(t *testing.T) {
	// A plain title has no number to key on; inventing one would place a phantom episode in
	// the library.
	if parsed := ParseAnimeFilename("Frieren the Slayer.mkv"); parsed != nil {
		t.Errorf("expected nil for a filename without an episode number, got %+v", parsed)
	}
}

func TestCleanFolderTitle(t *testing.T) {
	cases := map[string]string{
		"Re.ZERO.Starting.Life.in.Another.World.S03.1080p.BluRay.Opus.2.0.x265-Headpatter": "Re ZERO Starting Life in Another World",
		"Fullmetal.Alchemist.Brotherhood.WEB-DL.DDP5.1.H.264":                              "Fullmetal Alchemist Brotherhood",
		// A release year is not a quality tag, so it stays part of the cleaned title.
		"OnePiece.Movie.2012.1080p": "OnePiece Movie 2012",
	}
	for input, want := range cases {
		if got := CleanFolderTitle(input); got != want {
			t.Errorf("CleanFolderTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestExtractEpisodeByStrongPatterns(t *testing.T) {
	cases := []struct {
		input string
		want  float64
		ok    bool
	}{
		{"Show S02E13", 13, true},
		{"Episode 07", 7, true},
		{"ep 100", 100, true},
		{"Show - 42", 42, true},
		{"no number here", 0, false},
		{"S01E99999", 0, false},
	}
	for _, tc := range cases {
		got, ok := ExtractEpisodeByStrongPatterns(tc.input)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ExtractEpisodeByStrongPatterns(%q) = (%v, %v), want (%v, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSeasonForContentType(t *testing.T) {
	if got := SeasonForContentType(nil, ContentTypeMovie); got != 0 {
		t.Errorf("movie season = %d, want 0", got)
	}
	if got := SeasonForContentType(nil, ContentTypeTV); got != 1 {
		t.Errorf("tv season = %d, want 1", got)
	}
	season := 4
	if got := SeasonForContentType(&season, ContentTypeTV); got != 4 {
		t.Errorf("explicit season = %d, want 4", got)
	}
}
