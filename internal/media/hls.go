package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"animind-backend-go/internal/config"
)

// On-demand HLS sessions.
//
// A browser cannot play a Matroska file, so an MKV is remuxed into MPEG-TS segments by
// ffmpeg while the viewer watches. Video is always copied; audio is copied only when its
// codec is legal in MPEG-TS, otherwise it becomes AAC. Sessions are capped, evicted least
// recently used first, and expire when idle, because each one is an OS process.
//
// Native clients never reach this path: the desktop player demuxes the source itself.

// HLS session limits and identifiers.
var (
	sessionIDPattern    = regexp.MustCompile(`^[a-f0-9]{32}$`)
	segmentNamePattern  = regexp.MustCompile(`^seg\d{5}\.ts$`)
	playlistFileName    = "playlist.m3u8"
	segmentNamePatternF = "seg%05d.ts"

	// ErrUnknownSession reports a session id that no longer exists, which the client
	// treats as "create a new one".
	ErrUnknownSession = errors.New("media: hls session not found")
	// ErrSessionNotReady reports a playlist that has not produced a segment yet.
	ErrSessionNotReady = errors.New("media: hls session has no segments yet")
	// ErrTooManySessions is returned when eviction cannot free capacity.
	ErrTooManySessions = errors.New("media: too many concurrent HLS sessions")
	// ErrNotLocalMode reports an HLS request in S3 mode, where presigned URLs are used.
	ErrNotLocalMode = errors.New("media: HLS is only available in local storage mode")
)

// HLSSession is one running ffmpeg remux.
type HLSSession struct {
	ID              string    `json:"id"`
	EpisodeID       string    `json:"episodeId"`
	SourcePath      string    `json:"-"`
	AudioTrackIndex int       `json:"audioTrack"`
	PlaylistPath    string    `json:"-"`
	Dir             string    `json:"-"`
	StartedAt       time.Time `json:"startedAt"`
	LastAccessedAt  time.Time `json:"lastAccessedAt"`
	StartTime       float64   `json:"startTime"`
}

// HLSStats describes the manager for the admin screen.
type HLSStats struct {
	ActiveSessions int          `json:"activeSessions"`
	MaxSessions    int          `json:"maxSessions"`
	Sessions       []HLSSession `json:"sessions"`
}

// HLSManager owns every running session.
type HLSManager struct {
	cfg     *config.Config
	ffmpeg  *FFmpeg
	rootDir string
	logger  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*managedSession
	stopOnce sync.Once
	stopCh   chan struct{}
}

// managedSession pairs the public session with its process.
type managedSession struct {
	session *HLSSession
	cmd     *exec.Cmd
	destroy bool
	mu      sync.Mutex
}

// NewHLSManager builds the manager and starts its idle sweep.
func NewHLSManager(cfg *config.Config, ffmpeg *FFmpeg, logger *slog.Logger) (*HLSManager, error) {
	root := cfg.HLS.RootDir
	if root == "" {
		root = filepath.Join(os.TempDir(), "animind-hls")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("media: create HLS root %q: %w", root, err)
	}
	m := &HLSManager{
		cfg:      cfg,
		ffmpeg:   ffmpeg,
		rootDir:  root,
		logger:   logger,
		sessions: make(map[string]*managedSession),
		stopCh:   make(chan struct{}),
	}
	go m.idleSweep()
	return m, nil
}

// RootDir reports where playlists and segments are written.
func (m *HLSManager) RootDir() string { return m.rootDir }

// Stop ends the idle sweep and destroys every session. Shutdown calls this so no ffmpeg
// process survives the server.
func (m *HLSManager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.DestroyAll(ctx)
}

