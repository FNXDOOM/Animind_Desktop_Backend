package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/media"
	"animind-backend-go/internal/metadata"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/storage"
)

// Service indexes the library. A scan must be safe to trigger at any time — cron, the admin
// button, a storage webhook, and on startup in some deployments — since every write is an
// upsert and the prune step derives from the current listing; Run still allows only one.
type Service struct {
	cfg     *config.Config
	repo    *repository.Store
	storage *storage.Service
	meta    *metadata.Service
	llm     *OpenRouterClient
	ffmpeg  *media.FFmpeg
	logger  *slog.Logger

	mu       sync.Mutex
	running  bool
	warnOnce map[string]bool
	// processed collects episodes the current scan touched, for audio prewarm.
	processed []media.PrewarmTarget

	// lastStartedAt, lastFinishedAt, lastResult and lastError back the admin console. They
	// are written under mu and describe the most recent Run in this process; a restart
	// clears them, so the panel always says "no scan yet" rather than showing stale state.
	lastStartedAt  time.Time
	lastFinishedAt time.Time
	lastResult     *Result
	lastError      string
}

// New builds the scanner.
func New(cfg *config.Config, repo *repository.Store, store *storage.Service, meta *metadata.Service, llm *OpenRouterClient, ffmpeg *media.FFmpeg, logger *slog.Logger) *Service {
	return &Service{
		cfg: cfg, repo: repo, storage: store, meta: meta, llm: llm,
		ffmpeg: ffmpeg, logger: logger, warnOnce: map[string]bool{},
	}
}

// Result reports what a scan did.
type Result struct {
	Scanned   int                   `json:"scanned"`
	Inserted  int                   `json:"inserted"`
	Updated   int                   `json:"updated"`
	Removed   int                   `json:"removed"`
	Errors    []string              `json:"errors"`
	Duration  int64                 `json:"durationMs"`
	Processed []media.PrewarmTarget `json:"-"`

	PrunedEpisodes int `json:"prunedEpisodes"`
	PrunedShows    int `json:"prunedShows"`
	SubtitleFiles  int `json:"subtitleFiles"`
}

// ErrAlreadyRunning reports a scan attempted while another is in progress. Callers answer
// 409: two concurrent scans would double the metadata traffic for no benefit.
var ErrAlreadyRunning = errors.New("scanner: a library scan is already running")

// Status is the admin console's view of the scanner. LastResult is nil until a scan has
// completed in this process.
type Status struct {
	Running    bool
	StartedAt  time.Time
	FinishedAt time.Time
	LastResult *Result
	LastError  string
}

// Status snapshots the scanner state.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{
		Running: s.running, StartedAt: s.lastStartedAt,
		FinishedAt: s.lastFinishedAt, LastError: s.lastError,
	}
	if s.lastResult != nil {
		copied := *s.lastResult
		status.LastResult = &copied
	}
	return status
}

// Run performs a full scan of the configured library.
func (s *Service) Run(ctx context.Context) (result *Result, err error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	s.running = true
	s.lastStartedAt = time.Now()
	s.lastError = ""
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastFinishedAt = time.Now()
		if err != nil {
			s.lastError = err.Error()
		} else {
			s.lastResult = result
		}
		s.mu.Unlock()
	}()

	start := time.Now()
	result = &Result{Errors: []string{}}

	files, err := s.storage.ListVideoFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("scanner: list library files: %w", err)
	}
	result.Scanned = len(files)

	found := make(map[string]bool, len(files))
	for _, path := range files {
		found[path] = true
	}

	// Normalize the title spelling once per key so files whose names differ only in
	// punctuation are indexed under the same title. The show row itself is resolved per
	// file; the metadata service caches upstream lookups.
	byTitle := map[string]string{}
	for _, path := range files {
		parsed, err := s.parseFile(ctx, path, files)
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		if parsed == nil {
			continue
		}
		key := metadata.NormalizeTitleKey(parsed.Title)
		if key == "" {
			result.Errors = append(result.Errors, fmt.Sprintf("missing show title for %q", path))
			continue
		}
		title, known := byTitle[key]
		if !known {
			title = strings.TrimSpace(parsed.Title)
			byTitle[key] = title
		}

		if err := s.indexFile(ctx, title, path, parsed); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.Inserted++
	}

	if s.cfg.Scanner.PruneDeletedFiles {
		prunedEpisodes, prunedShows, err := s.prune(ctx, found)
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
		}
		result.PrunedEpisodes = prunedEpisodes
		result.PrunedShows = prunedShows
		result.Removed = prunedEpisodes + prunedShows
	}

	result.Duration = time.Since(start).Milliseconds()
	s.logger.Info("library scan finished",
		slog.Int("scanned", result.Scanned),
		slog.Int("indexed", result.Inserted),
		slog.Int("removed", result.Removed),
		slog.Int("errors", len(result.Errors)),
		slog.Int64("duration_ms", result.Duration))
	return result, nil
}

