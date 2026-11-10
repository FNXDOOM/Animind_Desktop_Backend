package downloads

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"animind-backend-go/internal/config"
)

// subtitleExts are the sidecar formats that travel inside packs: they ride the
// same selection rules as video so an episode and its subs land together.
var subtitleExts = []string{".srt", ".ass", ".ssa", ".vtt", ".sub"}

// videoExtensions mirrors config.ScannerConfig's VIDEO_EXTENSIONS default
// (lowercase, no dot): DownloadsConfig carries no env-tuned list, so the
// allowlist is pinned to the same defaults the scanner ships with.
var videoExtensions = []string{"mkv", "mp4", "avi", "webm", "m4v"}

var videoExtensionSet = extensionSet(videoExtensions)

// selectableExtensions is the full import allowlist: video plus subtitles.
var selectableExtensions = extensionSet(append(append([]string{}, videoExtensions...),
	strippedSubtitles()...))

func extensionSet(exts []string) map[string]bool {
	m := make(map[string]bool, len(exts))
	for _, ext := range exts {
		m[strings.ToLower(strings.TrimPrefix(ext, "."))] = true
	}
	return m
}

func strippedSubtitles() []string {
	out := make([]string, 0, len(subtitleExts))
	for _, ext := range subtitleExts {
		out = append(out, strings.TrimPrefix(ext, "."))
	}
	return out
}

// Limits from the brief: a torrent entry longer than these is hostile or
// unrepresentable on a real filesystem and must never touch disk.
const (
	maxSegmentRunes = 255
	maxPathRunes    = 1024
)

// errUnsafePath marks an entry that is not merely unselectable but hostile.
var errUnsafePath = errors.New("downloads: unsafe torrent path")

// windowsDeviceName reports whether base is a reserved DOS device name with or
// without an extension: writing "CON.mkv" wedges classic Windows file APIs.
func windowsDeviceName(base string) bool {
	stem := strings.SplitN(base, ".", 2)[0]
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// validateTorrentPath applies the hard rules to one torrent entry: device
// names and overlong segments error, whatever the extension.
func validateTorrentPath(path string) error {
	if utf8.RuneCountInString(path) > maxPathRunes {
		return fmt.Errorf("%w: path over %d runes", errUnsafePath, maxPathRunes)
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if utf8.RuneCountInString(seg) > maxSegmentRunes {
			return fmt.Errorf("%w: segment over %d runes", errUnsafePath, maxSegmentRunes)
		}
		if seg != "" && windowsDeviceName(seg) {
			return fmt.Errorf("%w: windows device name %q", errUnsafePath, seg)
		}
	}
	return nil
}

// escapesTorrentRoot reports whether a cleaned entry climbs out of the torrent
// root. Such entries are skipped, not fatal: a weird layout says nothing about
// the rest of the torrent.
func escapesTorrentRoot(path string) bool {
	cleaned := filepath.Clean(path)
	return filepath.IsAbs(cleaned) || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

// selectableExt reports whether an entry's extension may enter the library.
func selectableExt(path string) bool {
	return selectableExtensions[strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")]
}

// SelectableFiles returns the indexes worth importing: the allowlisted entries
// that stay inside the torrent root, keeping the largest MaxFiles by size and
// reporting the survivors in ascending index order. Device names and overlong
// paths error out — they must never touch disk — while traversal paths and
// unknown extensions are merely skipped. A non-positive MaxFiles means no cap.
func SelectableFiles(files []EngineFile, cfg config.DownloadsConfig) ([]int, error) {
	type candidate struct {
		index int
		size  int64
	}
	var cands []candidate
	for _, f := range files {
		if err := validateTorrentPath(f.Path); err != nil {
			return nil, err
		}
		if escapesTorrentRoot(f.Path) || !selectableExt(f.Path) {
			continue
		}
		cands = append(cands, candidate{index: f.Index, size: f.SizeBytes})
	}
	// largest first; stable so equal sizes keep the torrent's own order
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].size > cands[j].size })
	if cfg.MaxFiles > 0 && len(cands) > cfg.MaxFiles {
		cands = cands[:cfg.MaxFiles]
	}
	indexes := make([]int, 0, len(cands))
	for _, c := range cands {
		indexes = append(indexes, c.index)
	}
	sort.Ints(indexes)
	return indexes, nil
}
