package media

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"animind-backend-go/internal/storage"
)

// Subtitle handling.
//
// Extracted subtitles live next to the video in a canonical tree:
//
//	<Show>/Subtitles/Episode 05/English.vtt
//	<Show>/Subtitles/Season 03/Episode 05/English.vtt   (season > 1)
//
// Writing them once at scan time means playback never pays for an ffmpeg run, and the
// layout is stable enough that a re-scan does not redo work. SRT sidecars written by hand
// beside the video are still read, and converted on the way out.

// SubtitleTrack is one usable subtitle for a client.
type SubtitleTrack struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Language string `json:"language"`
	// Content is a complete WebVTT document. The site renders cues itself rather than
	// using a track element, so the text must be inline in the response.
	Content string `json:"content"`
}

const (
	subtitlesDirName = "Subtitles"
	srtExtension     = ".srt"
	vttExtension     = ".vtt"
)

// EpisodeFolderName formats the per-episode directory, widening to three digits only past
// 99 so a 1000-episode series still sorts.
func EpisodeFolderName(number float64) string {
	whole := int(number)
	width := 2
	if whole >= 100 {
		width = 3
	}
	return fmt.Sprintf("Episode %0*d", width, whole)
}

// SeasonFolderName formats the per-season directory.
func SeasonFolderName(season int) string { return fmt.Sprintf("Season %02d", season) }

// SubtitleDirRelative returns the canonical subtitle directory for a video, relative to
// the library root.
func SubtitleDirRelative(videoRelPath string, season int, episode float64) string {
	root := strings.Split(filepath.ToSlash(videoRelPath), "/")[0]
	if root == "" {
		root = "Library"
	}
	parts := []string{root, subtitlesDirName}
	if season > 1 {
		parts = append(parts, SeasonFolderName(season))
	}
	parts = append(parts, EpisodeFolderName(episode))
	return filepath.FromSlash(strings.Join(parts, "/"))
}

// ExtractEmbeddedSubtitles converts a video's embedded subtitle streams into WebVTT files
// on disk. It is a no-op when the directory already holds subtitles, when the target is
// read-only, or when ffmpeg cannot read the file.
func (f *FFmpeg) ExtractEmbeddedSubtitles(ctx context.Context, store *storage.Service, videoRelPath string, episode float64, season int) error {
	if !store.IsLocal() {
		// Sidecar extraction needs a writable directory on the host.
		return nil
	}
	videoPath, err := store.LocalPath(videoRelPath)
	if err != nil {
		return err
	}
	targetDir, err := store.LocalPath(SubtitleDirRelative(videoRelPath, season, episode))
	if err != nil {
		return err
	}

	if existing, err := os.ReadDir(targetDir); err == nil {
		for _, entry := range existing {
			name := strings.ToLower(entry.Name())
			if strings.HasSuffix(name, vttExtension) || strings.HasSuffix(name, srtExtension) {
				return nil
			}
		}
	}

	if !dirIsWritable(targetDir) {
		slog.Debug("subtitle directory is not writable, skipping extraction", slog.String("dir", targetDir))
		return nil
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("media: create subtitle dir: %w", err)
	}

	streams, err := f.ProbeSubtitleStreams(ctx, videoPath)
	if err != nil {
		return err
	}

	usedNames := map[string]int{}
	for _, stream := range streams {
		if IsUnsupportedSubtitleCodec(stream.Codec) {
			continue
		}
		language := stream.Language
		if language == "" {
			language = "Unknown"
		}
		base := sanitizeName(language)
		if count := usedNames[base]; count > 0 {
			base = fmt.Sprintf("%s.%d", base, count+1)
		}
		usedNames[sanitizeName(language)]++

		outPath := filepath.Join(targetDir, base+vttExtension)
		if err := f.writeSubtitleVTT(ctx, videoPath, stream, outPath); err != nil {
			slog.Warn("subtitle extraction failed for one stream",
				slog.String("file", videoRelPath),
				slog.Int("stream", stream.Index),
				slog.String("codec", stream.Codec),
				slog.String("error", err.Error()))
			if strings.Contains(err.Error(), "permission denied") {
				break
			}
			continue
		}
	}
	return nil
}

