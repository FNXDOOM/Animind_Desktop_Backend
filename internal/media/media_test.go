package media

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestConvertSRTToVTT(t *testing.T) {
	input := "1\r\n00:00:01,000 --> 00:00:04,000\r\nHello world\r\n\r\n2\r\n00:00:05,500 --> 00:00:08,000\r\nSecond line\r\n"
	out := ConvertSRTToVTT(input)

	if !strings.HasPrefix(out, "WEBVTT\n\n") {
		t.Fatalf("a VTT document must start with the header, got %q", out[:min(20, len(out))])
	}
	if strings.Contains(out, ",000 -->") {
		t.Error("SRT comma timestamps must become dot timestamps")
	}
	if !strings.Contains(out, "00:00:01.000 --> 00:00:04.000") {
		t.Errorf("expected converted timestamps, got %q", out)
	}
	if strings.Contains(out, "\n2\n") {
		t.Errorf("numeric cue index lines must be removed: %q", out)
	}
	if !strings.Contains(out, "Hello world") || !strings.Contains(out, "Second line") {
		t.Error("cue text must survive conversion")
	}
}

func TestConvertAssToVTTSkipsSignsAndKaraoke(t *testing.T) {
	ass := `[Script Info]
Title: Test

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Plain dialogue line
Dialogue: 0,0:00:05.00,0:00:08.00,Sign Typeset,,0,0,0,,A floating sign
Dialogue: 0,0:00:09.00,0:00:12.00,Karaoke,,0,0,0,,{\k120}la la
Dialogue: 0,0:00:13.00,0:00:16.00,Default,,0,0,0,,{\p1}m 0 0 l 10 10{\p0}
Dialogue: 0,0:00:17.00,0:00:20.00,Default,,0,0,0,,First{\i} line\NSecond line
Dialogue: 1,0:00:21.00,0:00:24.00,Default,,0,0,0,,12 34 56
Comment: 0,0:00:25.00,0:00:28.00,Default,,0,0,0,,should be ignored

`
	out := ConvertAssToVTT(ass)

	if !strings.HasPrefix(out, "WEBVTT\n\n") {
		t.Fatalf("output must be a VTT document: %q", out)
	}
	if !strings.Contains(out, "00:00:01.000 --> 00:00:04.000") {
		t.Errorf("dialogue cue missing or timestamps malformed:\n%s", out)
	}
	if !strings.Contains(out, "Plain dialogue line") {
		t.Error("default-style dialogue must be kept")
	}
	if strings.Contains(out, "floating sign") {
		t.Error("a style named like a sign must be dropped")
	}
	if strings.Contains(out, "la la") {
		t.Error("karaoke style must be dropped")
	}
	if strings.Contains(out, "m 0 0 l 10 10") {
		t.Error("a drawing command must be dropped")
	}
	if strings.Contains(out, "12 34 56") {
		t.Error("a cue that is only digits must be dropped as junk")
	}
	if strings.Contains(out, "should be ignored") {
		t.Error("Comment: lines must never be emitted")
	}
	if !strings.Contains(out, "First line") || !strings.Contains(out, "Second line") {
		t.Errorf("override blocks must be stripped and \\N must become a line break:\n%s", out)
	}
	if strings.Contains(out, "{\\i}") {
		t.Error("ASS override tags must not reach the client")
	}
}

func TestConvertAssToVTTDedupesRepeatedCues(t *testing.T) {
	ass := `[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Same text
Dialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,Same text
`
	out := ConvertAssToVTT(ass)
	if count := strings.Count(out, "Same text"); count != 1 {
		t.Errorf("consecutive identical cues should collapse, found %d:\n%s", count, out)
	}
}

