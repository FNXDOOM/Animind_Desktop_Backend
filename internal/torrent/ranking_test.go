package torrent

import (
	"testing"
	"time"

	"animind-backend-go/internal/scanner"
)

func TestTokenOverlap(t *testing.T) {
	cases := []struct {
		name  string
		q     string
		title string
		group string
		want  float64
	}{
		// [SubsPlease] must not count towards the title's overlap: every SubsPlease
		// release would otherwise outrank a bare-title hit.
		{"group tag excluded", "frieren", "[SubsPlease] frieren - 01", "SubsPlease", 0.5},
		{"identical token sets", "frieren 01", "Frieren 01", "", 1},
		{"disjoint", "frieren", "sousou no", "", 0},
		{"empty title", "frieren", "", "", 0},
		{"empty query", "", "frieren", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenOverlap(tc.q, tc.title, tc.group); got != tc.want {
				t.Fatalf("tokenOverlap(%q, %q, %q) = %v, want %v", tc.q, tc.title, tc.group, got, tc.want)
			}
		})
	}
}

func TestNormalizedSeedersAndRelevance(t *testing.T) {
	if got := normalizedSeeders(0); got != 0 {
		t.Fatalf("normalizedSeeders(0) = %v", got)
	}
	if got := normalizedSeeders(20); got != 0.5 {
		t.Fatalf("normalizedSeeders(20) = %v", got)
	}
	row := Result{Title: "frieren 01", Seeders: 20}
	want := 0.7*1 + 0.3*0.5
	if got := relevanceScore("frieren 01", row); got != want {
		t.Fatalf("relevanceScore = %v, want %v", got, want)
	}
}

func rankFixture() []Result {
	return []Result{
		{InfoHash: "low", Title: "frieren 01", Seeders: 1, SizeBytes: 100,
			PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{InfoHash: "high", Title: "Frieren 02", Seeders: 90, SizeBytes: 300,
			PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{InfoHash: "mid", Title: "other show 01", Seeders: 40, SizeBytes: 200,
			PublishedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
	}
}

func order(rows []Result) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.InfoHash
	}
	return out
}

func TestRankRowsSorts(t *testing.T) {
	cases := []struct {
		name string
		sort string
		want []string
	}{
		// relevance favours the matching titles; the unrelated row falls behind despite
		// its mid-tier seeders.
		{"relevance by default", "", []string{"high", "low", "mid"}},
		{"relevance explicit", "relevance", []string{"high", "low", "mid"}},
		{"seeders bypass", "seeders", []string{"high", "mid", "low"}},
		{"size bypass", "size", []string{"high", "mid", "low"}},
		{"date bypass", "date", []string{"high", "mid", "low"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rankRows(rankFixture(), "frieren", tc.sort)
			if got := order(got); !equalStrings(got, tc.want) {
				t.Fatalf("sort=%q order %v, want %v", tc.sort, got, tc.want)
			}
		})
	}
}

func TestRankRowsStableTies(t *testing.T) {
	rows := []Result{
		{InfoHash: "first", Title: "frieren 01", Seeders: 10},
		{InfoHash: "second", Title: "frieren 01", Seeders: 10},
	}
	got := rankRows(rows, "frieren", "seeders")
	if got[0].InfoHash != "first" || got[1].InfoHash != "second" {
		t.Fatalf("ties must keep input order: %v", order(got))
	}
}

func TestParsedRichness(t *testing.T) {
	empty := Result{Title: "x"}
	full := Result{Title: "x", Parsed: scanner.ParseTorrentTitle("[SubsPlease] Frieren - 01-12 (1080p)")}
	if parsedRichness(empty.Parsed) != 0 {
		t.Fatalf("unparsed row must score 0: %d", parsedRichness(empty.Parsed))
	}
	if parsedRichness(full.Parsed) <= parsedRichness(empty.Parsed) {
		t.Fatalf("parsed row must outscore an unparsed one: %d", parsedRichness(full.Parsed))
	}
}

func TestFilterRows(t *testing.T) {
	rows := []Result{
		{InfoHash: "dead", Title: "frieren", Seeders: 0},
		{InfoHash: "batch", Title: "frieren batch", Seeders: 2,
			Parsed: scanner.TorrentTitle{IsBatch: true}},
		{InfoHash: "sd", Title: "frieren 480p", Seeders: 3,
			Parsed: scanner.TorrentTitle{Spec: scanner.ReleaseSpec{Resolution: ptrInt(480)}}},
		{InfoHash: "hd", Title: "frieren 1080p", Seeders: 4,
			Parsed: scanner.TorrentTitle{Spec: scanner.ReleaseSpec{Resolution: ptrInt(1080)}}},
		{InfoHash: "hevc", Title: "frieren 1080p hevc", Seeders: 5,
			Parsed: scanner.TorrentTitle{Spec: scanner.ReleaseSpec{Resolution: ptrInt(1080), VideoCodec: ptrStr("HEVC")}}},
	}

	t.Run("zero seeders hidden unless minSeeders is zero", func(t *testing.T) {
		if got := filterRows(rows, Query{}); slicesContain(order(got), "dead") {
			t.Fatalf("dead rows hide by default: %v", order(got))
		}
		if got := filterRows(rows, Query{MinSeeders: ptrInt(0)}); !slicesContain(order(got), "dead") {
			t.Fatalf("minSeeders=0 keeps dead rows: %v", order(got))
		}
	})

	t.Run("minSeeders floor", func(t *testing.T) {
		got := order(filterRows(rows, Query{MinSeeders: ptrInt(4)}))
		want := []string{"hd", "hevc"} // filterRows preserves input order
		if !equalStrings(got, want) {
			t.Fatalf("minSeeders=4: %v, want %v", got, want)
		}
	})

	t.Run("batch only and exclude", func(t *testing.T) {
		only := order(filterRows(rows, Query{Batch: "only"}))
		if !equalStrings(only, []string{"batch"}) {
			t.Fatalf("batch=only: %v", only)
		}
		excl := filterRows(rows, Query{Batch: "exclude"})
		if slicesContain(order(excl), "batch") || !equalStrings(order(excl), []string{"sd", "hd", "hevc"}) {
			t.Fatalf("batch=exclude: %v", order(excl))
		}
	})

	t.Run("resolution", func(t *testing.T) {
		got := order(filterRows(rows, Query{Resolution: ptrInt(1080)}))
		// "frieren 1080p hevc" and "frieren 1080p" match; the unparsed batch row cannot
		// contradict the request and stays.
		if !equalStrings(got, []string{"batch", "hd", "hevc"}) {
			t.Fatalf("resolution=1080: %v", got)
		}
	})

	t.Run("codec", func(t *testing.T) {
		got := order(filterRows(rows, Query{Codec: ptrStr("x264")}))
		// "x264" must normalize to the parsed AVC so only the HEVC row drops; rows without
		// a parsed codec cannot contradict the request and stay.
		if !equalStrings(got, []string{"batch", "sd", "hd"}) {
			t.Fatalf("codec=x264: %v", got)
		}
	})
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func slicesContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
