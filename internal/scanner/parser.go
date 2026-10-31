// Package scanner indexes the media library: it discovers files, turns release filenames
// into show and episode records, and prunes rows whose file disappeared.
//
// Parsing is deterministic and ordered: the folder path is tried first because a release
// group's filename is far more reliable than a bare episode number, and an LLM guess is
// used only when nothing matched and the operator opted in.
package scanner

import (
	"regexp"
	"strconv"
	"strings"
)

// ContentType is the kind of release a file represents.
type ContentType string

const (
	ContentTypeTV      ContentType = "tv"
	ContentTypeOVA     ContentType = "ova"
	ContentTypeONA     ContentType = "ona"
	ContentTypeSpecial ContentType = "special"
	ContentTypeMovie   ContentType = "movie"
	ContentTypeUnknown ContentType = "unknown"
)

// Parsed describes one release file.
type Parsed struct {
	Title       string
	Episode     float64
	Season      *int
	ContentType ContentType
	Quality     string
	Group       string
}

var (
	extensionPattern      = regexp.MustCompile(`(?i)\.[a-z0-9]{2,4}$`)
	groupTagPattern       = regexp.MustCompile(`^\[([^\]]+)\]\s*(.*)$`)
	parenNoisePattern     = regexp.MustCompile(`\s*\([^)]*\)\s*`)
	bracketNoisePattern   = regexp.MustCompile(`\s*\[[^\]]*\]\s*`)
	dotUnderscorePattern  = regexp.MustCompile(`[._]+`)
	whitespacePattern     = regexp.MustCompile(`\s+`)
	folderSeasonPattern   = regexp.MustCompile(`\s+[Ss]\d{1,2}\b.*$`)
	folderReleasePattern  = regexp.MustCompile(`(?i)\s+(\d{3,4}p|BluRay|WEBRip|WEB-DL|HDTV|BDRip|DVDRip|x264|x265|HEVC|AVC|AAC|Opus|FLAC|H\.?264|H\.?265)\b.*`)
	folderGroupPattern    = regexp.MustCompile(`\s+-\s*\S+\s*$`)
	trailingGroupPattern  = regexp.MustCompile(`^(.+?)\s*[-–]\s*(\d{1,4})\s*$`)
	releaseGroupSuffix    = regexp.MustCompile(`[-–]\s*([A-Za-z][A-Za-z0-9]*)\s*$`)
	bareSeasonEpisode     = regexp.MustCompile(`(?i)^[Ss](\d{1,2})[Ee](\d{1,4})(?:[Vv]\d+)?$`)
	prefixedSeasonEpisode = regexp.MustCompile(`(?i)^(.+?)\s*(?:[-–]\s*)?[Ss](\d{1,2})[Ee](\d{1,4})(?:[Vv]\d+)?\b`)
	episodeWordPattern    = regexp.MustCompile(`^(.+?)\s*[Ee]p(?:isode)?\s*(\d{1,4})\s*$`)
	trailingNumberPattern = regexp.MustCompile(`^(.+?)\s+(\d{1,4})\s*$`)
	seasonWordPattern     = regexp.MustCompile(`(?i)^season\s*(\d{1,2})$`)
	seasonWordAnyPattern  = regexp.MustCompile(`(?i)\bseason\s*(\d{1,2})\b`)
	seasonShortPattern    = regexp.MustCompile(`(?i)^s(\d{1,2})$`)
	seasonShortAnyPattern = regexp.MustCompile(`(?i)\bs(\d{1,2})\b`)
	seasonSuffixPattern   = regexp.MustCompile(`(?i)\s*[-–]\s*season\s*\d{1,2}\s*$`)
	seasonSuffixPattern2  = regexp.MustCompile(`(?i)\s*season\s*\d{1,2}\s*$`)
	qualityPattern        = regexp.MustCompile(`(?i)\b(480p|720p|1080p|2160p|4K|BD|BluRay|WEB-DL)\b`)

	pathSeasonPattern = regexp.MustCompile(`(?i)^season\s*(\d{1,2})$`)
	pathOVA           = regexp.MustCompile(`(?i)\b(ova|oad)\b`)
	pathONA           = regexp.MustCompile(`(?i)\bona\b`)
	pathMovie         = regexp.MustCompile(`(?i)\b(movie|film|theatrical)\b`)
	pathSpecial       = regexp.MustCompile(`(?i)\b(special|specials|sp|extra|extras)\b`)
)

