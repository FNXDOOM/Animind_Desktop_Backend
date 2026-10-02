// Package media wraps the ffmpeg toolchain: stream probing, subtitle extraction,
// browser-safe audio variants, and on-demand HLS sessions.
//
// Every function here runs an external process, so each one takes a context with a
// timeout and treats a tool failure as a degraded result rather than a fatal error. A
// library scan must still record episodes when ffmpeg is missing.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"animind-backend-go/internal/config"
)

// FFmpeg runs and probes media with the system toolchain.
type FFmpeg struct {
	cfg           *config.Config
	mu            sync.Mutex
	missingLogged bool
}

// NewFFmpeg builds the runner.
func NewFFmpeg(cfg *config.Config) *FFmpeg { return &FFmpeg{cfg: cfg} }

// Stream is one track reported by ffprobe.
type Stream struct {
	Index    int    `json:"index"`
	Type     string `json:"codec_type"`
	Codec    string `json:"codec_name"`
	Language string `json:"language"`
	Title    string `json:"title"`
	Channels int    `json:"channels"`
	Default  bool   `json:"default"`
	Forced   bool   `json:"forced"`
	Width    int    `json:"-"`
	Height   int    `json:"-"`
	Tags     map[string]string
}

// probeOutput models the subset of ffprobe's JSON we read.
//
// The field types matter more than they look: ffprobe reports `disposition` flags and
// `bit_rate` as numbers, not booleans or strings, so a mismatched type fails the whole
// decode and every probe comes back empty. TestProbeDecodesRealFFprobeJSON pins this.
type probeOutput struct {
	Streams []struct {
		Index       int               `json:"index"`
		CodecType   string            `json:"codec_type"`
		CodecName   string            `json:"codec_name"`
		Channels    *int              `json:"channels"`
		Tags        map[string]string `json:"tags"`
		Width       *int              `json:"width"`
		Height      *int              `json:"height"`
		Disposition map[string]int    `json:"disposition"`
	} `json:"streams"`
}

// ProbeStreams returns every stream in a file.
func (f *FFmpeg) ProbeStreams(ctx context.Context, path string) ([]Stream, error) {
	return f.probe(ctx, path, "-show_streams")
}

// ProbeSubtitleStreams returns only subtitle streams.
func (f *FFmpeg) ProbeSubtitleStreams(ctx context.Context, path string) ([]Stream, error) {
	return f.probe(ctx, path, "-show_streams", "-select_streams", "s")
}

// ProbeAudioStreams returns only audio streams.
func (f *FFmpeg) ProbeAudioStreams(ctx context.Context, path string) ([]Stream, error) {
	return f.probe(ctx, path, "-show_streams", "-select_streams", "a")
}

// AudioStreamCodec reports the codec name of one audio stream, or "" when unknown.
func (f *FFmpeg) AudioStreamCodec(ctx context.Context, path string, index int) string {
	streams, err := f.probe(ctx, path, "-show_streams", "-select_streams", "a")
	if err != nil {
		return ""
	}
	for _, s := range streams {
		if s.Index == index {
			return strings.ToLower(s.Codec)
		}
	}
	return ""
}

// VideoStreamCodec reports the codec of the first video stream.
func (f *FFmpeg) VideoStreamCodec(ctx context.Context, path string) string {
	streams, err := f.probe(ctx, path, "-show_streams", "-select_streams", "v:0")
	if err != nil || len(streams) == 0 {
		return ""
	}
	return strings.ToLower(streams[0].Codec)
}

func (f *FFmpeg) probe(ctx context.Context, path string, flags ...string) ([]Stream, error) {
	args := append([]string{"-v", "error", "-print_format", "json"}, flags...)
	args = append(args, path)

	stdout, stderr, err := f.run(ctx, f.cfg.FFmpeg.FFprobePath, args...)
	if err != nil {
		return nil, f.toolError("ffprobe", err, stderr)
	}

	var out probeOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("media: decode ffprobe output: %w", err)
	}
	return parseProbeOutput(out), nil
}

// parseProbeOutput turns a decoded ffprobe document into streams. It is separate from the
// process run so the mapping can be tested against a real ffprobe payload.
func parseProbeOutput(out probeOutput) []Stream {
	streams := make([]Stream, 0, len(out.Streams))
	for _, s := range out.Streams {
		stream := Stream{
			Index:    s.Index,
			Type:     s.CodecType,
			Codec:    strings.ToLower(s.CodecName),
			Tags:     s.Tags,
			Language: NormalizeLanguage(tagValue(s.Tags, "language")),
			Title:    tagValue(s.Tags, "title"),
			Default:  s.Disposition["default"] != 0,
			Forced:   s.Disposition["forced"] != 0,
			Width:    derefInt(s.Width),
			Height:   derefInt(s.Height),
		}
		if s.Channels != nil {
			stream.Channels = *s.Channels
		}
		streams = append(streams, stream)
	}
	return streams
}

// RunFFmpeg executes ffmpeg with the given arguments.
func (f *FFmpeg) RunFFmpeg(ctx context.Context, args ...string) ([]byte, string, error) {
	stdout, stderr, err := f.run(ctx, f.cfg.FFmpeg.FFmpegPath, args...)
	if err != nil {
		return nil, string(stderr), f.toolError("ffmpeg", err, stderr)
	}
	return stdout, string(stderr), nil
}