// Create starts a session and returns its id plus the playlist URL to fetch.
//
// It blocks until the first segment exists: handing a client a playlist URL that yields
// nothing would look like a broken player rather than a slow start.
func (m *HLSManager) Create(ctx context.Context, sourcePath, episodeID string, audioTrackIndex int, startTime float64) (string, string, error) {
	if m.cfg.Storage.Mode != config.StorageLocal {
		return "", "", ErrNotLocalMode
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", "", fmt.Errorf("media: HLS source missing: %w", err)
	}
	if info.IsDir() {
		return "", "", errors.New("media: HLS source is a directory")
	}

	id, err := newSessionID()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(m.rootDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("media: create session dir: %w", err)
	}

	if err := m.evictIfNeeded(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}

	playlist := filepath.Join(dir, playlistFileName)
	segmentPattern := filepath.Join(dir, segmentNamePatternF)
	args := m.buildArgs(ctx, sourcePath, audioTrackIndex, startTime, playlist, segmentPattern)

	cmd := exec.Command(m.ffmpeg.Binary(), args...)
	cmd.Stderr = newLogPipe(m.logger, id)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("media: start ffmpeg: %w", err)
	}

	now := time.Now()
	entry := &managedSession{
		session: &HLSSession{
			ID: id, EpisodeID: episodeID, SourcePath: sourcePath,
			AudioTrackIndex: audioTrackIndex, PlaylistPath: playlist, Dir: dir,
			StartedAt: now, LastAccessedAt: now, StartTime: startTime,
		},
		cmd: cmd,
	}

	m.mu.Lock()
	m.sessions[id] = entry
	m.mu.Unlock()

	if err := m.awaitFirstSegment(ctx, playlist); err != nil {
		m.Destroy(ctx, id)
		return "", "", err
	}
	return id, "/api/hls/" + id + "/playlist.m3u8", nil
}

// buildArgs assembles the ffmpeg command line.
//
// -hls_playlist_type event makes the playlist append-only so a viewer can scrub backwards
// to the start of an episode; -hls_flags temp_file prevents a client reading a half-written
// segment.
func (m *HLSManager) buildArgs(ctx context.Context, sourcePath string, audioTrackIndex int, startTime float64, playlist, segmentPattern string) []string {
	segmentSeconds := int(m.cfg.HLS.SegmentDuration.Seconds())
	if segmentSeconds < 2 {
		segmentSeconds = 2
	}
	if segmentSeconds > 30 {
		segmentSeconds = 30
	}

	videoCodec := m.ffmpeg.VideoStreamCodec(ctx, sourcePath)
	audioCodec := m.ffmpeg.AudioStreamCodec(ctx, sourcePath, audioTrackIndex)
	copyAudio := IsMpegTsCopySafeCodec(audioCodec)

	args := []string{"-v", "warning", "-threads", "0"}
	if startTime > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", startTime))
	}
	args = append(args, "-i", sourcePath, "-map", "0:v:0", "-map", fmt.Sprintf("0:%d", audioTrackIndex), "-c:v", "copy")
	if videoCodec == "hevc" || videoCodec == "h265" {
		// Without the hvc1 tag, QuickTime and some hardware decoders reject the stream.
		args = append(args, "-tag:v", "hvc1")
	}
	if copyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", segmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_segment_type", "mpegts",
		"-hls_flags", "append_list+temp_file",
		"-hls_segment_filename", segmentPattern,
		"-start_number", "0",
		playlist,
	)
	return args
}