// writeSubtitleVTT renders one stream to a WebVTT file.
//
// ASS and SSA go through the in-process converter: ffmpeg's own ASS-to-VTT path keeps
// sign and karaoke lines, which are the ones that clutter an episode. Everything else uses
// ffmpeg's webvtt muxer.
func (f *FFmpeg) writeSubtitleVTT(ctx context.Context, videoPath string, stream Stream, outPath string) error {
	switch strings.ToLower(stream.Codec) {
	case "ass", "ssa":
		stdout, _, err := f.RunFFmpeg(ctx, "-v", "error", "-i", videoPath, "-map", fmt.Sprintf("0:%d", stream.Index), "-f", "ass", "pipe:1")
		if err != nil {
			return err
		}
		vtt := ConvertAssToVTT(string(stdout))
		if strings.TrimSpace(vtt) == "" {
			return fmt.Errorf("no usable dialogue lines")
		}
		return storage.WriteFileAtomic(outPath, []byte(vtt))
	default:
		args := []string{"-v", "error", "-i", videoPath, "-map", fmt.Sprintf("0:%d", stream.Index)}
		if strings.ToLower(stream.Codec) != "webvtt" {
			args = append(args, "-c:s", "webvtt")
		}
		args = append(args, "-f", "webvtt", outPath)
		if _, _, err := f.RunFFmpeg(ctx, args...); err != nil {
			_ = os.Remove(outPath)
			return err
		}
		return nil
	}
}

// dirIsWritable probes a directory by creating and removing a file. Checking the mode bits
// would be wrong on the ACL-backed mounts these libraries usually live on.
func dirIsWritable(dir string) bool {
	probe := filepath.Join(dir, ".write-probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return false
	}
	return os.Remove(probe) == nil
}

var unsafeNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeName(name string) string {
	cleaned := unsafeNamePattern.ReplaceAllString(strings.TrimSpace(name), "_")
	if cleaned == "" {
		return "Unknown"
	}
	return cleaned
}

// SubtitleTracks collects every subtitle available for one episode: the canonical
// extracted directory, its legacy spellings, and any sidecar files beside the video.
func (f *FFmpeg) SubtitleTracks(ctx context.Context, store *storage.Service, videoRelPath string, season int, episode float64) ([]SubtitleTrack, error) {
	base := strings.Split(filepath.ToSlash(videoRelPath), "/")
	fileName := base[len(base)-1]
	dir := strings.Join(base[:len(base)-1], "/")

	var tracks []SubtitleTrack
	seen := map[string]bool{}
	appendTrack := func(t SubtitleTrack) {
		if t.Content == "" || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		tracks = append(tracks, t)
	}

	// Canonical directory, then the older unseasoned and unpadded spellings.
	candidates := []string{SubtitleDirRelative(videoRelPath, season, episode)}
	root := candidates[0]
	if season > 1 {
		candidates = append(candidates, filepath.FromSlash(fmt.Sprintf("%s/%s/%s",
			strings.Split(filepath.ToSlash(videoRelPath), "/")[0], subtitlesDirName, EpisodeFolderName(episode))))
	}
	whole := int(episode)
	candidates = append(candidates,
		filepath.ToSlash(filepath.Join(filepath.Dir(root), fmt.Sprintf("Season %d", season), fmt.Sprintf("Episode %d", whole))))
	candidates = append(candidates, filepath.ToSlash(filepath.Join(filepath.Dir(root), fmt.Sprintf("Episode %d", whole))))

	for _, candidateDir := range candidates {
		entries, err := f.listSubtitleFiles(ctx, store, candidateDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			track, err := f.readSubtitleTrack(ctx, store, candidateDir, entry)
			if err != nil {
				slog.Warn("could not read subtitle file",
					slog.String("path", candidateDir+"/"+entry), slog.String("error", err.Error()))
				continue
			}
			appendTrack(track)
		}
	}

	// Sidecar files that sit next to the video, e.g. "Episode 05.English.srt".
	sidecars, err := store.ListKeysUnder(ctx, dir+"/")
	if err == nil {
		stem := strings.ToLower(StripExtensionFromName(fileName))
		for _, key := range sidecars {
			name := filepath.Base(filepath.ToSlash(key))
			ext := strings.ToLower(filepath.Ext(name))
			if ext != vttExtension && ext != srtExtension {
				continue
			}
			if !strings.HasPrefix(strings.ToLower(name), stem) {
				continue
			}
			data, readErr := store.GetObject(ctx, key, "")
			if readErr != nil {
				continue
			}
			language := subtitleLanguageFromName(name)
			content := string(data)
			if ext == srtExtension {
				content = ConvertSRTToVTT(content)
			}
			appendTrack(SubtitleTrack{
				ID:       "sidecar-" + sanitizeName(name),
				Label:    language,
				Language: language,
				Content:  content,
			})
		}
	}

	return tracks, nil
}

