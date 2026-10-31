package torrent

import (
	"sort"
	"strings"
	"unicode"

	"animind-backend-go/internal/scanner"
)

// Pure scoring and filtering for the search pipeline: no I/O, no locks, so the tests cover
// them without an indexer or HTTP.

// tokenOverlap is the Jaccard overlap of the lower-cased tokens in q and title. Title
// tokens lose the parsed release-group tag, or every SubsPlease release would outscore a
// bare-title hit purely on its own group name.
func tokenOverlap(q, title, group string) float64 {
	qTokens := tokenize(q)
	titleTokens := tokenize(title)
	titleTokens = subtractTokens(titleTokens, tokenize(group))
	if len(qTokens) == 0 || len(titleTokens) == 0 {
		return 0
	}
	qSet := make(map[string]bool, len(qTokens))
	for _, t := range qTokens {
		qSet[t] = true
	}
	intersect, union := 0, len(titleTokens)
	seenTitle := make(map[string]bool, len(titleTokens))
	for _, t := range titleTokens {
		seenTitle[t] = true
	}
	for t := range qSet {
		if seenTitle[t] {
			intersect++
		} else {
			union++
		}
	}
	return float64(intersect) / float64(union)
}

// tokenize lower-cases and splits on anything that is not a letter or digit, so "[SubsPlease]"
// and "SubsPlease" tokenize identically and non-Latin titles keep their runes.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func subtractTokens(tokens, drop []string) []string {
	if len(drop) == 0 {
		return tokens
	}
	dropSet := make(map[string]bool, len(drop))
	for _, t := range drop {
		dropSet[t] = true
	}
	out := tokens[:0:0]
	for _, t := range tokens {
		if !dropSet[t] {
			out = append(out, t)
		}
	}
	return out
}

// normalizedSeeders squashes the seeder count into 0..1 with diminishing returns, so a
// 400-seeder swarm cannot drown a freshly released torrent the way a raw count would.
func normalizedSeeders(seeders int) float64 {
	s := float64(seeders)
	return s / (s + 20)
}

// relevanceScore is the brief's 0.7 × token overlap + 0.3 × normalized seeders.
func relevanceScore(q string, r Result) float64 {
	return 0.7*tokenOverlap(q, r.Title, derefString(r.Parsed.Spec.ReleaseGroup)) +
		0.3*normalizedSeeders(r.Seeders)
}

// parsedRichness counts the populated fields of a parse; the merge keeps the richer of two
// rows sharing an info hash.
func parsedRichness(t scanner.TorrentTitle) int {
	n := 0
	for _, s := range []*string{t.Spec.Source, t.Spec.VideoCodec, t.Spec.Subs, t.Spec.ReleaseGroup} {
		if s != nil {
			n++
		}
	}
	for _, i := range []*int{t.Season, t.EpisodeStart, t.EpisodeEnd, t.Spec.Resolution, t.Spec.BitDepth} {
		if i != nil {
			n++
		}
	}
	if t.Show != "" {
		n++
	}
	if len(t.Spec.AudioCodecs) > 0 {
		n++
	}
	return n
}

// rankRows orders a copy of rows by the requested sort; anything unknown, including the
// empty string, means relevance. The copy is never nil, so a zero-row search page still
// marshals its data as [] rather than null.
func rankRows(rows []Result, q, sortBy string) []Result {
	out := make([]Result, len(rows))
	copy(out, rows)
	switch sortBy {
	case "seeders":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Seeders > out[j].Seeders })
	case "size":
		sort.SliceStable(out, func(i, j int) bool { return out[i].SizeBytes > out[j].SizeBytes })
	case "date":
		sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	default:
		type scored struct {
			row   Result
			score float64
		}
		pairs := make([]scored, len(out))
		for i, r := range out {
			pairs[i] = scored{row: r, score: relevanceScore(q, r)}
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })
		for i := range pairs {
			out[i] = pairs[i].row
		}
	}
	return out
}

// filterRows applies the client-side filters in request order: seeders floor (zero-seeder
// rows stay hidden unless minSeeders is explicitly 0), batch, resolution and codec. A row
// whose parse does not state a resolution or codec cannot contradict the request and stays.
func filterRows(rows []Result, q Query) []Result {
	floor := 1
	if q.MinSeeders != nil {
		floor = max(0, *q.MinSeeders)
	}
	out := rows[:0:0]
	for _, r := range rows {
		if r.Seeders < floor {
			continue
		}
		switch q.Batch {
		case "only":
			if !r.Parsed.IsBatch {
				continue
			}
		case "exclude":
			if r.Parsed.IsBatch {
				continue
			}
		}
		if q.Resolution != nil && r.Parsed.Spec.Resolution != nil && *r.Parsed.Spec.Resolution != *q.Resolution {
			continue
		}
		if q.Codec != nil && r.Parsed.Spec.VideoCodec != nil &&
			normalizeCodec(*q.Codec) != normalizeCodec(*r.Parsed.Spec.VideoCodec) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// normalizeCodec maps the release-scene spellings onto the parser's canonical names, so a
// request for "x264" matches a parsed "AVC" (mirrors scanner.canonicalVideoCodec).
func normalizeCodec(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	switch b.String() {
	case "x264", "h264", "avc":
		return "avc"
	case "x265", "h265", "hevc":
		return "hevc"
	default:
		return b.String()
	}
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