// StripExtension removes a trailing file extension of two to four alphanumeric characters.
func StripExtension(name string) string { return extensionPattern.ReplaceAllString(name, "") }

// stripGroupTag removes a leading "[Group]" marker and reports the group it named.
func stripGroupTag(name string) (cleaned, group string) {
	match := groupTagPattern.FindStringSubmatch(name)
	if match == nil {
		return name, ""
	}
	return match[2], match[1]
}

// stripTrailingNoise drops release decorations: dots used as separators, parenthetical
// hash or source tags, and square-bracket group tags.
func stripTrailingNoise(name string) string {
	out := dotUnderscorePattern.ReplaceAllString(name, " ")
	out = parenNoisePattern.ReplaceAllString(out, " ")
	out = bracketNoisePattern.ReplaceAllString(out, " ")
	out = whitespacePattern.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

// CleanFolderTitle reduces a release directory name to a show title.
//
// "Re.ZERO.Starting.Life.in.Another.World.S03.1080p.BluRay.Opus.2.0.x265-Headpatter"
// becomes "Re ZERO Starting Life in Another World": the season marker, the quality and
// codec run, and the trailing release group are all cut.
func CleanFolderTitle(folderName string) string {
	out := dotUnderscorePattern.ReplaceAllString(folderName, " ")
	out = folderSeasonPattern.ReplaceAllString(out, "")
	out = folderReleasePattern.ReplaceAllString(out, "")
	out = folderGroupPattern.ReplaceAllString(out, "")
	return strings.TrimSpace(whitespacePattern.ReplaceAllString(out, " "))
}

// extractEpisode parses a title-and-number form. Patterns are tried in the order the
// original parser used, because "Title - 01" must win before "Title 01".
func extractEpisode(name string) (title string, episode float64, season *int, ok bool) {
	if match := trailingGroupPattern.FindStringSubmatch(name); match != nil {
		return strings.TrimSpace(match[1]), toNumber(match[2]), nil, true
	}
	if match := bareSeasonEpisode.FindStringSubmatch(name); match != nil {
		return "", toNumber(match[2]), intPointer(match[1]), true
	}
	if match := prefixedSeasonEpisode.FindStringSubmatch(name); match != nil {
		return strings.TrimSpace(match[1]), toNumber(match[3]), intPointer(match[2]), true
	}
	if match := episodeWordPattern.FindStringSubmatch(name); match != nil {
		return strings.TrimSpace(match[1]), toNumber(match[2]), nil, true
	}
	if match := trailingNumberPattern.FindStringSubmatch(name); match != nil {
		return strings.TrimSpace(match[1]), toNumber(match[2]), nil, true
	}
	return "", 0, nil, false
}

// ExtractEpisodeByStrongPatterns matches an episode number inside a longer string, used
// when a filename is really a subtitle file where the title and language tokens interleave.
//
// Patterns mirror the migration script's order: SxxEyy, "Ep yy", a dash-separated number,
// then a language-aware trailing number.
func ExtractEpisodeByStrongPatterns(name string) (float64, bool) {
	strong := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bS\d{1,2}E(\d{1,4})\b`),
		regexp.MustCompile(`(?i)\bE(?:P|pisode)?\s*(\d{1,4})\b`),
		// A dash-separated number has to stand alone: Go's regexp engine has no lookahead,
		// so the boundary is consumed instead of asserted.
		regexp.MustCompile(`(?:^|\s)-\s*(\d{1,4})(?:\s|$)`),
	}
	for _, pattern := range strong {
		if match := pattern.FindStringSubmatch(name); match != nil {
			n := toNumber(match[1])
			if n >= 1 && n <= 5000 {
				return n, true
			}
		}
	}
	return 0, false
}

// extractSeasonFromPathParts finds a season marker in the directory chain, scanning from
// the file backwards so the closest enclosing "Season 3" wins over a stray match higher up.
func extractSeasonFromPathParts(parts []string) *int {
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if match := seasonWordPattern.FindStringSubmatch(part); match != nil {
			return intPointer(match[1])
		}
		if match := seasonWordAnyPattern.FindStringSubmatch(part); match != nil {
			return intPointer(match[1])
		}
		if match := seasonShortPattern.FindStringSubmatch(part); match != nil {
			return intPointer(match[1])
		}
		spaced := strings.ReplaceAll(part, ".", " ")
		spaced = strings.ReplaceAll(spaced, "_", " ")
		spaced = strings.ReplaceAll(spaced, "-", " ")
		if match := seasonShortAnyPattern.FindStringSubmatch(spaced); match != nil {
			return intPointer(match[1])
		}
	}
	return nil
}

// inferContentTypeFromPathParts classifies a release from its full path text.
// Order matters: a "Movie" tag beats an "OVA" directory, and specials are only decided
// after both.
func inferContentTypeFromPathParts(parts []string) ContentType {
	joined := strings.ToLower(whitespacePattern.ReplaceAllString(
		strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(strings.Join(parts, " ")), " "))
	switch {
	case pathMovie.MatchString(joined):
		return ContentTypeMovie
	case pathOVA.MatchString(joined):
		return ContentTypeOVA
	case pathONA.MatchString(joined):
		return ContentTypeONA
	case pathSpecial.MatchString(joined):
		return ContentTypeSpecial
	default:
		return ContentTypeTV
	}
}

// stripSeasonSuffix removes a trailing "Season N" from a title.
func stripSeasonSuffix(title string) string {
	out := seasonSuffixPattern.ReplaceAllString(title, "")
	out = seasonSuffixPattern2.ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}

// ParseAnimeFilename parses a single filename, with or without its extension.
// It returns nil when no episode number can be identified.
func ParseAnimeFilename(filename string) *Parsed {
	name := StripExtension(strings.TrimSpace(filename))
	name, group := stripGroupTag(name)
	name = stripTrailingNoise(name)
	if name == "" {
		return nil
	}

	title, episode, season, ok := extractEpisode(name)
	if !ok {
		return nil
	}
	parsed := &Parsed{Title: title, Episode: episode, Season: season, Group: group}
	if match := qualityPattern.FindStringSubmatch(filename); match != nil {
		parsed.Quality = match[1]
	}
	return parsed
}

// ParseFolderPath resolves a full library-relative path into a show, season and episode.
//
// Folder titles win over filename titles: "Frieren/Season 1/S01E01.mkv" names the show
// "Frieren", not the empty title that "S01E01" would produce.
func ParseFolderPath(relativePath string) *Parsed {
	normalized := strings.ReplaceAll(relativePath, "\\", "/")
	parts := strings.Split(normalized, "/")
	if len(parts) == 0 {
		return nil
	}

	filename := parts[len(parts)-1]
	var folderTitle string
	if len(parts) > 1 {
		folderTitle = parts[0]
	}

	seasonFromPath := extractSeasonFromPathParts(parts[:len(parts)-1])
	contentType := inferContentTypeFromPathParts(parts)

	parsed := ParseAnimeFilename(filename)
	if parsed == nil {
		return nil
	}
	if parsed.Season == nil && seasonFromPath != nil {
		parsed.Season = seasonFromPath
	}
	if parsed.ContentType == "" {
		parsed.ContentType = contentType
	}

	if folderTitle != "" && len(strings.TrimSpace(folderTitle)) > 1 && !pathSeasonPattern.MatchString(strings.TrimSpace(folderTitle)) {
		cleaned := CleanFolderTitle(strings.TrimSpace(folderTitle))
		candidate := stripSeasonSuffix(cleaned)
		if candidate == "" {
			candidate = cleaned
		}
		if candidate == "" {
			candidate = strings.TrimSpace(folderTitle)
		}
		parsed.Title = candidate
	} else if strings.TrimSpace(parsed.Title) == "" {
		return nil
	}
	return parsed
}

// SeasonForContentType defaults the season when a release type has no season concept:
// movies, OVAs and specials live in season 0, ordinary episodes in season 1.
func SeasonForContentType(season *int, contentType ContentType) int {
	if season != nil {
		if *season >= 0 {
			return *season
		}
	}
	switch contentType {
	case ContentTypeTV, ContentTypeUnknown:
		return 1
	default:
		return 0
	}
}

func toNumber(raw string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return n
}

func intPointer(raw string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	return &n
}

// ref builds a pointer to a value, for optional ReleaseSpec fields.
func ref[T any](v T) *T { return &v }

// ReleaseSpec is the technical profile of one release name: every field the download
// pipeline needs to compare a torrent against the library's quality preferences. A field the
// name never tags stays nil, empty or false — nothing is inferred from neighbours, so
// "Hi10P" alone is a bit depth and never a video codec.
type ReleaseSpec struct {
	Resolution   *int
	Source       *string
	VideoCodec   *string
	BitDepth     *int
	AudioCodecs  []string
	DualAudio    bool
	Subs         *string
	HDR          bool
	ReleaseGroup *string
	IsBatch      bool
}

// TorrentTitle merges what a torrent name says about its content (show, season, episode
// span) with its technical profile. EpisodeStart holds a single episode's number or the
// first episode of a batch; EpisodeEnd is only set for explicit ranges.
type TorrentTitle struct {
	Show         string
	Season       *int
	EpisodeStart *int
	EpisodeEnd   *int
	IsBatch      bool
	Spec         ReleaseSpec
}

var (
	// The frame-rate suffixes ("1080p60", "720p30fps") ride on the same resolution token.
	resolutionPattern = regexp.MustCompile(`(?i)\b(480|720|1080|2160)p(?:\d{1,2})?(?:fps)?\b`)
	sourcePattern     = regexp.MustCompile(`(?i)\b(WEB[-. ]?DL|WEBRip|BluRay|BDRip|HDTV|DVD(?:Rip)?)\b`)
	// "H.264" survives the dot-to-space pass as "H 264", so the separator class admits spaces.
	videoCodecPattern = regexp.MustCompile(`(?i)\b(x264|h[ .]?264|avc|x265|h[ .]?265|hevc|av1|vp9)\b`)
	bitDepthPattern   = regexp.MustCompile(`(?i)\b(Hi10P?|10[-. ]?bit|8[-. ]?bit)\b`)
	// E-AC-3 must precede AC-3 in the alternation or the leading E would detach.
	audioCodecPattern = regexp.MustCompile(`(?i)\b(E-?AC-?3(?:\.?\d\.\d)?|TrueHD(?:\.?\d\.\d)?|DTS(?:-HD(?: MA)?|:X)?|AAC(?:\.?\d\.\d)?|FLAC|Opus|AC-?3(?:\.?\d\.\d)?|Vorbis|ALAC|MP3|PCM(?:\s\d\.\d)?)\b`)
	// Only tags that name subtitle tracks feed Subs; audio tags like "Dual Audio" say
	// nothing about subtitles and set DualAudio instead.
	subsPattern  = regexp.MustCompile(`(?i)\b(Multi(?:ple)?[ -]?Sub(?:title)?s?|Soft[ -]?Subs?|Hard[ -]?Subs?|No[ -]?Subs?)\b`)
	hdrPattern   = regexp.MustCompile(`(?i)\b(HDR(?:10\+?)?|Dolby Vision|DV)\b`)
	episodeRange = regexp.MustCompile(`(?i)(?:e|ep\.?|\s-\s|\s)(\d{1,4})\s*(?:-|~|–|to)\s*e?(\d{1,4})\b`)
	// ParseAnimeFilename reads a batch's range end as its episode and leaves the range start
	// behind in the title. The leftover tail depends on the separator: dash ranges delegate
	// "Show - 01" (separator + start), while "~" and "to" ranges delegate "Show - 01 ~" /
	// "Show - 01 to" (start + separator). The trim removes both shapes — an optional leading
	// dash, the start number, then an optional trailing separator — anchored to the title's
	// end at a word boundary before the tail's whitespace so numbers inside the show name
	// itself survive.
	batchRangeTailPattern = regexp.MustCompile(`(?i)(?:^|\b)\s*(?:[-–]\s*)?\d{1,4}\s*(?:[-–~]|\bto\b)?\s*$`)
	batchWord             = regexp.MustCompile(`(?i)\b(batch|complete)\b`)
)

// trailingTagTokens are the technical tokens a trailing "-Token" suffix may not be read as:
// without this guard a bracket-less "Show.S01.WEB-DL" would name its release group "DL".
var trailingTagTokens = map[string]bool{
	"264": true, "265": true, "8bit": true, "10bit": true, "aac": true, "ac3": true, "av1": true,
	"avc": true, "bd": true, "bdrip": true, "bluray": true, "batch": true, "dts": true,
	"dts-hd": true, "dv": true, "dvd": true, "dvdrip": true, "dl": true, "eac3": true,
	"flac": true, "h264": true, "h265": true, "hd": true, "hdtv": true, "hdr": true,
	"hdr10": true, "hdr10+": true, "hevc": true, "hybrid": true, "ma": true, "mp3": true,
	"opus": true, "pcm": true, "repack": true, "remux": true, "truehd": true, "v2": true,
	"vorbis": true, "web": true, "web-dl": true, "webdl": true, "webrip": true, "x264": true,
	"x265": true, "alac": true,
}

// ParseRelease reads the technical tags out of a release name.
//
// Tags are matched after the leading "[Group]" marker is removed but while bracketed and
// parenthesised sections stay intact: release names carry "(1080p)", "[Dual Audio]" and
// CRC hashes inside those sections, and a full noise strip would delete exactly what this
// function is asked to read. Dots and underscores become spaces first so dotted names like
// "Show.1080p.WEB-DL.H.264" tokenize like spaced ones.
func ParseRelease(raw string) ReleaseSpec {
	stripped := StripExtension(raw)
	body, group := stripGroupTag(stripped)
	loose := whitespacePattern.ReplaceAllString(dotUnderscorePattern.ReplaceAllString(body, " "), " ")
	denoised := stripTrailingNoise(body)

	spec := ReleaseSpec{}
	if m := resolutionPattern.FindStringSubmatch(loose); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			spec.Resolution = &n
		}
	}
	if m := sourcePattern.FindStringSubmatch(loose); m != nil {
		spec.Source = ref(canonicalSource(m[1]))
	}
	if m := videoCodecPattern.FindStringSubmatch(loose); m != nil {
		spec.VideoCodec = ref(canonicalVideoCodec(m[1]))
	}
	if m := bitDepthPattern.FindStringSubmatch(loose); m != nil {
		spec.BitDepth = ref(canonicalBitDepth(m[0]))
	}
	for _, m := range audioCodecPattern.FindAllStringSubmatch(loose, -1) {
		spec.AudioCodecs = append(spec.AudioCodecs, canonicalAudioCodec(m[1]))
	}
	spec.AudioCodecs = dedupeCodecs(spec.AudioCodecs)
	if m := subsPattern.FindStringSubmatch(loose); m != nil {
		spec.Subs = ref(canonicalSubs(m[0]))
	}
	flattened := strings.ReplaceAll(strings.ToLower(loose), "-", " ")
	spec.DualAudio = strings.Contains(flattened, "dual audio") || strings.Contains(flattened, "multi audio")
	spec.HDR = hdrPattern.MatchString(loose)
	if group != "" {
		spec.ReleaseGroup = ref(group)
	} else if m := releaseGroupSuffix.FindStringSubmatch(denoised); m != nil && !trailingTagTokens[strings.ToLower(m[1])] {
		spec.ReleaseGroup = ref(m[1])
	}
	spec.IsBatch = batchWord.MatchString(stripped) || episodeRange.MatchString(stripped)
	return spec
}

// ParseTorrentTitle resolves a torrent name into its content identity and technical spec.
// ParseAnimeFilename stays the only source of show, season and episode; the range check
// runs afterwards because "Show - 01-12" parses as episode 12 there, and a batch's first
// episode is the more useful reading for the download pipeline. Once the range is
// confirmed, the start number and its separator are trimmed from the delegated title for
// every range shape: "Show - 01", "Show - 01 ~" and "Show - 01 to" all clean to "Show".
func ParseTorrentTitle(raw string) TorrentTitle {
	stripped := StripExtension(raw)
	spec := ParseRelease(raw)
	title := TorrentTitle{Spec: spec, IsBatch: spec.IsBatch}
	parsed := ParseAnimeFilename(stripped)
	if parsed == nil {
		return title
	}
	title.Show = parsed.Title
	title.Season = parsed.Season
	if parsed.Episode > 0 {
		episode := int(parsed.Episode)
		title.EpisodeStart = &episode
	}
	if m := episodeRange.FindStringSubmatch(stripped); m != nil {
		start, startErr := strconv.Atoi(m[1])
		end, endErr := strconv.Atoi(m[2])
		if startErr == nil && endErr == nil {
			title.EpisodeStart = &start
			title.EpisodeEnd = &end
			title.IsBatch = true
			title.Show = strings.TrimSpace(batchRangeTailPattern.ReplaceAllString(title.Show, ""))
		}
	}
	return title
}

func canonicalSource(tag string) string {
	low := strings.ReplaceAll(strings.ToLower(tag), " ", "")
	switch low {
	case "web-dl", "web.dl", "webdl":
		return "WEB-DL"
	case "webrip":
		return "WEBRip"
	case "bluray":
		return "BluRay"
	case "bdrip":
		return "BDRip"
	case "hdtv":
		return "HDTV"
	case "dvdrip":
		return "DVDRip"
	case "dvd":
		return "DVD"
	default:
		return tag
	}
}

func canonicalVideoCodec(tag string) string {
	low := strings.NewReplacer(" ", "", ".", "", "-", "").Replace(strings.ToLower(tag))
	switch low {
	case "x264", "h264", "avc":
		return "AVC"
	case "x265", "h265", "hevc":
		return "HEVC"
	case "av1":
		return "AV1"
	case "vp9":
		return "VP9"
	default:
		return tag
	}
}

func canonicalBitDepth(tag string) int {
	if strings.Contains(strings.ToLower(tag), "8") {
		return 8
	}
	return 10
}

func canonicalAudioCodec(tag string) string {
	low := strings.ToLower(tag)
	switch {
	case strings.HasPrefix(low, "e-ac-3"), strings.HasPrefix(low, "eac-3"), strings.HasPrefix(low, "eac3"):
		return "E-AC-3"
	case strings.HasPrefix(low, "truehd"):
		return "TrueHD"
	case strings.HasPrefix(low, "dts-hd"):
		if strings.Contains(low, "ma") {
			return "DTS-HD MA"
		}
		return "DTS-HD"
	case strings.HasPrefix(low, "dts:x"):
		return "DTS-X"
	case strings.HasPrefix(low, "dts"):
		return "DTS"
	case strings.HasPrefix(low, "aac"):
		return "AAC"
	case strings.HasPrefix(low, "flac"):
		return "FLAC"
	case strings.HasPrefix(low, "opus"):
		return "Opus"
	case strings.HasPrefix(low, "vorbis"):
		return "Vorbis"
	case strings.HasPrefix(low, "alac"):
		return "ALAC"
	case strings.HasPrefix(low, "mp3"):
		return "MP3"
	case strings.HasPrefix(low, "pcm"):
		return "PCM"
	case strings.HasPrefix(low, "ac3"), strings.HasPrefix(low, "ac-3"):
		return "AC-3"
	default:
		return tag
	}
}

// canonicalSubs maps a subtitle-tag match to the pipeline's coarse labels. Only tags that
// name subtitle tracks reach it: audio tags like "Dual Audio" say nothing about subtitles,
// so they set DualAudio alone and leave Subs nil.
func canonicalSubs(tag string) string {
	low := strings.ToLower(tag)
	switch {
	case strings.Contains(low, "soft"):
		return "softsub"
	case strings.Contains(low, "hard"):
		return "hardsub"
	case strings.HasPrefix(low, "no"):
		return "none"
	default:
		return "multi"
	}
}

// dedupeCodecs keeps the first spelling of each codec: names often repeat "AAC 2.0 … AAC 5.1".
func dedupeCodecs(codecs []string) []string {
	if len(codecs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(codecs))
	out := make([]string, 0, len(codecs))
	for _, codec := range codecs {
		if codec == "" || seen[codec] {
			continue
		}
		seen[codec] = true
		out = append(out, codec)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