func (f *FFmpeg) listSubtitleFiles(ctx context.Context, store *storage.Service, dirRelative string) ([]string, error) {
	if !store.IsLocal() {
		keys, err := store.ListKeysUnder(ctx, filepath.ToSlash(dirRelative)+"/")
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(keys))
		for _, key := range keys {
			out = append(out, filepath.Base(filepath.ToSlash(key)))
		}
		return out, nil
	}
	path, err := store.LocalPath(dirRelative)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (f *FFmpeg) readSubtitleTrack(ctx context.Context, store *storage.Service, dirRelative, name string) (SubtitleTrack, error) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext != vttExtension && ext != srtExtension {
		return SubtitleTrack{}, fmt.Errorf("not a subtitle file")
	}
	key := filepath.ToSlash(filepath.Join(dirRelative, name))
	data, err := store.GetObject(ctx, key, "")
	if err != nil {
		return SubtitleTrack{}, err
	}
	content := string(data)
	if ext == srtExtension {
		content = ConvertSRTToVTT(content)
	}
	language := subtitleLanguageFromName(name)
	return SubtitleTrack{
		ID:       "embedded-" + sanitizeName(strings.TrimSuffix(name, ext)),
		Label:    language,
		Language: language,
		Content:  content,
	}, nil
}

var subtitleLanguagePattern = regexp.MustCompile(`(?i)(english|japanese|spanish|eng|jpn|spa|rus|hindi|french|german|arabic|ara|chi|fre|ger|ita|por)`)

func subtitleLanguageFromName(name string) string {
	match := subtitleLanguagePattern.FindString(strings.ToLower(name))
	if match == "" {
		return "Unknown"
	}
	return NormalizeLanguage(match)
}

// StripExtensionFromName removes a trailing file extension.
func StripExtensionFromName(name string) string {
	if idx := strings.LastIndex(name, "."); idx > 0 {
		return name[:idx]
	}
	return name
}