// parseFile turns a library path into a show and episode, using the LLM fallback only when
// the deterministic parser produced nothing and the operator enabled it.
func (s *Service) parseFile(ctx context.Context, path string, allPaths []string) (*Parsed, error) {
	if parsed := ParseFolderPath(path); parsed != nil && parsed.Title != "" {
		return parsed, nil
	}
	if !s.cfg.Scanner.OpenRouterFallback {
		return nil, nil
	}

	guess := s.llm.Infer(ctx, Input{
		RelativePath:     filepath.ToSlash(path),
		SiblingFileNames: siblingFileNames(path, allPaths),
	})
	if guess == nil {
		return nil, nil
	}
	if guess.Confidence < s.cfg.Metadata.OpenRouter.MinConfidence {
		s.warnOnceLog("low-confidence",
			fmt.Sprintf("Low-confidence scanner guess (%.2f) for %q; skipped", guess.Confidence, path))
		return nil, nil
	}
	if guess.Title == nil || guess.Episode == nil {
		return nil, nil
	}
	return &Parsed{
		Title:       *guess.Title,
		Episode:     *guess.Episode,
		Season:      guess.Season,
		ContentType: guess.ContentType,
		Group:       "scanner-llm",
	}, nil
}

// indexFile resolves the show, writes the episode row, and extracts subtitles.
func (s *Service) indexFile(ctx context.Context, title, path string, parsed *Parsed) error {
	showID, err := s.resolveShow(ctx, title)
	if err != nil {
		return err
	}

	contentType := parsed.ContentType
	if contentType == "" {
		contentType = ContentTypeTV
	}
	season := SeasonForContentType(parsed.Season, contentType)
	bucket := s.storage.Bucket()

	episode := repository.Episode{
		ShowID:        showID,
		SeasonNumber:  season,
		ContentType:   string(contentType),
		EpisodeNumber: &parsed.Episode,
		FilePath:      path,
		BucketName:    bucket,
	}
	if parsed.Group != "" {
		source := parsed.Group
		episode.ScannerSource = &source
	}
	episode.OriginalTitle = stringPointer(filepath.Base(filepath.ToSlash(path)))

	created, err := s.repo.UpsertEpisode(ctx, episode)
	if err != nil {
		return fmt.Errorf("scanner: index %q: %w", path, err)
	}
	s.ProcessedTarget(created.ID, path)

	// Subtitle extraction runs inline and sequentially: it is CPU-bound, and a parallel
	// burst of ffmpeg processes would make playback stutter for whoever is watching.
	if s.storage.IsLocal() {
		if err := s.ffmpeg.ExtractEmbeddedSubtitles(ctx, s.storage, path, parsed.Episode, season); err != nil {
			s.logger.Debug("subtitle extraction skipped",
				slog.String("file", path), slog.String("error", err.Error()))
		}
	}
	return nil
}

// ProcessedTarget records an episode the current scan touched for audio prewarm.
func (s *Service) ProcessedTarget(id, path string) {
	s.mu.Lock()
	s.processed = append(s.processed, media.PrewarmTarget{ID: id, FilePath: path})
	s.mu.Unlock()
}

// PrewarmTargets returns the episodes the last scan touched and clears the list.
func (s *Service) PrewarmTargets() []media.PrewarmTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := s.processed
	s.processed = nil
	return targets
}

