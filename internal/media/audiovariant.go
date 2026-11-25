package media

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/storage"
)

// Browser-safe audio variants.
//
// Browsers play only AAC or MP3 from MP4, so the first playback of an Opus/Vorbis track
// builds a cached remux reused later, keyed by source size and mtime so a replaced episode
// cannot serve a stale track.

// AudioTrack describes one audio stream and its playback requirements.
type AudioTrack struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Language string `json:"language"`
	// StreamIndex is the ffmpeg stream specifier the client sends back.
	StreamIndex      int    `json:"streamIndex"`
	Codec            string `json:"codec"`
	BrowserSupported bool   `json:"browserSupported"`
	Cached           bool   `json:"cached"`
	CacheMode        string `json:"cacheMode,omitempty"`
}

// AudioCache manages the variant cache.
type AudioCache struct {
	cfg       *config.Config
	ffmpeg    *FFmpeg
	cacheRoot string
}

// NewAudioCache builds the cache manager; S3 mode has no writable library directory.
func NewAudioCache(cfg *config.Config, ffmpeg *FFmpeg) *AudioCache {
	return &AudioCache{
		cfg:       cfg,
		ffmpeg:    ffmpeg,
		cacheRoot: filepath.Join(cfg.Storage.LocalPath, storage.AudioCacheDirName),
	}
}

// Available reports whether variants can be cached at all.
func (a *AudioCache) Available() bool { return !a.cacheDisabled() }

func (a *AudioCache) cacheDisabled() bool { return a.cfg.Storage.Mode != config.StorageLocal }