// Binary reports the configured ffmpeg executable, for callers that build their own
// argument list (the HLS session manager).
func (f *FFmpeg) Binary() string { return f.cfg.FFmpeg.FFmpegPath }

// Run executes a configured binary directly, returning captured output. The HLS manager
// uses this because its process must outlive the request that started it.
func (f *FFmpeg) Run(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	stdout, stderr, err := f.run(ctx, binary, args...)
	return stdout, string(stderr), err
}

// run executes a command, capturing its output. nice is applied on POSIX hosts so a
// background conversion cannot starve playback of CPU.
func (f *FFmpeg) run(ctx context.Context, binary string, args ...string) (stdout, stderr []byte, err error) {
	timeout := f.cfg.FFmpeg.ProcessTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if shouldUseNice(f.cfg.FFmpeg.Nice) {
		// `-n` is required, not optional: BusyBox's nice (Alpine containers) treats a bare
		// number as the command name and fails with "can't execute '19'".
		wrapped := append([]string{"-n", strconv.Itoa(f.cfg.FFmpeg.Nice), binary}, args...)
		cmd = exec.CommandContext(runCtx, "nice", wrapped...)
	} else {
		cmd = exec.CommandContext(runCtx, binary, args...)
	}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// toolError converts an exec failure into a readable error and remembers whether the
// missing-binary case was already reported, so a scan without ffmpeg installed logs one
// warning instead of one per file.
func (f *FFmpeg) toolError(tool string, err error, stderr []byte) error {
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found") {
		f.mu.Lock()
		if !f.missingLogged {
			f.missingLogged = true
			f.mu.Unlock()
			return fmt.Errorf("media: %s is not installed or not on PATH (set %s_PATH)", tool, strings.ToUpper(tool))
		}
		f.mu.Unlock()
		return fmt.Errorf("media: %s unavailable", tool)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("media: %s timed out", tool)
	}
	detail := strings.TrimSpace(string(stderr))
	if len(detail) > 400 {
		detail = detail[:400]
	}
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("media: %s failed: %s", tool, detail)
}

// BrowserCanProgressivePlay reports whether a browser can play this container without a
// remux. Everything else needs an HLS session, or a native player that demuxes the file itself.
//
// This sits beside the codec tables because container legality and codec legality are one
// decision about one file, and the streaming handler must not re-derive either of them.
func BrowserCanProgressivePlay(path string) bool {
	switch ExtensionOf(path) {
	case "mp4", "m4v", "webm":
		return true
	default:
		return false
	}
}

// IsBrowserSafeAudioCodec reports whether a codec plays in a browser from its container.
func IsBrowserSafeAudioCodec(codec string) bool {
	codec = strings.ToLower(codec)
	switch codec {
	case "aac", "mp3", "opus", "vorbis":
		return true
	}
	return strings.HasPrefix(codec, "mp4a") || strings.HasPrefix(codec, "aac")
}

// IsMpegTsCopySafeCodec reports whether an audio codec may be copied into MPEG-TS
// segments. Only AAC and MP3 are legal there; anything else must be transcoded to AAC.
func IsMpegTsCopySafeCodec(codec string) bool {
	codec = strings.ToLower(codec)
	switch codec {
	case "aac", "mp3":
		return true
	}
	return strings.HasPrefix(codec, "mp4a") || strings.HasPrefix(codec, "aac")
}

// imageBasedSubtitleCodecs cannot become timed text. They are skipped rather than
// producing an empty VTT file.
var imageBasedSubtitleCodecs = map[string]bool{
	"hdmv_pgs_subtitle": true,
	"dvd_subtitle":      true,
	"xsub":              true,
	"dvb_subtitle":      true,
	"dvb_teletext":      true,
}

// IsUnsupportedSubtitleCodec reports whether ffmpeg can produce usable text from a codec.
func IsUnsupportedSubtitleCodec(codec string) bool {
	codec = strings.ToLower(codec)
	if codec == "ass" || codec == "ssa" {
		return false
	}
	if imageBasedSubtitleCodecs[codec] {
		return true
	}
	switch codec {
	case "subrip", "srt", "mov_text", "webvtt", "text":
		return false
	}
	return true
}

// NormalizeLanguage maps an ffprobe language tag to the display name both clients show.
func NormalizeLanguage(tag string) string {
	trimmed := strings.TrimSpace(strings.ToLower(tag))
	if trimmed == "" || trimmed == "und" {
		return "Unknown"
	}
	switch trimmed {
	case "eng", "en", "english":
		return "English"
	case "jpn", "jp", "japanese":
		return "Japanese"
	case "spa", "es", "spanish":
		return "Spanish"
	}
	if strings.Contains(trimmed, "english") {
		return "English"
	}
	if strings.Contains(trimmed, "japanese") {
		return "Japanese"
	}
	if strings.Contains(trimmed, "spanish") {
		return "Spanish"
	}
	return strings.ToUpper(tag)
}

func tagValue(tags map[string]string, key string) string {
	if tags == nil {
		return ""
	}
	if v, ok := tags[key]; ok {
		return v
	}
	// ffprobe case of tag names varies by container.
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// shouldUseNice reports whether to lower process priority. nice is a POSIX utility, so on
// Windows ffmpeg is invoked directly.
func shouldUseNice(level int) bool { return level != 0 && runtime.GOOS != "windows" }

// ExtensionOf returns a lowercase extension without the dot.
func ExtensionOf(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}