// awaitFirstSegment waits until ffmpeg has published a usable playlist.
func (m *HLSManager) awaitFirstSegment(ctx context.Context, playlist string) error {
	deadline := time.Now().Add(m.cfg.HLS.ReadyTimeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(playlist)
		if err == nil {
			text := string(data)
			if strings.Contains(text, ".ts") && strings.Contains(text, "#EXTINF:") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("media: HLS session timed out waiting for the first segment (is ffmpeg installed and the source readable?)")
}

// evictIfNeeded frees a slot when the cap is reached, dropping the least recently used
// session so a new viewer is not locked out by an abandoned tab.
func (m *HLSManager) evictIfNeeded(ctx context.Context) error {
	m.mu.Lock()
	max := m.cfg.HLS.MaxConcurrentSessions
	if max <= 0 {
		max = 2
	}
	if len(m.sessions) < max {
		m.mu.Unlock()
		return nil
	}

	var oldest *managedSession
	for _, entry := range m.sessions {
		if entry.destroy {
			continue
		}
		if oldest == nil || entry.session.LastAccessedAt.Before(oldest.session.LastAccessedAt) {
			oldest = entry
		}
	}
	m.mu.Unlock()

	if oldest == nil {
		return ErrTooManySessions
	}
	m.logger.Info("evicting HLS session to make room",
		slog.String("session", shortID(oldest.session.ID)),
		slog.String("episode", oldest.session.EpisodeID))
	m.Destroy(ctx, oldest.session.ID)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= max {
		return ErrTooManySessions
	}
	return nil
}

// Playlist returns the rewritten playlist text for a session.
//
// ffmpeg writes absolute segment paths into the playlist. A client would resolve those
// against its own host, so each line is reduced to its file name and served from this
// server's segment route instead.
func (m *HLSManager) Playlist(ctx context.Context, id string) (string, error) {
	entry, err := m.touch(id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(entry.session.PlaylistPath)
	if err != nil {
		return "", ErrSessionNotReady
	}

	var out strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			out.WriteString("\n")
		case strings.HasPrefix(trimmed, "#"):
			out.WriteString(trimmed + "\n")
		case strings.HasSuffix(trimmed, ".ts"):
			out.WriteString(filepath.Base(trimmed) + "\n")
		default:
			out.WriteString(filepath.Base(trimmed) + "\n")
		}
	}
	return out.String(), nil
}

// SegmentPath validates a segment name and returns its on-disk path.
//
// The name is matched against a strict pattern rather than cleaned and joined: this route
// is reachable with only a session id, so a traversal attempt must fail closed.
func (m *HLSManager) SegmentPath(id, segmentName string) (string, error) {
	entry, err := m.touch(id)
	if err != nil {
		return "", err
	}
	if !segmentNamePattern.MatchString(segmentName) {
		return "", fmt.Errorf("media: invalid segment name %q", segmentName)
	}
	path := filepath.Join(entry.session.Dir, segmentName)
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return "", fmt.Errorf("media: segment %q is not available yet", segmentName)
	}
	return path, nil
}

