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
