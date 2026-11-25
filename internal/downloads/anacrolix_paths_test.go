package downloads

import (
	"path/filepath"
	"reflect"
	"testing"
)

// syntheticFile stands in for upstream *torrent.File, whose fields are unexported and whose
// BytesCompleted needs a live client; path shapes match upstream's initFiles.
type syntheticFile struct {
	path      string
	length    int64
	completed int64
}

func (f syntheticFile) Path() string          { return f.path }
func (f syntheticFile) Length() int64         { return f.length }
func (f syntheticFile) BytesCompleted() int64 { return f.completed }

// TestEngineFilesMapping runs every case through the handle's flow: files are sorted by path
// at metadata time, then mapped to EngineFiles with Index over the sorted order.
func TestEngineFilesMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []syntheticFile // the order upstream's Torrent.Files() reports
		want []EngineFile
	}{
		{
			name: "single-file torrent: path is the file's own name, no directory prefix",
			// Upstream builds File.path from info.BestName() alone for
			// single-file torrents, so Path() never carries a directory.
			in: []syntheticFile{
				{path: "Big Buck Bunny.mkv", length: 397312, completed: 1024},
			},
			want: []EngineFile{
				{Index: 0, Path: "Big Buck Bunny.mkv", SizeBytes: 397312, Progress: 1024},
			},
		},
		{
			name: "directory torrent: path is Name/rel/path with sizes and progress",
			in: []syntheticFile{
				{path: "Show.S01.1080p.WEB-DL.x264/Show - 01.mkv", length: 393216, completed: 262144},
				{path: "Show.S01.1080p.WEB-DL.x264/show.nfo", length: 4096, completed: 4096},
			},
			want: []EngineFile{
				{Index: 0, Path: filepath.FromSlash("Show.S01.1080p.WEB-DL.x264/Show - 01.mkv"), SizeBytes: 393216, Progress: 262144},
				{Index: 1, Path: filepath.FromSlash("Show.S01.1080p.WEB-DL.x264/show.nfo"), SizeBytes: 4096, Progress: 4096},
			},
		},
		{
			name: "paths are cleaned: redundant separators never leak",
			// Upstream joins with single '/', but a malformed metainfo can carry empty
			// elements; Clean is lexical defence and normalizes separators per platform.
			in: []syntheticFile{
				{path: "Release//Show - 01.mkv", length: 1},
				{path: "Release//show.nfo", length: 2},
			},
			want: []EngineFile{
				{Index: 0, Path: filepath.FromSlash("Release/Show - 01.mkv"), SizeBytes: 1},
				{Index: 1, Path: filepath.FromSlash("Release/show.nfo"), SizeBytes: 2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := tt.in
			sortFilesByPath(files)
			got := engineFiles(files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("engineFiles:\n got %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

// TestEngineFileOrderIsSortedByPathAndStable pins the index contract: files are ordered by
// path, duplicate paths keep upstream order, and Index follows the sorted list.
func TestEngineFileOrderIsSortedByPathAndStable(t *testing.T) {
	t.Parallel()
	in := []syntheticFile{
		{path: "Release/Show - 03.mkv", length: 3},
		{path: "Release/Show - 01.mkv", length: 1},
		{path: "Release/Show - 02.mkv", length: 2},
		{path: "Release/Show - 01.mkv", length: 100}, // malformed: duplicate path
	}
	sortFilesByPath(in)
	wantLengths := []int64{1, 100, 2, 3} // sorted by path; the duplicate pair must not swap
	for i, w := range wantLengths {
		if in[i].length != w {
			t.Fatalf("sorted position %d: got length %d, want %d (order %v)", i, in[i].length, w, in)
		}
	}
	got := engineFiles(in)
	for i, ef := range got {
		if ef.Index != i {
			t.Fatalf("EngineFile at position %d carries Index %d", i, ef.Index)
		}
	}
}