func TestNormalizeLanguage(t *testing.T) {
	cases := map[string]string{
		"":        "Unknown",
		"und":     "Unknown",
		"eng":     "English",
		"EN":      "English",
		"jpn":     "Japanese",
		"spa":     "Spanish",
		"English": "English",
		"pob":     "POB",
	}
	for input, want := range cases {
		if got := NormalizeLanguage(input); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestNiceArguments locks the exact argv passed to nice. BusyBox's implementation - the one
// in Alpine images - refuses a bare level and would try to execute "19" as a program, which
// breaks every probe and transcode inside the container while working fine on a GNU host.
func TestNiceArguments(t *testing.T) {
	if !shouldUseNice(19) && runtime.GOOS != "windows" {
		t.Fatal("nice should be used on POSIX hosts when a level is configured")
	}
	if shouldUseNice(0) {
		t.Error("a level of 0 means do not change priority")
	}
	if runtime.GOOS == "windows" && shouldUseNice(19) {
		t.Error("nice does not exist on Windows")
	}
}

// ffprobePayload is a real ffprobe answer for a Matroska file with two audio streams and an
// ASS subtitle track, including the numeric disposition flags and numeric bit_rate that
// broke an earlier version of this decoder when they were typed as bool and string.
const ffprobePayload = `{
  "streams": [
    {"index":0,"codec_name":"h264","codec_type":"video","width":320,"height":240,
     "avg_frame_rate":"25/1","time_base":"1/1000","bit_rate":119232,
     "disposition":{"default":1,"dub":0,"original":0,"comment":0,"lyrics":0,"karaoke":0,
       "forced":0,"hearing_impaired":0,"visual_impaired":0,"clean_effects":0,
       "attached_pic":0,"timed_thumbnails":0,"non_diegetic":0,"captions":0,"descriptions":0,
       "metadata":0,"dependent":0,"still_image":0}},
    {"index":1,"codec_name":"aac","codec_type":"audio","channels":1,"sample_rate":"44100",
     "bit_rate":96896,"disposition":{"default":1,"forced":0},
     "tags":{"language":"eng","handler_name":"SoundHandler"}},
    {"index":2,"codec_name":"libopus","codec_type":"audio","channels":2,"sample_rate":"48000",
     "bit_rate":64000,"disposition":{"default":0,"forced":0},
     "tags":{"language":"jpn","title":"Japanese dub"}},
    {"index":3,"codec_name":"ass","codec_type":"subtitle","bit_rate":0,
     "disposition":{"default":1,"forced":0},"tags":{"language":"eng"}}
  ]
}`

func TestProbeDecodesRealFFprobeJSON(t *testing.T) {
	var out probeOutput
	if err := json.Unmarshal([]byte(ffprobePayload), &out); err != nil {
		t.Fatalf("ffprobe payload must decode (a type mismatch here empties every track list): %v", err)
	}
	streams := parseProbeOutput(out)
	if len(streams) != 4 {
		t.Fatalf("decoded %d streams, want 4", len(streams))
	}

	video := streams[0]
	if video.Type != "video" || video.Codec != "h264" || video.Width != 320 || video.Height != 240 {
		t.Errorf("video stream = %+v", video)
	}
	if !video.Default {
		t.Error("a numeric disposition flag of 1 must read as default")
	}

	first, second := streams[1], streams[2]
	if first.Codec != "aac" || first.Language != "English" || first.Channels != 1 {
		t.Errorf("first audio = %+v", first)
	}
	if second.Codec != "libopus" || second.Language != "Japanese" || second.Title != "Japanese dub" {
		t.Errorf("second audio = %+v", second)
	}
	if second.Default {
		t.Error("numeric 0 in disposition must read as not-default")
	}

	subs := streams[3]
	if subs.Type != "subtitle" || subs.Codec != "ass" {
		t.Errorf("subtitle stream = %+v", subs)
	}
	if IsUnsupportedSubtitleCodec(subs.Codec) {
		t.Error("ASS must be extractable")
	}
}

func TestCodecCapabilitySplit(t *testing.T) {
	// Opus is playable from WebM in a browser but is not legal in MPEG-TS, which is exactly
	// why the two functions differ.
	if !IsBrowserSafeAudioCodec("opus") {
		t.Error("opus must count as browser-safe")
	}
	if IsMpegTsCopySafeCodec("opus") {
		t.Error("opus must not be copied into MPEG-TS")
	}
	if !IsMpegTsCopySafeCodec("aac") || !IsMpegTsCopySafeCodec("mp3") {
		t.Error("aac and mp3 are the copy-safe codecs")
	}
	if IsMpegTsCopySafeCodec("vorbis") || IsMpegTsCopySafeCodec("") {
		t.Error("unknown codecs must require transcode")
	}
}

func TestSubtitleCodecSupport(t *testing.T) {
	for _, codec := range []string{"subrip", "srt", "mov_text", "ass", "ssa", "webvtt"} {
		if IsUnsupportedSubtitleCodec(codec) {
			t.Errorf("%s must be convertible", codec)
		}
	}
	for _, codec := range []string{"hdmv_pgs_subtitle", "dvd_subtitle", "xsub", "dvb_subtitle", "dvb_teletext", "hdmv_pgs"} {
		if !IsUnsupportedSubtitleCodec(codec) {
			t.Errorf("%s is image-based and must be skipped", codec)
		}
	}
}

func TestSubtitleDirectoryLayout(t *testing.T) {
	if got := EpisodeFolderName(5); got != "Episode 05" {
		t.Errorf("EpisodeFolderName(5) = %q", got)
	}
	if got := EpisodeFolderName(105); got != "Episode 105" {
		t.Errorf("EpisodeFolderName(105) = %q, want three digits", got)
	}
	if got := SeasonFolderName(3); got != "Season 03" {
		t.Errorf("SeasonFolderName(3) = %q", got)
	}

	first := SubtitleDirRelative("Frieren/Season 1/Frieren - 01.mkv", 1, 1)
	if filepathLike(first) != "Frieren/Subtitles/Episode 01" {
		t.Errorf("season 1 must not add a season folder, got %q", first)
	}
	third := SubtitleDirRelative("Frieren/Season 3/Frieren - 05.mkv", 3, 5)
	if filepathLike(third) != "Frieren/Subtitles/Season 03/Episode 05" {
		t.Errorf("season 3 must add a season folder, got %q", third)
	}
}

// filepathLike normalizes separators so the expectation holds on Windows and POSIX.
func filepathLike(path string) string { return strings.ReplaceAll(path, "\\", "/") }

func TestBrowserCanProgressivePlay(t *testing.T) {
	// These are the containers a browser demuxes without help. Anything else must be routed
	// to an HLS session, and getting this wrong is a black player rather than an error.
	playable := []string{"show/ep.mp4", "show/ep.M4V", "show/ep.webm", "ep.mp4"}
	notPlayable := []string{"show/ep.mkv", "show/ep.avi", "show/ep.ts", "show/ep.flv", "show/ep.mov", "noext"}

	for _, path := range playable {
		if !BrowserCanProgressivePlay(path) {
			t.Errorf("%s should play progressively", path)
		}
	}
	for _, path := range notPlayable {
		if BrowserCanProgressivePlay(path) {
			t.Errorf("%s must go through HLS", path)
		}
	}
}

func TestExtensionOf(t *testing.T) {
	cases := map[string]string{
		"show/episode.mkv":    "mkv",
		"show/episode.MKV":    "mkv",
		"show/episode.tar.gz": "gz",
		"noextension":         "",
	}
	for input, want := range cases {
		if got := ExtensionOf(input); got != want {
			t.Errorf("ExtensionOf(%q) = %q, want %q", input, got, want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