// ConvertSRTToVTT turns an SRT document into WebVTT.
//
// The two formats differ only in the header, the comma-vs-dot separator in timestamps, and
// the presence of cue index lines.
func ConvertSRTToVTT(srt string) string {
	normalized := strings.ReplaceAll(srt, "\r\n", "\n")
	var out strings.Builder
	out.WriteString("WEBVTT\n\n")
	indexPattern := regexp.MustCompile(`(?m)^\s*\d+\s*\n`)
	body := indexPattern.ReplaceAllString(normalized, "")
	body = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}),(\d{3})`).ReplaceAllString(body, "$1.$2")
	out.WriteString(strings.TrimSpace(body))
	out.WriteString("\n")
	return out.String()
}

// assStyleBlocklist matches style names used for typesetting rather than dialogue: signs,
// karaoke, openings and credits. Cues in those styles are dropped.
var assStyleBlocklist = regexp.MustCompile(`(?i)sign|caption|title|song|op|ed|opening|ending|karaoke|kara|romaji|credit|note|info|typeset|on\s?screen|screen`)

var (
	assDialoguePrefix  = "Dialogue:"
	assCommentPrefix   = "Comment:"
	assFormatPrefix    = "Format:"
	assDrawingPattern  = regexp.MustCompile(`\{[^}]*\\p[1-9][^}]*\}`)
	assBracePattern    = regexp.MustCompile(`\{[^}]*\}`)
	assNewlinePattern  = regexp.MustCompile(`\\N|\\n`)
	assHardSpaceStr    = `\h`
	assTimePattern     = regexp.MustCompile(`(\d+):(\d{2}):(\d{2})\.(\d{2})`)
	junkOnlyPattern    = regexp.MustCompile(`(?i)^[\d\s.\-mlbsqnp]+$`)
	punctuationPattern = regexp.MustCompile(`^[\s\W]+$`)
)

// ConvertAssToVTT renders ASS/SSA dialogue into WebVTT.
//
// Only the [Events] section is read, using its own Format line to locate the fields, since
// column order is not fixed across release groups. Drawing commands, override blocks and
// blacklisted styles are dropped, and consecutive duplicate cues collapse.
func ConvertAssToVTT(ass string) string {
	lines := strings.Split(strings.ReplaceAll(ass, "\r\n", "\n"), "\n")

	var columns map[string]int
	var cues []vttCue
	previousText := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, assFormatPrefix):
			columns = parseAssFormat(strings.TrimSpace(strings.TrimPrefix(trimmed, assFormatPrefix)))
		case strings.HasPrefix(trimmed, assDialoguePrefix):
			if columns == nil {
				continue
			}
			fields := splitAssFields(strings.TrimSpace(strings.TrimPrefix(trimmed, assDialoguePrefix)))
			start, okStart := fieldAt(fields, columns, "start")
			end, okEnd := fieldAt(fields, columns, "end")
			text, okText := fieldAt(fields, columns, "text")
			style, _ := fieldAt(fields, columns, "style")
			if !okStart || !okEnd || !okText {
				continue
			}
			if assStyleBlocklist.MatchString(style) {
				continue
			}
			if assDrawingPattern.MatchString(text) {
				continue
			}
			rendered := stripAssTags(text)
			if rendered == "" || looksLikeJunk(rendered) {
				continue
			}
			if rendered == previousText && len(cues) > 0 {
				continue
			}
			previousText = rendered
			cues = append(cues, vttCue{
				Start: assTimeToVTT(start),
				End:   assTimeToVTT(end),
				Text:  rendered,
			})
		case strings.HasPrefix(trimmed, assCommentPrefix):
			continue
		}
	}

	var out strings.Builder
	out.WriteString("WEBVTT\n\n")
	for _, cue := range cues {
		fmt.Fprintf(&out, "%s --> %s\n%s\n\n", cue.Start, cue.End, cue.Text)
	}
	return out.String()
}

type vttCue struct {
	Start string
	End   string
	Text  string
}

// parseAssFormat maps ASS event field names to their column index.
func parseAssFormat(format string) map[string]int {
	columns := make(map[string]int, 12)
	for i, name := range strings.Split(format, ",") {
		key := strings.ToLower(strings.TrimSpace(name))
		switch key {
		case "start", "end", "style", "name", "text", "actor", "effect", "layer", "marginl", "marginr", "marginv":
			columns[key] = i
		}
	}
	return columns
}

// splitAssFields splits an event line, letting the final field keep its commas.
func splitAssFields(line string) []string {
	fields := strings.SplitN(line, ",", 11)
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	return fields
}

func fieldAt(fields []string, columns map[string]int, name string) (string, bool) {
	idx, ok := columns[name]
	if !ok || idx >= len(fields) {
		return "", false
	}
	return fields[idx], true
}

// stripAssTags removes override blocks and converts ASS line breaks.
func stripAssTags(text string) string {
	out := assBracePattern.ReplaceAllString(text, "")
	out = assNewlinePattern.ReplaceAllString(out, "\n")
	out = strings.ReplaceAll(out, assHardSpaceStr, "\u00A0")
	out = regexp.MustCompile(`\\\[.*?\]`).ReplaceAllString(out, "")
	out = regexp.MustCompile(`\\[a-zA-Z]\w*[^)]*\)?`).ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}

// looksLikeJunk rejects cues that are only timing noise or punctuation, which ASS karaoke
// and effect lines commonly produce.
func looksLikeJunk(text string) bool {
	if punctuationPattern.MatchString(text) {
		return true
	}
	return junkOnlyPattern.MatchString(text)
}

// assTimeToVTT converts "0:01:02.03" into "00:01:02.030".
func assTimeToVTT(timestamp string) string {
	match := assTimePattern.FindStringSubmatch(timestamp)
	if match == nil {
		return "00:00:00.000"
	}
	hours, _ := strconv.Atoi(match[1])
	minutes, _ := strconv.Atoi(match[2])
	seconds, _ := strconv.Atoi(match[3])
	centis, _ := strconv.Atoi(match[4])
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, centis*10)
}