// EmbeddedAudioTracks lists the audio streams of a file with cache status.
func (a *AudioCache) EmbeddedAudioTracks(ctx context.Context, videoPath string, episodeID string) ([]AudioTrack, error) {
	streams, err := a.ffmpeg.ProbeAudioStreams(ctx, videoPath)
	if err != nil {
		return nil, err
	}
	metadata := a.readMetadata(episodeID)

	tracks := make([]AudioTrack, 0, len(streams))
	for _, stream := range streams {
		browserSafe := IsBrowserSafeAudioCodec(stream.Codec)
		label := fmt.Sprintf("#%d (%s", stream.Index, strings.ToUpper(stream.Codec))
		if stream.Channels > 0 {
			label += fmt.Sprintf(" / %dg", stream.Channels)
		}
		label += ")"
		if stream.Language != "" && stream.Language != "Unknown" {
			label = fmt.Sprintf("%s %s", stream.Language, label)
		}

		track := AudioTrack{
			ID:               fmt.Sprintf("audio-%d", stream.Index),
			Label:            label,
			Language:         stream.Language,
			StreamIndex:      stream.Index,
			Codec:            stream.Codec,
			BrowserSupported: browserSafe,
		}
		if entry, ok := metadata[stream.Index]; ok {
			if _, statErr := os.Stat(entry.VariantPath); statErr == nil {
				track.Cached = true
				track.CacheMode = entry.Mode
			}
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

// trackMetadata records one cached variant.
type trackMetadata struct {
	StreamIndex     int    `json:"streamIndex"`
	Codec           string `json:"codec"`
	BrowserSafe     bool   `json:"browserSafe"`
	Mode            string `json:"mode"`
	SourceSignature string `json:"sourceSignature"`
	VariantPath     string `json:"variantPath"`
	UpdatedAt       string `json:"updatedAt"`
	Size            int64  `json:"size"`
}

type cacheMetadata struct {
	EpisodeID string          `json:"episodeId"`
	Tracks    []trackMetadata `json:"tracks"`
}

func (a *AudioCache) episodeDir(episodeID string) string {
	return filepath.Join(a.cacheRoot, episodeID)
}

// sourceSignature binds a cached variant to the exact source file version.
func sourceSignature(path string, trackIndex int, mode string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d:%d:%s", path, info.Size(), info.ModTime().Unix(), trackIndex, mode), nil
}

func variantHash(signature string) string {
	sum := sha1.Sum([]byte(signature))
	return hex.EncodeToString(sum[:])[:12]
}

// GetOrCreateVariant produces or reuses a browser-playable file for one audio track; a
// track a browser can already play returns the source unchanged.
func (a *AudioCache) GetOrCreateVariant(ctx context.Context, videoPath, episodeID string, trackIndex int, browserSafe bool) (string, error) {
	if a.cacheDisabled() {
		return videoPath, nil
	}
	signature, err := sourceSignature(videoPath, trackIndex, variantMode(browserSafe))
	if err != nil {
		return "", err
	}
	dir := a.episodeDir(episodeID)
	name := fmt.Sprintf("a%d-%s-%s.mp4", trackIndex, variantMode(browserSafe), variantHash(signature))
	target := filepath.Join(dir, name)

	if info, statErr := os.Stat(target); statErr == nil && info.Size() > 0 {
		a.recordTrack(episodeID, trackMetadata{
			StreamIndex: trackIndex, Mode: variantMode(browserSafe), SourceSignature: signature,
			VariantPath: target, UpdatedAt: time.Now().UTC().Format(time.RFC3339), Size: info.Size(),
			BrowserSafe: browserSafe,
		})
		return target, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("media: create audio cache dir: %w", err)
	}

	// A copy is tried first: most modern releases carry AAC already, and copying is instant.
	if browserSafe {
		if err := a.runVariantCopy(ctx, videoPath, trackIndex, target); err == nil {
			a.recordTrack(episodeID, trackMetadata{
				StreamIndex: trackIndex, Mode: "copy", SourceSignature: signature,
				VariantPath: target, UpdatedAt: time.Now().UTC().Format(time.RFC3339), BrowserSafe: true,
			})
			return target, nil
		}
	}

	tempTarget := target + ".partial"
	// -f mp4 is required: the .partial name has no recognised container extension.
	args := []string{"-y", "-v", "error", "-i", videoPath,
		"-map", "0:v:0", "-map", fmt.Sprintf("0:%d", trackIndex),
		"-c:v", "copy", "-c:a", "aac", "-b:a", "320k", "-movflags", "+faststart",
		"-f", "mp4", tempTarget}
	if _, stderr, err := a.ffmpeg.RunFFmpeg(ctx, args...); err != nil {
		_ = os.Remove(tempTarget)
		return "", fmt.Errorf("media: audio variant transcode: %s", strings.TrimSpace(stderr))
	}
	if err := os.Rename(tempTarget, target); err != nil {
		return "", fmt.Errorf("media: publish audio variant: %w", err)
	}

	a.recordTrack(episodeID, trackMetadata{
		StreamIndex: trackIndex, Mode: "transcode", SourceSignature: signature,
		VariantPath: target, UpdatedAt: time.Now().UTC().Format(time.RFC3339), BrowserSafe: false,
	})
	return target, nil
}

func variantMode(browserSafe bool) string {
	if browserSafe {
		return "copy"
	}
	return "transcode"
}

func (a *AudioCache) runVariantCopy(ctx context.Context, videoPath string, trackIndex int, target string) error {
	tempTarget := target + ".partial"
	args := []string{"-y", "-v", "error", "-i", videoPath,
		"-map", "0:v:0", "-map", fmt.Sprintf("0:%d", trackIndex),
		"-c:v", "copy", "-c:a", "copy", "-movflags", "+faststart",
		"-f", "mp4", tempTarget}
	if _, _, err := a.ffmpeg.RunFFmpeg(ctx, args...); err != nil {
		_ = os.Remove(tempTarget)
		return err
	}
	return os.Rename(tempTarget, target)
}

// readMetadata loads an episode's cache index; a corrupt file is treated as empty.
func (a *AudioCache) readMetadata(episodeID string) map[int]trackMetadata {
	out := map[int]trackMetadata{}
	raw, err := os.ReadFile(filepath.Join(a.episodeDir(episodeID), "metadata.json"))
	if err != nil {
		return out
	}
	var metadata cacheMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return out
	}
	for _, track := range metadata.Tracks {
		out[track.StreamIndex] = track
	}
	return out
}

func (a *AudioCache) recordTrack(episodeID string, entry trackMetadata) {
	dir := a.episodeDir(episodeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	existing := a.readMetadata(episodeID)
	existing[entry.StreamIndex] = entry

	metadata := cacheMetadata{EpisodeID: episodeID}
	for _, track := range existing {
		metadata.Tracks = append(metadata.Tracks, track)
	}
	sort.Slice(metadata.Tracks, func(i, j int) bool { return metadata.Tracks[i].StreamIndex < metadata.Tracks[j].StreamIndex })

	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0o644)
}

// PrewarmEpisode builds variants for tracks a browser could not play, Japanese audio first.
func (a *AudioCache) PrewarmEpisode(ctx context.Context, videoPath, episodeID string) int {
	if a.cacheDisabled() || !a.cfg.AudioCache.PrewarmEnabled {
		return 0
	}
	streams, err := a.ffmpeg.ProbeAudioStreams(ctx, videoPath)
	if err != nil {
		slog.Debug("prewarm skipped: could not probe audio", slog.String("error", err.Error()))
		return 0
	}

	type candidate struct {
		index         int
		japaneseAudio bool
	}
	var candidates []candidate
	for _, stream := range streams {
		if IsBrowserSafeAudioCodec(stream.Codec) {
			continue
		}
		candidates = append(candidates, candidate{index: stream.Index, japaneseAudio: stream.Language == "Japanese"})
	}
	// Japanese first, then everything else, keeping ffmpeg work proportional to what
	// viewers actually select.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].japaneseAudio && !candidates[j].japaneseAudio
	})
	if limit := a.cfg.AudioCache.PrewarmMaxTracksPerEpisode; limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	built := 0
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return built
		}
		if _, err := a.GetOrCreateVariant(ctx, videoPath, episodeID, c.index, false); err != nil {
			slog.Debug("prewarm could not build variant",
				slog.String("episode", episodeID), slog.Int("stream", c.index), slog.String("error", err.Error()))
			continue
		}
		built++
	}
	return built
}