// resolveShow finds the catalog row for a title or creates it, enriched with metadata.
// Matching order: exact title, external id, provider canonical title, then — only when no
// provider answered — a scored fuzzy match, which keeps "Re:Zero" and "Re Zero" on one show.
func (s *Service) resolveShow(ctx context.Context, title string) (string, error) {
	trimmed := strings.TrimSpace(title)

	if existing, err := s.repo.ShowByTitle(ctx, trimmed); err == nil {
		s.refreshMetadataIfIncomplete(ctx, existing, trimmed)
		return existing.ID, nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return "", err
	}

	meta, err := s.meta.Fetch(ctx, trimmed)
	if err != nil {
		s.logger.Debug("metadata lookup failed", slog.String("title", trimmed), slog.String("error", err.Error()))
	}

	if meta != nil {
		if meta.AnilistID != nil {
			if byAnilist, err := s.repo.ShowByAnilistID(ctx, *meta.AnilistID); err == nil {
				s.refreshMetadataIfIncomplete(ctx, byAnilist, trimmed)
				return byAnilist.ID, nil
			} else if !errors.Is(err, repository.ErrNotFound) {
				return "", err
			}
		}
		if !strings.EqualFold(meta.Title, trimmed) {
			if canonical, err := s.repo.ShowByTitle(ctx, meta.Title); err == nil {
				s.refreshMetadataIfIncomplete(ctx, canonical, trimmed)
				return canonical.ID, nil
			} else if !errors.Is(err, repository.ErrNotFound) {
				return "", err
			}
		}
	} else {
		// With no provider data, the only way to avoid a duplicate is to score what the
		// library already holds.
		candidates, err := s.repo.ShowCandidates(ctx, trimmed, 8)
		if err != nil {
			return "", err
		}
		if len(candidates) > 0 {
			best := candidates[0]
			bestScore := -1
			for _, candidate := range candidates {
				if score := scoreShowCandidate(candidate, trimmed); score > bestScore {
					bestScore, best = score, candidate
				}
			}
			// A weak score means the folder really is a new show.
			if bestScore >= 9 {
				s.refreshMetadataIfIncomplete(ctx, &best, trimmed)
				return best.ID, nil
			}
		}
	}

	show := showFromMetadata(meta, trimmed)
	created, err := s.repo.CreateShow(ctx, show)
	if err != nil {
		if !repository.IsUniqueViolation(err) {
			return "", fmt.Errorf("scanner: create show %q: %w", trimmed, err)
		}
		// Another writer inserted the same title concurrently; adopt their row.
		existing, lookupErr := s.repo.ShowByTitle(ctx, show.Title)
		if lookupErr != nil {
			return "", fmt.Errorf("scanner: create show %q raced and re-read failed: %w", trimmed, lookupErr)
		}
		return existing.ID, nil
	}
	return created.ID, nil
}

// showFromMetadata builds the row to insert, falling back to the folder title when no
// provider answered.
func showFromMetadata(meta *metadata.Metadata, fallback string) repository.Show {
	title := fallback
	if meta != nil && meta.Title != "" {
		title = meta.Title
	}
	show := repository.Show{Title: title, Genres: []string{}}
	if meta == nil {
		return show
	}
	show.Synopsis = meta.Synopsis
	show.CoverImageURL = meta.CoverImageURL
	show.Genres = meta.Genres
	show.Rating = meta.Rating
	show.EpisodeCount = meta.EpisodeCount
	show.Studio = meta.Studio
	show.Status = meta.Status
	show.Year = meta.Year
	show.AnilistID = meta.AnilistID
	if meta.Trailer != nil {
		show.TrailerID = &meta.Trailer.ID
		site := meta.Trailer.Site
		show.TrailerSite = &site
		show.TrailerThumbnail = meta.Trailer.Thumbnail
	}
	return show
}

