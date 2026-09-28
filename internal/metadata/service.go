// Package metadata resolves a release title into catalog information.
//
// AniList is tried first because it carries artwork, trailers and an id the library can
// dedupe against. MyAnimeList is the fallback for when AniList throttles or has no match.
// Results are cached per title and de-duplicated across concurrent lookups, because a scan
// of a hundred episodes of one series must not send a hundred identical requests.
package metadata

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"animind-backend-go/internal/config"
)

// Trailer is a video preview reference.
type Trailer struct {
	ID        string  `json:"id"`
	Site      string  `json:"site"`
	Thumbnail *string `json:"thumbnail"`
}

// Metadata is the provider-independent result of a title lookup.
type Metadata struct {
	Source        string   `json:"source"`
	ID            int      `json:"id"`
	Title         string   `json:"title"`
	Synopsis      *string  `json:"synopsis"`
	CoverImageURL *string  `json:"cover_image_url"`
	Genres        []string `json:"genres"`
	Rating        *float64 `json:"rating"`
	EpisodeCount  *int     `json:"episode_count"`
	Studio        *string  `json:"studio"`
	Status        *string  `json:"status"`
	Year          *string  `json:"year"`
	AnilistID     *int     `json:"anilist_id"`
	Trailer       *Trailer `json:"trailer"`
}

// Service orchestrates the providers and the cache.
type Service struct {
	anilist *AniListClient
	mal     *MALClient
	ttl     time.Duration

	mu       sync.Mutex
	cache    map[string]*cacheEntry
	inFlight map[string]*inFlightRequest
}

type cacheEntry struct {
	meta      *Metadata
	expiresAt time.Time
}

// inFlightRequest lets concurrent lookups of the same title share one upstream call.
type inFlightRequest struct {
	done  chan struct{}
	meta  *Metadata
	err   error
	ready sync.Once
}

func (r *inFlightRequest) finish(meta *Metadata, err error) {
	r.meta = meta
	r.err = err
	r.ready.Do(func() { close(r.done) })
}

// New builds the orchestrator.
func New(cfg *config.Config, logger *slog.Logger) *Service {
	return &Service{
		anilist:  NewAniList(cfg, logger),
		mal:      NewMAL(cfg, logger),
		ttl:      cfg.Metadata.MetadataCacheTTL,
		cache:    make(map[string]*cacheEntry),
		inFlight: make(map[string]*inFlightRequest),
	}
}

