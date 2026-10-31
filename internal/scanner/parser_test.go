package scanner

import (
	"reflect"
	"strings"
	"testing"
)

// ptr builds an expected pointer field for the release tables.
func ptr[T any](v T) *T { return &v }

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

// TestParseRelease pins ParseRelease against real release-name shapes. Every expectation is
// readable from the name's own tags: an absent or ambiguous tag stays nil/empty and nothing
// is inferred (e.g. "Hi10P" alone names a bit depth, never a video codec).
func TestParseRelease(t *testing.T) {
	// rows=48
	cases := []struct {
		name string
		want ReleaseSpec
	}{
		{"[SubsPlease] Sousou no Frieren - 01 (1080p) [10B0C3B4].mkv", ReleaseSpec{
			Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease")}},
		{"[SubsPlease] Sousou no Frieren - 01v2 (1080p) [10B0C3B4].mkv", ReleaseSpec{
			Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease")}},
		{"[EMBER] Anime Title [WEBRip 1080p HEVC 10bit][Dual Audio][AAC+FLAC][Multi-Subs]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("WEBRip"), VideoCodec: ptr("HEVC"),
			BitDepth: ptr(10), AudioCodecs: []string{"AAC", "FLAC"}, DualAudio: true,
			Subs: ptr("multi"), ReleaseGroup: ptr("EMBER")}},
		{"[Erai-raws] Show - 01 ~ 12 [1080p][Multiple Subtitle][WEB-DL]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("WEB-DL"), Subs: ptr("multi"),
			ReleaseGroup: ptr("Erai-raws"), IsBatch: true}},
		// No codec tag in the name, so VideoCodec stays nil: Hi10P says nothing about AVC vs HEVC.
		{"Show S02E05 1080p BluRay Hi10P FLAC 5.1 - Group", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), BitDepth: ptr(10),
			AudioCodecs: []string{"FLAC"}, ReleaseGroup: ptr("Group")}},
		{"Show S02E05 1080p BluRay Hi10P x264 FLAC - Group", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), VideoCodec: ptr("AVC"),
			BitDepth: ptr(10), AudioCodecs: []string{"FLAC"}, ReleaseGroup: ptr("Group")}},
		{"[SubsPlease] Show - 01-12 (1080p) [Group]", ReleaseSpec{
			Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease"), IsBatch: true}},
		{"[ANO] Show [BDRip h264 8bit AC3]", ReleaseSpec{
			Source: ptr("BDRip"), VideoCodec: ptr("AVC"), BitDepth: ptr(8),
			AudioCodecs: []string{"AC-3"}, ReleaseGroup: ptr("ANO")}},
		{"[VTV] Show [720p][HDTV][AAC]", ReleaseSpec{
			Resolution: ptr(720), Source: ptr("HDTV"), AudioCodecs: []string{"AAC"},
			ReleaseGroup: ptr("VTV")}},
		{"[ANO] Show [1080p][BDRip][FLAC]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BDRip"), AudioCodecs: []string{"FLAC"},
			ReleaseGroup: ptr("ANO")}},
		{"Show - 01 [V0] (Opus).mkv", ReleaseSpec{AudioCodecs: []string{"Opus"}}},
		{"Show S01 [2160p][WEB-DL][HDR10][HEVC]", ReleaseSpec{
			Resolution: ptr(2160), Source: ptr("WEB-DL"), VideoCodec: ptr("HEVC"), HDR: true}},
		{"Show S01 2160p WEB-DL DV HDR10 HEVC-GRP", ReleaseSpec{
			Resolution: ptr(2160), Source: ptr("WEB-DL"), VideoCodec: ptr("HEVC"),
			HDR: true, ReleaseGroup: ptr("GRP")}},
		{"[Group] Show [AV1][10bit][1080p]", ReleaseSpec{
			Resolution: ptr(1080), VideoCodec: ptr("AV1"), BitDepth: ptr(10),
			ReleaseGroup: ptr("Group")}},
		{"Show 1080p BluRay EAC3 5.1 x265-Group", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), VideoCodec: ptr("HEVC"),
			AudioCodecs: []string{"E-AC-3"}, ReleaseGroup: ptr("Group")}},
		{"Show 1080p BluRay DTS-HD MA 5.1 x264-Grp", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), VideoCodec: ptr("AVC"),
			AudioCodecs: []string{"DTS-HD MA"}, ReleaseGroup: ptr("Grp")}},
		{"[Grp] Show [1080p][BluRay][DTS:X][x265]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), VideoCodec: ptr("HEVC"),
			AudioCodecs: []string{"DTS-X"}, ReleaseGroup: ptr("Grp")}},
		{"Show - 05 [720p][HardSub]", ReleaseSpec{
			Resolution: ptr(720), Subs: ptr("hardsub")}},
		{"[Group] Show [1080p][SoftSubs]", ReleaseSpec{
			Resolution: ptr(1080), Subs: ptr("softsub"), ReleaseGroup: ptr("Group")}},
		{"Show S01 WEB-DL AAC No Subs-Grp", ReleaseSpec{
			Source: ptr("WEB-DL"), AudioCodecs: []string{"AAC"}, Subs: ptr("none"),
			ReleaseGroup: ptr("Grp")}},
		// Audio tags say nothing about subtitles: DualAudio is set, Subs stays nil unless a
		// Multi-/Soft/Hard/No-Subs tag is present too.
		{"Show [1080p][Multi-Audio][AAC 2.0][FLAC 5.1]", ReleaseSpec{
			Resolution: ptr(1080), AudioCodecs: []string{"AAC", "FLAC"}, DualAudio: true}},
		{"Show - 01 (BD 1080p) [Dual Audio][AAC 2.0]", ReleaseSpec{
			Resolution: ptr(1080), AudioCodecs: []string{"AAC"}, DualAudio: true}},
		{"[SubsPlease] Show (Batch) (1080p)", ReleaseSpec{
			Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease"), IsBatch: true}},
		{"[Group] Show Complete Batch [1080p][x265]", ReleaseSpec{
			Resolution: ptr(1080), VideoCodec: ptr("HEVC"), ReleaseGroup: ptr("Group"),
			IsBatch: true}},
		{"[Group] Show - 03 [v2][1080p]", ReleaseSpec{
			Resolution: ptr(1080), ReleaseGroup: ptr("Group")}},
		{"[Group] Show - 07 [1080p][D41D8CD9]", ReleaseSpec{
			Resolution: ptr(1080), ReleaseGroup: ptr("Group")}},
		{"Show S01 - 08 (1080p60)", ReleaseSpec{Resolution: ptr(1080)}},
		{"[Group] Show Movie [2160p][BDRip][HEVC][TrueHD.7.1]", ReleaseSpec{
			Resolution: ptr(2160), Source: ptr("BDRip"), VideoCodec: ptr("HEVC"),
			AudioCodecs: []string{"TrueHD"}, ReleaseGroup: ptr("Group")}},
		{"Show 4K/2160p WEB-DL HEVC AAC-Grp", ReleaseSpec{
			Resolution: ptr(2160), Source: ptr("WEB-DL"), VideoCodec: ptr("HEVC"),
			AudioCodecs: []string{"AAC"}, ReleaseGroup: ptr("Grp")}},
		{"Show.S02.1080p.WEB-DL.AAC.H.264-Grp", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("WEB-DL"), VideoCodec: ptr("AVC"),
			AudioCodecs: []string{"AAC"}, ReleaseGroup: ptr("Grp")}},
		{"Show_S01_720p_HDTV_x264_AAC-Grp", ReleaseSpec{
			Resolution: ptr(720), Source: ptr("HDTV"), VideoCodec: ptr("AVC"),
			AudioCodecs: []string{"AAC"}, ReleaseGroup: ptr("Grp")}},
		{"Show 1080p WEB DL x265 AAC 5.1", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("WEB-DL"), VideoCodec: ptr("HEVC"),
			AudioCodecs: []string{"AAC"}}},
		{"Show Movie DVDRip 480p x264 AC3-Grp", ReleaseSpec{
			Resolution: ptr(480), Source: ptr("DVDRip"), VideoCodec: ptr("AVC"),
			AudioCodecs: []string{"AC-3"}, ReleaseGroup: ptr("Grp")}},
		{"Show Movie [DVD][480p][AAC]", ReleaseSpec{
			Resolution: ptr(480), Source: ptr("DVD"), AudioCodecs: []string{"AAC"}}},
		{"Show 1080p BDRip x265 10bit FLAC-Grp", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BDRip"), VideoCodec: ptr("HEVC"),
			BitDepth: ptr(10), AudioCodecs: []string{"FLAC"}, ReleaseGroup: ptr("Grp")}},
		{"Show 720p WEBRip 8-bit AAC 2.0 x264-Grp", ReleaseSpec{
			Resolution: ptr(720), Source: ptr("WEBRip"), VideoCodec: ptr("AVC"),
			BitDepth: ptr(8), AudioCodecs: []string{"AAC"}, ReleaseGroup: ptr("Grp")}},
		{"Show OVA [1080p][x264][Vorbis][Multi-Audio]", ReleaseSpec{
			Resolution: ptr(1080), VideoCodec: ptr("AVC"), AudioCodecs: []string{"Vorbis"},
			DualAudio: true}},
		{"Show - 01 [1080p][ALAC][x265]", ReleaseSpec{
			Resolution: ptr(1080), VideoCodec: ptr("HEVC"), AudioCodecs: []string{"ALAC"}}},
		{"Show [480p][MP3][x264]", ReleaseSpec{
			Resolution: ptr(480), VideoCodec: ptr("AVC"), AudioCodecs: []string{"MP3"}}},
		{"Show [1080p][WEB-DL][VP9][Opus]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("WEB-DL"), VideoCodec: ptr("VP9"),
			AudioCodecs: []string{"Opus"}}},
		{"Show [1080p][BluRay][PCM 5.1][x264]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), VideoCodec: ptr("AVC"),
			AudioCodecs: []string{"PCM"}}},
		{"Show [720p][HDTV][DTS][x264]", ReleaseSpec{
			Resolution: ptr(720), Source: ptr("HDTV"), VideoCodec: ptr("AVC"),
			AudioCodecs: []string{"DTS"}}},
		{"Show [1080p][BluRay][DTS-HD][x265]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), VideoCodec: ptr("HEVC"),
			AudioCodecs: []string{"DTS-HD"}}},
		{"Show S01 2160p WEB-DL HDR HEVC-Grp", ReleaseSpec{
			Resolution: ptr(2160), Source: ptr("WEB-DL"), VideoCodec: ptr("HEVC"),
			HDR: true, ReleaseGroup: ptr("Grp")}},
		{"[Erai-raws] Show - 05 [1080p][Multiple Subtitles][WEB-DL]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("WEB-DL"), Subs: ptr("multi"),
			ReleaseGroup: ptr("Erai-raws")}},
		{"Show - 01 – 12 [1080p][BluRay]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), IsBatch: true}},
		{"Show Episodes 01 to 12 [1080p]", ReleaseSpec{
			Resolution: ptr(1080), IsBatch: true}},
		{"Show Complete Series [1080p][BluRay][FLAC]", ReleaseSpec{
			Resolution: ptr(1080), Source: ptr("BluRay"), AudioCodecs: []string{"FLAC"},
			IsBatch: true}},
	}
	if len(cases) < 40 {
		t.Fatalf("release table has %d rows; keep at least 40 so every tag family stays covered", len(cases))
	}
	for _, tc := range cases {
		got := ParseRelease(tc.name)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// TestParseReleaseHostileNames walks destructive or reserved names through the parser: it
// must never panic and never invent tags the name does not carry.
func TestParseReleaseHostileNames(t *testing.T) {
	for _, name := range []string{
		`..\..\evil.mkv`, `CON.mkv`, `COM1[Group].mkv`,
		"Show\x00.mp4", strings.Repeat("A", 400) + ".mkv",
	} {
		if got := ParseRelease(name); !reflect.DeepEqual(got, ReleaseSpec{}) {
			t.Errorf("ParseRelease(%q) = %+v, want the zero spec", name, got)
		}
	}
	// NUL (1080p) does carry a resolution tag; only that tag may be read from it.
	got := ParseRelease(`NUL (1080p).mkv`)
	want := ReleaseSpec{Resolution: ptr(1080)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRelease NUL = %+v, want %+v", got, want)
	}
}

func TestParseTorrentTitleBatches(t *testing.T) {
	got := ParseTorrentTitle("[SubsPlease] Show - 01-12 (1080p) [Group]")
	if !got.IsBatch || got.EpisodeStart == nil || *got.EpisodeStart != 1 ||
		got.EpisodeEnd == nil || *got.EpisodeEnd != 12 {
		t.Fatalf("batch parse wrong: %+v", got)
	}
}

// TestParseTorrentTitle pins how show, season and episode flow out of a torrent name.
// ParseAnimeFilename stays the only source of content fields — a batch range's trailing
// "- NN" is trimmed from the delegated show — and rows also document where it currently
// yields nothing (v2-suffixed episodes).
func TestParseTorrentTitle(t *testing.T) {
	cases := []struct {
		name        string
		wantShow    string
		wantSeason  *int
		wantEpStart *int
		wantEpEnd   *int
		wantIsBatch bool
		wantSpec    ReleaseSpec
	}{
		{"[SubsPlease] Frieren - 01 (1080p) [ABC12345].mkv", "Frieren", nil, ptr(1), nil, false,
			ReleaseSpec{Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease")}},
		{"Show S02E05 1080p BluRay - Grp.mkv", "Show", ptr(2), ptr(5), nil, false,
			ReleaseSpec{Resolution: ptr(1080), Source: ptr("BluRay"), ReleaseGroup: ptr("Grp")}},
		{"[SubsPlease] Show - 01-12 (1080p) [Group]", "Show", nil, ptr(1), ptr(12), true,
			ReleaseSpec{Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease"), IsBatch: true}},
		// The en-dash range delegates "Show - 01" from ParseAnimeFilename; the tail is trimmed.
		{"Show - 01 – 12 [1080p][BluRay]", "Show", nil, ptr(1), ptr(12), true,
			ReleaseSpec{Resolution: ptr(1080), Source: ptr("BluRay"), IsBatch: true}},
		{"[Group] Show (Batch) (1080p).mkv", "", nil, nil, nil, true,
			ReleaseSpec{Resolution: ptr(1080), ReleaseGroup: ptr("Group"), IsBatch: true}},
		{"[SubsPlease] Show - 01v2 (1080p).mkv", "", nil, nil, nil, false,
			ReleaseSpec{Resolution: ptr(1080), ReleaseGroup: ptr("SubsPlease")}},
	}
	for _, tc := range cases {
		got := ParseTorrentTitle(tc.name)
		if got.Show != tc.wantShow || got.IsBatch != tc.wantIsBatch {
			t.Errorf("%q: show = %q, batch = %v; want %q, %v", tc.name, got.Show, got.IsBatch, tc.wantShow, tc.wantIsBatch)
		}
		if !reflect.DeepEqual(got.Season, tc.wantSeason) {
			t.Errorf("%q: season = %v, want %v", tc.name, got.Season, tc.wantSeason)
		}
		if !reflect.DeepEqual(got.EpisodeStart, tc.wantEpStart) || !reflect.DeepEqual(got.EpisodeEnd, tc.wantEpEnd) {
			t.Errorf("%q: episodes = %v..%v, want %v..%v", tc.name, got.EpisodeStart, got.EpisodeEnd, tc.wantEpStart, tc.wantEpEnd)
		}
		if !reflect.DeepEqual(got.Spec, tc.wantSpec) {
			t.Errorf("%q:\n got spec %+v\nwant spec %+v", tc.name, got.Spec, tc.wantSpec)
		}
	}
}
