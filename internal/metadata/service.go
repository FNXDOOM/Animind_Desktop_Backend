// Package metadata resolves a release title into catalog information. AniList is first, for
// its artwork, trailer and dedupe id, with MyAnimeList covering a miss or throttle. Lookups
// are cached per title and shared across concurrent ones, so a scan asks providers once.
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

	"animind-backend-go/internal/cache"
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
	// AlternateTitles carries every other name a provider knows the show by: synonyms plus
	// the romaji and native titles. The download pipeline matches torrent names against
	// them, and the scanner's fuzzy title match reads them too.
	AlternateTitles []string `json:"alternate_titles,omitempty"`
}

// Service orchestrates the providers and the cache.
type Service struct {
	anilist *AniListClient
	mal     *MALClient
	ttl     time.Duration
	// shared is the cross-process half of the cache: nil keeps lookups inside this process,
	// which is what a single-node scan has always done.
	shared cache.Store
	logger *slog.Logger

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

// New builds the orchestrator. stateCache may be nil.
func New(cfg *config.Config, stateCache cache.Store, logger *slog.Logger) *Service {
	return &Service{
		anilist:  NewAniList(cfg, logger),
		mal:      NewMAL(cfg, logger),
		ttl:      cfg.Metadata.MetadataCacheTTL,
		shared:   stateCache,
		logger:   logger,
		cache:    make(map[string]*cacheEntry),
		inFlight: make(map[string]*inFlightRequest),
	}
}

// Fetch resolves a title; a nil result is not an error, it means no provider matched, and
// the scanner then keeps the folder name as the title. The local map makes one scan cheap,
// the shared store the next one, so a restart keeps its answers and its throttle budget.
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

	// Only the leader consults the shared store: readers waiting on the channel receive the
	// same answer, so one title costs one lookup per process whatever the concurrency.
	meta, err := s.fetchThroughShared(ctx, key, title)
	request.finish(meta, err)

	s.mu.Lock()
	delete(s.inFlight, key)
	if err == nil {
		s.cache[key] = &cacheEntry{meta: meta, expiresAt: time.Now().Add(s.ttl)}
	}
	s.mu.Unlock()
	return meta, err
}

// fetchThroughShared answers from the cross-process cache when it can, and publishes what the
// providers returned into it.
func (s *Service) fetchThroughShared(ctx context.Context, key, title string) (*Metadata, error) {
	if meta, found := s.fetchShared(ctx, key); found {
		return meta, nil
	}
	meta, err := s.fetchUncached(ctx, title)
	if err != nil || meta == nil {
		return meta, err
	}
	s.storeShared(ctx, key, meta)
	return meta, err
}

// fetchShared reads the cross-process cache. An unreachable store is a miss, never a failed
// lookup: the providers are still there.
func (s *Service) fetchShared(ctx context.Context, key string) (*Metadata, bool) {
	if s.shared == nil {
		return nil, false
	}
	meta, found, err := cache.GetJSON[*Metadata](ctx, s.shared, cache.MetadataKey(key))
	if err != nil {
		s.logger.Warn("metadata: shared cache read failed", slog.String("error", err.Error()))
		return nil, false
	}
	return meta, found
}

func (s *Service) storeShared(ctx context.Context, key string, meta *Metadata) {
	if s.shared == nil {
		return
	}
	// A title nobody matched is not stored: providers can miss for a season and come back,
	// and a shared "no" would outlast the process that learned it.
	if err := cache.SetJSON(ctx, s.shared, cache.MetadataKey(key), meta, s.ttl); err != nil {
		s.logger.Warn("metadata: shared cache write failed", slog.String("error", err.Error()))
	}
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
	meta.AlternateTitles = dedupeTitles(alternateTitlesFromAniList(m))
	return meta
}

// alternateTitlesFromAniList collects the synonyms plus the romaji and native titles; the
// English title is already the primary Metadata.Title.
func alternateTitlesFromAniList(m *Media) []string {
	titles := make([]string, 0, len(m.Synonyms)+2)
	titles = append(titles, m.Synonyms...)
	if strings.TrimSpace(m.Title.Romaji) != "" {
		titles = append(titles, m.Title.Romaji)
	}
	if strings.TrimSpace(m.Title.Native) != "" {
		titles = append(titles, m.Title.Native)
	}
	return titles
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
	// The English alternative title is already the primary Metadata.Title candidate, so the
	// alternates are the synonyms plus the native (Japanese) title.
	alternates := make([]string, 0, len(a.AlternativeTitles.Synonyms)+1)
	alternates = append(alternates, a.AlternativeTitles.Synonyms...)
	if native := deref(a.AlternativeTitles.Japanese); strings.TrimSpace(native) != "" {
		alternates = append(alternates, native)
	}
	meta.AlternateTitles = dedupeTitles(alternates)
	return meta
}

// dedupeTitles keeps the first occurrence of each non-empty title, preserving provider order.
func dedupeTitles(titles []string) []string {
	seen := make(map[string]bool, len(titles))
	out := make([]string, 0, len(titles))
	for _, title := range titles {
		trimmed := strings.TrimSpace(title)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// StripHTML removes markup from a provider description: AniList sends HTML even with
// asHtml:false in some fields, and the library stores plain text that both clients render
// directly, so tags must not survive the import.
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