// Fetch resolves a title, returning nil when no provider recognizes it. A nil result is
// not an error: the scanner then keeps the folder name as the title.
func (s *Service) Fetch(ctx context.Context, title string) (*Metadata, error) {
	key := NormalizeTitleKey(title)
	if key == "" {
		return nil, nil
	}

	s.mu.Lock()
	if entry, ok := s.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		meta := entry.meta
		s.mu.Unlock()
		return meta, nil
	}
	if request, ok := s.inFlight[key]; ok {
		s.mu.Unlock()
		select {
		case <-request.done:
			return request.meta, request.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	request := &inFlightRequest{done: make(chan struct{})}
	s.inFlight[key] = request
	s.mu.Unlock()

	meta, err := s.fetchUncached(ctx, title)
	request.finish(meta, err)

	s.mu.Lock()
	delete(s.inFlight, key)
	if err == nil {
		s.cache[key] = &cacheEntry{meta: meta, expiresAt: time.Now().Add(s.ttl)}
	}
	s.mu.Unlock()
	return meta, err
}

func (s *Service) fetchUncached(ctx context.Context, title string) (*Metadata, error) {
	media, err := s.anilist.FetchMeta(ctx, title)
	if err != nil {
		slog.Debug("AniList lookup failed, trying MyAnimeList",
			slog.String("title", title), slog.String("error", describe(err)))
	}
	if err == nil && media != nil {
		return fromAniList(media), nil
	}

	anime, malErr := s.mal.FetchMeta(ctx, title)
	if malErr != nil || anime == nil {
		return nil, nil
	}
	return fromMAL(anime), nil
}

func fromAniList(m *Media) *Metadata {
	meta := &Metadata{
		Source: "anilist",
		ID:     m.ID,
		Title:  firstNonEmpty(deref(m.Title.English), m.Title.Romaji),
		Genres: m.Genres,
		Status: orEmptyString(m.Status),
	}
	if m.Description != nil {
		cleaned := StripHTML(*m.Description)
		meta.Synopsis = &cleaned
	}
	meta.CoverImageURL = m.CoverImage.Large
	if meta.CoverImageURL == nil {
		meta.CoverImageURL = m.CoverImage.Medium
	}
	if m.Average != nil {
		// AniList scores are 0-100; the library and both clients display 0-10.
		rating := float64(*m.Average) / 10
		meta.Rating = &rating
	}
	meta.EpisodeCount = m.Episodes
	if len(m.Studios.Nodes) > 0 {
		studio := m.Studios.Nodes[0].Name
		meta.Studio = &studio
	}
	if m.SeasonYear != nil {
		year := itoa(*m.SeasonYear)
		meta.Year = &year
	}
	anilistID := m.ID
	meta.AnilistID = &anilistID
	if m.Trailer != nil && m.Trailer.ID != "" {
		meta.Trailer = &Trailer{ID: m.Trailer.ID, Site: orDefaultString(m.Trailer.Site, "youtube"), Thumbnail: m.Trailer.Thumbnail}
	}
	return meta
}

func fromMAL(a *MALAnime) *Metadata {
	meta := &Metadata{
		Source: "myanimelist",
		ID:     a.ID,
		Title:  firstNonEmpty(deref(a.AlternativeTitles.English), a.Title),
	}
	if a.Synopsis != nil {
		cleaned := StripHTML(*a.Synopsis)
		meta.Synopsis = &cleaned
	}
	meta.CoverImageURL = a.Picture.Large
	if meta.CoverImageURL == nil {
		meta.CoverImageURL = a.Picture.Medium
	}
	for _, g := range a.Genres {
		if g.Name != "" {
			meta.Genres = append(meta.Genres, g.Name)
		}
	}
	meta.Rating = a.Mean
	meta.EpisodeCount = a.NumEpisodes
	meta.Status = a.Status
	if len(a.Studios) > 0 {
		studio := a.Studios[0].Name
		meta.Studio = &studio
	}
	if a.StartSeason != nil && a.StartSeason.Year != nil {
		year := itoa(*a.StartSeason.Year)
		meta.Year = &year
	}
	return meta
}

// nonWordPattern collapses anything that is not a letter or digit into a single space.
var nonWordPattern = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizeTitleKey produces the comparison key used for caching and title matching:
// unicode-normalised, alphanumeric-only, single-spaced, lowercase.
func NormalizeTitleKey(title string) string {
	lowered := strings.ToLower(strings.TrimSpace(title))
	trimmed := norm.NFKD.String(lowered)
	trimmed = strings.Map(func(r rune) rune {
		if r < 128 {
			return r
		}
		// Combining marks left by NFKD would otherwise become separator characters.
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, trimmed)
	spaced := nonWordPattern.ReplaceAllString(trimmed, " ")
	return strings.Join(strings.Fields(spaced), " ")
}

var tagPattern = regexp.MustCompile(`<[^>]*>`)

// StripHTML removes markup from a provider description.
//
// AniList sends HTML even with asHtml:false in some fields, and the library stores plain
// text that both clients render directly, so tags must not survive the import.
func StripHTML(text string) string {
	if text == "" {
		return ""
	}
	cleaned := tagPattern.ReplaceAllString(text, "")
	cleaned = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#039;", "'", "&apos;", "'", "&amp;", "&", "&nbsp;", " ").Replace(cleaned)
	return strings.Join(strings.Fields(cleaned), " ")
}

func deref[T any](value *T) T {
	var zero T
	if value == nil {
		return zero
	}
	return *value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func orEmptyString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func orDefaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