// refreshMetadataIfIncomplete fills a show's missing fields from a provider.
func (s *Service) refreshMetadataIfIncomplete(ctx context.Context, show *repository.Show, searchTerm string) {
	if !showIsIncomplete(show) {
		return
	}
	meta, err := s.meta.Fetch(ctx, firstNonEmptyString(show.Title, searchTerm))
	if err != nil || meta == nil {
		return
	}
	patch := showFromMetadata(meta, show.Title)
	if err := s.repo.FillShowMetadata(ctx, show.ID, patch); err != nil {
		s.logger.Warn("could not refresh show metadata", slog.String("show", show.ID), slog.String("error", err.Error()))
		return
	}
	s.logger.Info("refreshed missing show metadata",
		slog.String("title", show.Title), slog.String("source", meta.Source))
}

// showIsIncomplete reports whether any field a card or detail page displays is empty.
func showIsIncomplete(show *repository.Show) bool {
	return isEmptyString(show.CoverImageURL) || isEmptyString(show.Synopsis) || len(show.Genres) == 0 ||
		show.Rating == nil || show.EpisodeCount == nil || isEmptyString(show.Studio) ||
		isEmptyString(show.Status) || isEmptyString(show.Year) || show.AnilistID == nil
}

// scoreShowCandidate ranks a fuzzy title match: richer rows and closer names win.
func scoreShowCandidate(candidate repository.Show, searchTerm string) int {
	score := 0
	if candidate.AnilistID != nil {
		score += 8
	}
	if !isEmptyString(candidate.CoverImageURL) {
		score += 3
	}
	if !isEmptyString(candidate.Synopsis) {
		score += 2
	}
	normalizedCandidate := metadata.NormalizeTitleKey(candidate.Title)
	normalizedSearch := metadata.NormalizeTitleKey(searchTerm)
	switch {
	case normalizedCandidate == normalizedSearch:
		score += 6
	case strings.Contains(normalizedCandidate, normalizedSearch) || strings.Contains(normalizedSearch, normalizedCandidate):
		score += 3
	}
	return score
}

// prune removes episodes whose file is gone and shows left without episodes.
func (s *Service) prune(ctx context.Context, found map[string]bool) (episodes, shows int, err error) {
	rows, err := s.repo.ListEpisodePaths(ctx)
	if err != nil {
		return 0, 0, err
	}

	var stale []string
	for _, row := range rows {
		if !found[row.FilePath] {
			stale = append(stale, row.ID)
		}
	}
	if len(stale) > 0 {
		deleted, err := s.repo.DeleteEpisodesByID(ctx, stale)
		if err != nil {
			return 0, 0, err
		}
		episodes = int(deleted)
		s.logger.Info("pruned episodes whose file is missing", slog.Int("count", episodes))
	}

	active, err := s.repo.ShowIDsWithEpisodes(ctx)
	if err != nil {
		return episodes, 0, err
	}
	all, err := s.repo.ListShowIDs(ctx)
	if err != nil {
		return episodes, 0, err
	}
	var orphans []string
	for _, id := range all {
		if !active[id] {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 {
		deleted, err := s.repo.DeleteShowsByID(ctx, orphans)
		if err != nil {
			s.logger.Warn("could not prune orphan shows", slog.String("error", err.Error()))
			return episodes, 0, nil
		}
		shows = int(deleted)
		s.logger.Info("pruned shows with no episodes", slog.Int("count", shows))
	}
	return episodes, shows, nil
}

// siblingFileNames lists the base names sharing a file's directory, which gives a parser
// the episode range of a season.
func siblingFileNames(path string, all []string) []string {
	dir := filepath.Dir(filepath.ToSlash(path))
	out := make([]string, 0, 16)
	for _, candidate := range all {
		if filepath.Dir(filepath.ToSlash(candidate)) == dir {
			out = append(out, filepath.Base(filepath.ToSlash(candidate)))
		}
	}
	sort.Strings(out)
	return out
}

// warnOnceLog records a message the first time it occurs so a systemic problem is visible
// without repeating per file.
func (s *Service) warnOnceLog(key, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnOnce[key] {
		return
	}
	s.warnOnce[key] = true
	s.logger.Warn(message)
}

func stringPointer(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func isEmptyString(v *string) bool { return v == nil || strings.TrimSpace(*v) == "" }

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