// Seek restarts the session at a new start time.
//
// ffmpeg has no in-place seek for an event-type playlist, so the process is killed, its
// output removed, and a new one started from the requested offset.
func (m *HLSManager) Seek(ctx context.Context, id string, timeSeconds float64) error {
	m.mu.Lock()
	entry := m.sessions[id]
	m.mu.Unlock()
	if entry == nil {
		return ErrUnknownSession
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.destroy {
		return ErrUnknownSession
	}
	if entry.cmd != nil && entry.cmd.Process != nil {
		_ = entry.cmd.Process.Kill()
		_, _ = entry.cmd.Process.Wait()
	}
	if err := clearDir(entry.session.Dir); err != nil {
		return err
	}

	if timeSeconds < 0 {
		timeSeconds = 0
	}
	playlist := entry.session.PlaylistPath
	args := m.buildArgs(ctx, entry.session.SourcePath, entry.session.AudioTrackIndex, timeSeconds,
		playlist, filepath.Join(entry.session.Dir, segmentNamePatternF))

	cmd := exec.Command(m.ffmpeg.Binary(), args...)
	cmd.Stderr = newLogPipe(m.logger, id)
	if err := cmd.Start(); err != nil {
		m.destroyLocked(entry)
		return fmt.Errorf("media: restart ffmpeg after seek: %w", err)
	}
	entry.cmd = cmd
	entry.session.StartTime = timeSeconds
	entry.session.LastAccessedAt = time.Now()

	if err := m.awaitFirstSegment(ctx, playlist); err != nil {
		m.destroyLocked(entry)
		return err
	}
	return nil
}

// Destroy stops a session and removes its files.
func (m *HLSManager) Destroy(ctx context.Context, id string) {
	m.mu.Lock()
	entry := m.sessions[id]
	m.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	m.destroyLocked(entry)

	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// destroyLocked tears down the process and directory. Callers hold entry.mu.
func (m *HLSManager) destroyLocked(entry *managedSession) {
	if entry.destroy {
		return
	}
	entry.destroy = true
	if entry.cmd != nil && entry.cmd.Process != nil {
		_ = entry.cmd.Process.Kill()
		_, _ = entry.cmd.Process.Wait()
	}
	_ = os.RemoveAll(entry.session.Dir)
}

// DestroyAll shuts every session down; used on graceful exit.
func (m *HLSManager) DestroyAll(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.Destroy(ctx, id)
	}
	// Remove the root so a restart never finds orphaned segments.
	_ = os.RemoveAll(m.rootDir)
	_ = os.MkdirAll(m.rootDir, 0o755)
}

// Has reports whether a session id is live, which the playlist route answers with 404
// before doing any work.
func (m *HLSManager) Has(id string) bool {
	if !sessionIDPattern.MatchString(id) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.sessions[id]
	return entry != nil && !entry.destroy
}

// Stats snapshots the running sessions for the admin screen.
func (m *HLSManager) Stats() HLSStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := HLSStats{ActiveSessions: len(m.sessions), MaxSessions: m.cfg.HLS.MaxConcurrentSessions}
	now := time.Now()
	for _, entry := range m.sessions {
		if entry.destroy {
			continue
		}
		session := *entry.session
		session.LastAccessedAt = session.LastAccessedAt.UTC()
		stats.Sessions = append(stats.Sessions, session)
		_ = now
	}
	sort.Slice(stats.Sessions, func(i, j int) bool {
		return stats.Sessions[i].StartedAt.Before(stats.Sessions[j].StartedAt)
	})
	stats.ActiveSessions = len(stats.Sessions)
	return stats
}

// touch refreshes a session's idle clock and returns it.
func (m *HLSManager) touch(id string) (*managedSession, error) {
	if !sessionIDPattern.MatchString(id) {
		return nil, ErrUnknownSession
	}
	m.mu.Lock()
	entry := m.sessions[id]
	m.mu.Unlock()
	if entry == nil || entry.destroy {
		return nil, ErrUnknownSession
	}
	entry.mu.Lock()
	entry.session.LastAccessedAt = time.Now()
	entry.mu.Unlock()
	return entry, nil
}

// idleSweep destroys sessions that have not been touched within the idle timeout.
func (m *HLSManager) idleSweep() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}

		cutoff := time.Now().Add(-m.cfg.HLS.IdleTimeout)
		m.mu.Lock()
		expired := make([]string, 0, 4)
		for id, entry := range m.sessions {
			if entry.session.LastAccessedAt.Before(cutoff) {
				expired = append(expired, id)
			}
		}
		m.mu.Unlock()

		for _, id := range expired {
			m.logger.Info("expiring idle HLS session", slog.String("session", shortID(id)))
			m.Destroy(context.Background(), id)
		}
	}
}

// clearDir removes every file in a session directory, keeping the directory itself.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o755)
		}
		return err
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("media: generate session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// logPipe forwards ffmpeg's stderr into the structured logger, dropping the progress lines
// ffmpeg emits continuously.
type logPipe struct {
	logger *slog.Logger
	id     string
	buf    []byte
}

func newLogPipe(logger *slog.Logger, id string) io.Writer {
	return &logPipe{logger: logger, id: shortID(id)}
}

func (p *logPipe) Write(data []byte) (int, error) {
	p.buf = append(p.buf, data...)
	for {
		idx := strings.IndexByte(string(p.buf), '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(string(p.buf[:idx]))
		p.buf = p.buf[idx+1:]
		if line == "" || strings.Contains(line, "frame=") {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		p.logger.Warn("ffmpeg", slog.String("session", p.id), slog.String("line", line))
	}
	if len(p.buf) > 4096 {
		p.buf = p.buf[len(p.buf)-2048:]
	}
	return len(data), nil
}