// PrewarmLibrary processes episodes a scan touched, honouring configured limits.
func (a *AudioCache) PrewarmLibrary(ctx context.Context, store *storage.Service, episodes []PrewarmTarget) int {
	if a.cacheDisabled() || !a.cfg.AudioCache.PrewarmEnabled || len(episodes) == 0 {
		return 0
	}
	seen := map[string]bool{}
	limit := a.cfg.AudioCache.PrewarmEpisodeLimit
	processed, built := 0, 0

	for _, episode := range episodes {
		if limit > 0 && processed >= limit {
			break
		}
		if seen[episode.ID] {
			continue
		}
		seen[episode.ID] = true
		processed++

		path, err := store.LocalPath(episode.FilePath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		built += a.PrewarmEpisode(ctx, path, episode.ID)
	}
	return built
}

// PrewarmTarget is one episode selected for prewarm.
type PrewarmTarget struct {
	ID       string
	FilePath string
}

// CacheCleanupResult summarises a TTL sweep.
type CacheCleanupResult struct {
	DeletedFiles  int
	DeletedDirs   int
	PrunedEntries int
}

// CleanupByTTL deletes unused variants, prunes the index so it never points at a removed
// file, and drops empty episode directories.
func (a *AudioCache) CleanupByTTL(ttl time.Duration) (CacheCleanupResult, error) {
	var result CacheCleanupResult
	if a.cacheDisabled() {
		return result, nil
	}
	info, err := os.Stat(a.cacheRoot)
	if err != nil || !info.IsDir() {
		return result, nil
	}
	if ttl <= 0 {
		ttl = time.Duration(14*24) * time.Hour
	}

	dirs, err := os.ReadDir(a.cacheRoot)
	if err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-ttl)

	for _, entry := range dirs {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(a.cacheRoot, entry.Name())
		remaining := 0

		files, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, file := range files {
			if file.Name() == "metadata.json" || file.IsDir() {
				continue
			}
			path := filepath.Join(dir, file.Name())
			info, statErr := os.Stat(path)
			if statErr != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if removeErr := os.Remove(path); removeErr == nil {
					result.DeletedFiles++
					continue
				}
			}
			remaining++
		}

		// Rewrite the index so it cannot reference a file that no longer exists.
		metadataPath := filepath.Join(dir, "metadata.json")
		if raw, readErr := os.ReadFile(metadataPath); readErr == nil {
			var metadata cacheMetadata
			if json.Unmarshal(raw, &metadata) == nil {
				kept := metadata.Tracks[:0]
				for _, track := range metadata.Tracks {
					info, statErr := os.Stat(track.VariantPath)
					if statErr != nil || info.ModTime().Before(cutoff) {
						result.PrunedEntries++
						continue
					}
					kept = append(kept, track)
				}
				if len(kept) != len(metadata.Tracks) {
					metadata.Tracks = kept
					if updated, marshalErr := json.MarshalIndent(metadata, "", "  "); marshalErr == nil {
						_ = os.WriteFile(metadataPath, updated, 0o644)
					}
				}
			}
		}

		if remaining == 0 {
			if err := os.RemoveAll(dir); err == nil {
				result.DeletedDirs++
			}
		}
	}
	return result, nil
}
